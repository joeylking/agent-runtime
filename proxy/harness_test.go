package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/mcp"
	"github.com/joeylking/agent-runtime/proxy/internal/fakeserver"
	"github.com/joeylking/agent-runtime/view"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The upstream is this test binary run again as the fake payment server:
// the mcp package takes a real command and nothing else from outside it.
const envServe = "FAKE_PAYMENTS_SERVE"

func TestMain(m *testing.M) {
	if os.Getenv(envServe) == "1" {
		if err := fakeserver.Run(context.Background()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// rule is a ToolRule from its file fields.
func rule(class agentrt.SideEffect) ToolRule {
	return ToolRule{FileRule: mcp.FileRule{SideEffect: class, Timeout: "10s"}}
}

// testConfig serves the fake bank: balance a read, pay a remote mutation,
// which DefaultPolicy sends to an operator, and refund destructive, which
// it denies. Nothing is held and the paths are under dir.
func testConfig(t *testing.T, dir string) *Config {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Database: filepath.Join(dir, "runs.db"),
		Session:  "test",
		Servers: []ServerConfig{{
			Name:     "bank",
			Command:  []string{self},
			Env:      []string{envServe + "=1", fakeserver.EnvLedger + "=" + filepath.Join(dir, "ledger")},
			Manifest: filepath.Join(dir, "bank.manifest.json"),
			Rules: map[string]ToolRule{
				"balance": rule(agentrt.ReadOnly),
				"pay":     rule(agentrt.RemoteMutation),
				"refund":  rule(agentrt.Destructive),
			},
		}},
		Hold: &Duration{0},
	}
	return cfg
}

// pinServers writes every server's manifest, as `agentrt-proxy pin` does.
func pinServers(t *testing.T, cfg *Config) {
	t.Helper()
	for _, sc := range cfg.Servers {
		m, err := mcp.Pin(context.Background(), sc.Server())
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(m)
		if err := os.WriteFile(sc.Manifest, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// skewClock is the wall clock moved by an offset a test sets, so expiries
// judged by the proxy's clock can pass without waiting, while the store's
// operator path stamps the wall clock.
type skewClock struct{ skew atomic.Int64 }

func (c *skewClock) now() time.Time      { return time.Now().Add(time.Duration(c.skew.Load())) }
func (c *skewClock) add(d time.Duration) { c.skew.Add(int64(d)) }

// harness is one proxy serving one in-memory host.
type harness struct {
	t      *testing.T
	dir    string
	cfg    *Config
	p      *Proxy
	cs     *sdk.ClientSession
	clock  *skewClock
	ledger string
	logs   *syncBuffer
	served chan error
	cancel context.CancelFunc

	mu       sync.Mutex
	seen     []string
	progress atomic.Int32
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newHarness pins and serves cfg, which change may adjust first.
func newHarness(t *testing.T, change func(*Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	if change != nil {
		change(cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	pinServers(t, cfg)
	return serveHarness(t, dir, cfg)
}

func serveHarness(t *testing.T, dir string, cfg *Config) *harness {
	t.Helper()
	h := &harness{t: t, dir: dir, cfg: cfg, clock: &skewClock{}, ledger: filepath.Join(dir, "ledger"), logs: &syncBuffer{}}
	p, _, err := open(context.Background(), cfg, options{log: h.logs, now: h.clock.now, poll: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	st, ct := sdk.NewInMemoryTransports()
	h.served = make(chan error, 1)
	go func() { h.served <- p.Serve(ctx, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "host", Version: "1"}, &sdk.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *sdk.ProgressNotificationClientRequest) { h.progress.Add(1) },
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cs = cs
	t.Cleanup(h.close)
	return h
}

func (h *harness) close() {
	if h.cs == nil {
		return
	}
	h.cs.Close()
	select {
	case <-h.served:
	case <-time.After(30 * time.Second):
		h.t.Error("Serve did not return after the host closed")
	}
	h.cancel()
	h.p.Close()
	h.cs = nil
}

// call makes one tools/call and keeps its text.
func (h *harness) call(name, args string) *sdk.CallToolResult {
	h.t.Helper()
	return h.callCtx(context.Background(), name, args, nil)
}

func (h *harness) callCtx(ctx context.Context, name, args string, token any) *sdk.CallToolResult {
	h.t.Helper()
	params := &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(args)}
	if token != nil {
		params.SetProgressToken(token)
	}
	res, err := h.cs.CallTool(ctx, params)
	if err != nil {
		h.t.Fatalf("%s %s: %v", name, args, err)
	}
	h.keep(res)
	return res
}

func (h *harness) keep(res *sdk.CallToolResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, resultString(res))
}

func resultString(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// wantPrefix fails unless the result's text starts with prefix and its
// isError is as the outcome's.
func wantPrefix(t *testing.T, res *sdk.CallToolResult, prefix string, isError bool) string {
	t.Helper()
	s := resultString(res)
	if !strings.HasPrefix(s, prefix+":") || res.IsError != isError {
		t.Fatalf("got isError=%v %q, want %s (isError=%v)", res.IsError, s, prefix, isError)
	}
	return s
}

func (h *harness) payments() int { return fakeserver.Count(h.ledger) }

// pending is every approval the store has waiting, oldest first.
func (h *harness) pending() []agentrt.Approval {
	h.t.Helper()
	return pendingIn(h.t, h.p.store)
}

func pendingIn(t *testing.T, store *agentrt.Store) []agentrt.Approval {
	t.Helper()
	ctx := context.Background()
	runs, err := store.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []agentrt.Approval
	for i := len(runs) - 1; i >= 0; i-- {
		as, err := store.ListApprovals(ctx, runs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Status == agentrt.ApprovalPending {
				out = append(out, a)
			}
		}
	}
	return out
}

func (h *harness) approvals() int {
	h.t.Helper()
	ctx := context.Background()
	runs, _ := h.p.store.ListRuns(ctx)
	n := 0
	for _, r := range runs {
		as, _ := h.p.store.ListApprovals(ctx, r.ID)
		n += len(as)
	}
	return n
}

// decide approves or rejects an approval the way cmd/agentrt does: bound
// to the hash of the approval it read.
func decide(t *testing.T, store *agentrt.Store, a agentrt.Approval, approve bool, note string) {
	t.Helper()
	ctx := context.Background()
	cur, err := store.GetApproval(ctx, a.RunID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		err = view.ApproveShown(ctx, store, nil, a.RunID, a.ID, cur.Hash, "operator", note)
	} else {
		err = view.RejectShown(ctx, store, nil, a.RunID, a.ID, cur.Hash, "operator", note)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// onlyPending is the one approval waiting.
func (h *harness) onlyPending() agentrt.Approval {
	h.t.Helper()
	p := h.pending()
	if len(p) != 1 {
		h.t.Fatalf("%d approvals pending, want 1", len(p))
	}
	return p[0]
}

// runs is every run in the store, oldest first.
func (h *harness) runs() []agentrt.Run {
	h.t.Helper()
	runs, err := h.p.store.ListRuns(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]agentrt.Run, len(runs))
	for i := range runs {
		out[len(runs)-1-i] = runs[i]
	}
	return out
}

// settledOrWaiting fails for any run that is not terminal, waiting on an
// approval, or interrupted: every call's run must end.
func settledOrWaiting(t *testing.T, store *agentrt.Store) {
	t.Helper()
	runs, err := store.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if !r.Status.Terminal() && r.Status != agentrt.StatusWaitingForApproval {
			t.Errorf("run %s is %s: every run must end, wait on an approval, or be interrupted", r.ID, r.Status)
		}
	}
}

func eventTypes(t *testing.T, store *agentrt.Store, runID string) []string {
	t.Helper()
	events, err := store.ListEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// eventually polls cond for up to d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
