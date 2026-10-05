package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/proxy"
	"github.com/joeylking/agent-runtime/proxy/internal/fakeserver"
	"github.com/joeylking/agent-runtime/view"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This test binary is run again in two roles: as the fake payment server
// the proxy connects to, and as the proxy itself, so every test here
// drives real processes over real pipes.
const (
	envServe = "FAKE_PAYMENTS_SERVE"
	envProxy = "AGENTRT_PROXY_TEST_CHILD"
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(envServe) == "1":
		if err := fakeserver.Run(context.Background()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case os.Getenv(envProxy) == "1":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// setup writes a configuration for the fake bank under a fresh directory,
// with change applied to the JSON object first, and pins it with the pin
// command. pay needs an approval unless change says otherwise.
func setup(t *testing.T, change func(map[string]any)) (dir, config string) {
	t.Helper()
	dir = t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]any{
		"balance": map[string]any{"side_effect": "read_only", "timeout": "5s"},
		"pay":     map[string]any{"side_effect": "remote_mutation", "timeout": "20s"},
		"refund":  map[string]any{"side_effect": "destructive", "timeout": "5s"},
	}
	cfg := map[string]any{
		"database": "runs.db",
		"session":  "laptop",
		"hold":     "0s",
		"servers": []any{map[string]any{
			"name":     "bank",
			"command":  []string{self},
			"env":      []string{envServe + "=1", fakeserver.EnvLedger + "=" + filepath.Join(dir, "ledger")},
			"manifest": "bank.manifest.json",
			"rules":    rules,
		}},
	}
	if change != nil {
		change(cfg)
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	config = filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"pin", "-config", config}, nil, &out, &errb); code != 0 {
		t.Fatalf("pin: %d\n%s%s", code, out.String(), errb.String())
	}
	return dir, config
}

// allowPay lets payments run without an approval.
func allowPay(cfg map[string]any) {
	rules := cfg["servers"].([]any)[0].(map[string]any)["rules"].(map[string]any)
	rules["pay"].(map[string]any)["outcome"] = "allow"
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

// proc is one proxy process and the host connected to it over its stdin
// and stdout.
type proc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cs     *sdk.ClientSession
	stderr *syncBuffer
	done   chan struct{}
	err    error
}

func start(t *testing.T, config string) *proc {
	t.Helper()
	self, _ := os.Executable()
	p := &proc{t: t, stderr: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(self, "-config", config)
	p.cmd.Env = append(os.Environ(), envProxy+"=1")
	p.cmd.Stderr = p.stderr
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(p.kill)
	client := sdk.NewClient(&sdk.Implementation{Name: "host", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p.cs, err = client.Connect(ctx, &sdk.IOTransport{Reader: stdout, Writer: p.stdin}, nil)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, p.stderr.String())
	}
	return p
}

func (p *proc) call(name, args string) *sdk.CallToolResult {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		p.t.Fatalf("%s %s: %v\nstderr:\n%s", name, args, err, p.stderr.String())
	}
	return res
}

// stop closes the proxy's stdin, as a host does, and waits for it to exit.
func (p *proc) stop() {
	p.t.Helper()
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.t.Fatalf("the proxy did not exit after its stdin closed\n%s", p.stderr.String())
	}
	if p.err != nil {
		p.t.Fatalf("the proxy exited with %v\n%s", p.err, p.stderr.String())
	}
}

// crash kills the proxy as a power cut or kill -9 would.
func (p *proc) crash() {
	p.t.Helper()
	p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.done
}

func (p *proc) kill() {
	select {
	case <-p.done:
		return
	default:
	}
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func wantPrefix(t *testing.T, res *sdk.CallToolResult, prefix string) string {
	t.Helper()
	s := text(res)
	if !strings.HasPrefix(s, prefix+":") {
		t.Fatalf("got %q, want %s", s, prefix)
	}
	return s
}

// operator opens the database as agentrt does, beside the proxies.
func operator(t *testing.T, dir string) *agentrt.Store {
	t.Helper()
	store, err := agentrt.OpenExisting(filepath.Join(dir, "runs.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func pending(t *testing.T, store *agentrt.Store) []agentrt.Approval {
	t.Helper()
	ctx := context.Background()
	runs, err := store.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []agentrt.Approval
	for _, r := range runs {
		as, _ := store.ListApprovals(ctx, r.ID)
		for _, a := range as {
			if a.Status == agentrt.ApprovalPending {
				out = append(out, a)
			}
		}
	}
	return out
}

// decide approves or rejects bound to the hash read, as agentrt does.
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

// TestCLI_HelpStatesTheScope: the help text says what the proxy does not
// govern, without softening.
func TestCLI_HelpStatesTheScope(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"-h"}, nil, &out, &errb); code != 0 {
		t.Fatalf("-h exited %d", code)
	}
	help := strings.Join(strings.Fields(errb.String()), " ")
	for _, want := range []string{
		"It governs only the calls that go through it.",
		"can approve its own request with the operator command or by editing the database",
		"one person who is both the host's user and the operator. It does not separate them.",
		"no limits or policy over a conversation's history",
		"refuses a deliberate identical repeat",
		"The pin covers what a server presents, not what it does.",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if code := Run(context.Background(), nil, nil, &out, &errb); code != 2 {
		t.Errorf("no -config exited %d, want 2", code)
	}
}

// TestCLI_PinAndCheck: pin writes each manifest owner-only and prints the
// server's hints; check reports what registered and what is unclassified,
// and fails when a server registers nothing.
func TestCLI_PinAndCheck(t *testing.T) {
	dir, config := setup(t, func(cfg map[string]any) {
		rules := cfg["servers"].([]any)[0].(map[string]any)["rules"].(map[string]any)
		delete(rules, "refund")
	})
	info, err := os.Stat(filepath.Join(dir, "bank.manifest.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest: %v %v", info, err)
	}
	var out, errb bytes.Buffer
	Run(context.Background(), []string{"pin", "-config", config}, nil, &out, &errb)
	for _, want := range []string{"3 tools pinned", "balance", "readOnly", "refund", "claims nothing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("pin output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := Run(context.Background(), []string{"check", "-config", config}, nil, &out, &errb); code != 0 {
		t.Fatalf("check exited %d: %s", code, errb.String())
	}
	for _, want := range []string{"registered balance as bank_balance (read_only)", "registered pay as bank_pay (remote_mutation)", "unclassified refund", "status tool: gate_status"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("check output lacks %q:\n%s", want, out.String())
		}
	}
	_, empty := setup(t, func(cfg map[string]any) {
		cfg["servers"].([]any)[0].(map[string]any)["rules"] = map[string]any{}
	})
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), []string{"check", "-config", empty}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no tool registered") {
		t.Fatalf("check with nothing classified exited %d: %s", code, errb.String())
	}
}

// TestProcess_RestartCollectsAnApprovalGrantedWhileItWasGone: the approval
// is a row in the database, so a proxy started after the first one exited
// collects it and runs the payment once.
func TestProcess_RestartCollectsAnApprovalGrantedWhileItWasGone(t *testing.T) {
	dir, config := setup(t, nil)
	args := `{"to":"nina","amount":12}`
	first := start(t, config)
	wantPrefix(t, first.call("bank_pay", args), proxy.PrefixPending)
	first.stop()
	if !strings.Contains(first.stderr.String(), "approve laptop.") {
		t.Errorf("stderr gave the operator no command:\n%s", first.stderr.String())
	}
	store := operator(t, dir)
	p := pending(t, store)
	if len(p) != 1 {
		t.Fatalf("%d pending", len(p))
	}
	decide(t, store, p[0], true, "")
	second := start(t, config)
	if res := second.call("bank_pay", args); res.IsError || text(res) != "paid 12.00 to nina" {
		t.Fatalf("collected after a restart: %q", text(res))
	}
	if n := fakeserver.Count(filepath.Join(dir, "ledger")); n != 1 {
		t.Fatalf("%d payments", n)
	}
	second.stop()
}

// TestProcess_TwoProxiesOnOneSession: two proxies on one database and
// session run an identical call once between them, the other told it is
// in progress or a duplicate, and run different calls side by side.
func TestProcess_TwoProxiesOnOneSession(t *testing.T) {
	dir, config := setup(t, allowPay)
	ledger := filepath.Join(dir, "ledger")
	a, b := start(t, config), start(t, config)
	same := `{"to":"olga","amount":3,"mode":"slow"}`
	results := make([]string, 2)
	var wg sync.WaitGroup
	for i, p := range []*proc{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = text(p.call("bank_pay", same))
		}()
	}
	wg.Wait()
	slices.Sort(results)
	paid := 0
	for _, r := range results {
		switch {
		case r == "paid 3.00 to olga":
			paid++
		case strings.HasPrefix(r, proxy.PrefixInProgress+":"), strings.HasPrefix(r, proxy.PrefixDuplicate+":"):
		default:
			t.Errorf("unexpected answer %q", r)
		}
	}
	if paid != 1 || fakeserver.Count(ledger) != 1 {
		t.Fatalf("identical calls: %q, %d payments", results, fakeserver.Count(ledger))
	}
	for i, p := range []*proc{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = text(p.call("bank_pay", []string{`{"to":"pia","amount":1,"mode":"slow"}`, `{"to":"quin","amount":2,"mode":"slow"}`}[i]))
		}()
	}
	wg.Wait()
	if results[0] != "paid 1.00 to pia" || results[1] != "paid 2.00 to quin" || fakeserver.Count(ledger) != 3 {
		t.Fatalf("different calls: %q, %d payments", results, fakeserver.Count(ledger))
	}
	a.stop()
	b.stop()
}

// crashMidPayment starts a proxy whose payment blocks inside the server
// after the server recorded it, and kills the proxy there. It returns the
// run, read from the database, and the lease the dead proxy held.
// With approved set, the payment needs an approval, which an operator
// grants before the call that collects it is cut off.
func crashMidPayment(t *testing.T, args string, approved bool) (dir, config string, run agentrt.Run, until time.Time) {
	t.Helper()
	dir, config = setup(t, func(cfg map[string]any) {
		if !approved {
			allowPay(cfg)
		}
		cfg["lease_ttl"] = "3s"
	})
	victim := start(t, config)
	if approved {
		wantPrefix(t, victim.call("bank_pay", args), proxy.PrefixPending)
		store := operator(t, dir)
		decide(t, store, pending(t, store)[0], true, "")
	}
	go victim.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "bank_pay", Arguments: json.RawMessage(args)})
	ledger := filepath.Join(dir, "ledger")
	eventually(t, 30*time.Second, "the payment to reach the server", func() bool { return fakeserver.Count(ledger) == 1 })
	victim.crash()
	store := operator(t, dir)
	runs, err := store.ListRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].Status != agentrt.StatusRunning {
		t.Fatalf("after the crash: %+v %v", runs, err)
	}
	_, until, err = store.Lease(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return dir, config, runs[0], until
}

// interruptedAfterLease checks the restarted proxy calls the cut-off
// payment in progress while the dead proxy's lease may still be live, and
// interrupted once it has expired, with an interrupted_side_effect
// approval waiting.
func interruptedAfterLease(t *testing.T, p *proc, store *agentrt.Store, args string, until time.Time) agentrt.Approval {
	t.Helper()
	// Only asserted with a clear second of the lease left: a slow start
	// must not make this test fail.
	if time.Now().Add(time.Second).Before(until) {
		wantPrefix(t, p.call("bank_pay", args), proxy.PrefixInProgress)
	} else {
		t.Log("the lease ran out before the first call; its in-progress answer is not checked")
	}
	time.Sleep(time.Until(until) + 100*time.Millisecond)
	res := p.call("bank_pay", args)
	s := wantPrefix(t, res, proxy.PrefixInterrupted)
	if res.IsError {
		t.Errorf("an interruption is not an error result: %q", s)
	}
	waiting := pending(t, store)
	if len(waiting) != 1 || waiting[0].Kind != agentrt.InterruptedSideEffect || !strings.Contains(s, waiting[0].ID) {
		t.Fatalf("pending = %+v, answer %q", waiting, s)
	}
	return waiting[0]
}

// TestProcess_CrashThenApproveRunsItAgainOnce: a payment cut off by a
// crash is not run again on its own: until the lease expires the call is
// in progress, then it waits on an interrupted_side_effect approval,
// another payment is blocked while it does, and a read still works. Once
// an operator approves, the identical call runs it again, once.
func TestProcess_CrashThenApproveRunsItAgainOnce(t *testing.T) {
	args := `{"to":"rob","amount":5,"mode":"block"}`
	dir, config, run, until := crashMidPayment(t, args, false)
	ledger := filepath.Join(dir, "ledger")
	store := operator(t, dir)
	p := start(t, config)
	a := interruptedAfterLease(t, p, store, args, until)
	wantPrefix(t, p.call("bank_pay", `{"to":"sue","amount":1}`), proxy.PrefixBlocked)
	if res := p.call("bank_balance", `{"account":"A"}`); res.IsError {
		t.Fatalf("a read while a payment's outcome is unknown: %q", text(res))
	}
	if n := fakeserver.Count(ledger); n != 1 {
		t.Fatalf("%d payments before the operator decided", n)
	}
	// The approved re-run is the same request, so it blocks too unless
	// released.
	os.WriteFile(ledger+".release", nil, 0o600)
	decide(t, store, a, true, "checked the bank: it did not go through")
	if res := p.call("bank_pay", args); res.IsError || text(res) != "paid 5.00 to rob" {
		t.Fatalf("the approved re-run: %q", text(res))
	}
	if n := fakeserver.Count(ledger); n != 2 {
		t.Fatalf("%d payments, want the first attempt and the approved re-run", n)
	}
	got := eventTypesOf(t, store, run.ID)
	want := []string{"run.created", "run.started", "step.started", "step.decided", "step.policy", "step.tool_started",
		"lease.taken_over", "step.interrupted", "step.policy", "approval.requested", "approval.decided",
		"run.resumed", "step.policy", "step.tool_started", "step.tool_finished", "run.finished"}
	if !slices.Equal(got, want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
	wantPrefix(t, p.call("bank_pay", args), proxy.PrefixDuplicate)
	p.stop()
}

// TestProcess_CrashThenRejectRunsNothingAndUnblocks: rejected instead,
// the cut-off payment is not run, the identical call is refused as
// rejected, and other payments to the tool run again.
func TestProcess_CrashThenRejectRunsNothingAndUnblocks(t *testing.T) {
	args := `{"to":"tom","amount":6,"mode":"block"}`
	dir, config, _, until := crashMidPayment(t, args, false)
	ledger := filepath.Join(dir, "ledger")
	store := operator(t, dir)
	p := start(t, config)
	a := interruptedAfterLease(t, p, store, args, until)
	decide(t, store, a, false, "it went through; do not repeat it")
	s := wantPrefix(t, p.call("bank_pay", args), proxy.PrefixRejected)
	if !strings.Contains(s, "it went through") {
		t.Errorf("rejected = %q", s)
	}
	if res := p.call("bank_pay", `{"to":"uma","amount":1}`); res.IsError || text(res) != "paid 1.00 to uma" {
		t.Fatalf("another payment after the rejection: %q", text(res))
	}
	if n := fakeserver.Count(ledger); n != 2 {
		t.Fatalf("%d payments, want the cut-off one and uma's", n)
	}
	p.stop()
}

// TestProcess_CrashThenRejectAsksAgainAfterTheWindow: a rejected re-run of
// a payment cut off by a crash does not resolve its unknown outcome: after
// the window the identical call asks an operator again, under a policy
// that allows payments, and runs nothing.
func TestProcess_CrashThenRejectAsksAgainAfterTheWindow(t *testing.T) {
	args := `{"to":"vic","amount":7,"mode":"block"}`
	dir, config := setup(t, func(cfg map[string]any) {
		allowPay(cfg)
		cfg["lease_ttl"] = "3s"
		cfg["repeat_window"] = "2s"
	})
	ledger := filepath.Join(dir, "ledger")
	victim := start(t, config)
	go victim.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "bank_pay", Arguments: json.RawMessage(args)})
	eventually(t, 30*time.Second, "the payment to reach the server", func() bool { return fakeserver.Count(ledger) == 1 })
	victim.crash()
	store := operator(t, dir)
	runs, err := store.ListRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("after the crash: %+v %v", runs, err)
	}
	_, until, err := store.Lease(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	p := start(t, config)
	a := interruptedAfterLease(t, p, store, args, until)
	decide(t, store, a, false, "")
	time.Sleep(2500 * time.Millisecond)
	s := wantPrefix(t, p.call("bank_pay", args), proxy.PrefixInterrupted)
	again := pending(t, store)
	if len(again) != 1 || again[0].Kind != agentrt.InterruptedSideEffect || again[0].ID == a.ID || !strings.Contains(s, again[0].ID) {
		t.Fatalf("after the window: %q, pending %+v", s, again)
	}
	if n := fakeserver.Count(ledger); n != 1 {
		t.Fatalf("%d payments", n)
	}
	p.stop()
}

// TestProcess_CrashOfAnApprovedPaymentAsksAgain: a payment that needed an
// approval, cut off after it was granted, waits on an interruption
// approval of its own; granting that one runs it again once, through a
// policy that still requires approval and finds this grant the one it
// asks for.
func TestProcess_CrashOfAnApprovedPaymentAsksAgain(t *testing.T) {
	args := `{"to":"wes","amount":9,"mode":"block"}`
	dir, config, run, until := crashMidPayment(t, args, true)
	ledger := filepath.Join(dir, "ledger")
	store := operator(t, dir)
	p := start(t, config)
	a := interruptedAfterLease(t, p, store, args, until)
	var capability struct {
		Interrupted *struct {
			StepID string `json:"step_id"`
		} `json:"interrupted"`
	}
	if err := json.Unmarshal(a.Capability, &capability); err != nil || capability.Interrupted == nil || capability.Interrupted.StepID != a.StepID {
		t.Fatalf("the interruption's capability does not name the earlier attempt: %s", a.Capability)
	}
	os.WriteFile(ledger+".release", nil, 0o600)
	decide(t, store, a, true, "")
	if res := p.call("bank_pay", args); res.IsError || text(res) != "paid 9.00 to wes" {
		t.Fatalf("the approved re-run: %q", text(res))
	}
	if n := fakeserver.Count(ledger); n != 2 {
		t.Fatalf("%d payments, want the first attempt and the approved re-run", n)
	}
	if got, _ := store.GetRun(context.Background(), run.ID); got.Status != agentrt.StatusCompleted {
		t.Fatalf("run %s is %s", run.ID, got.Status)
	}
	p.stop()
}

// TestProcess_StdinCloseMidCallFinishesTheStep: a host that closes the
// proxy's stdin while a call runs does not interrupt it: the proxy waits
// for the call, records its outcome, and exits cleanly.
func TestProcess_StdinCloseMidCallFinishesTheStep(t *testing.T) {
	dir, config := setup(t, allowPay)
	ledger := filepath.Join(dir, "ledger")
	p := start(t, config)
	go p.cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "bank_pay", Arguments: json.RawMessage(`{"to":"val","amount":8,"mode":"slow"}`)})
	eventually(t, 30*time.Second, "the payment to reach the server", func() bool { return fakeserver.Count(ledger) == 1 })
	p.stop()
	store := operator(t, dir)
	runs, _ := store.ListRuns(context.Background())
	if len(runs) != 1 || runs[0].Status != agentrt.StatusCompleted {
		t.Fatalf("runs after stdin closed: %+v", runs)
	}
	if got := eventTypesOf(t, store, runs[0].ID); slices.Contains(got, agentrt.EventStepInterrupted) || got[len(got)-1] != agentrt.EventRunFinished {
		t.Fatalf("events: %v", got)
	}
}

func eventTypesOf(t *testing.T, store *agentrt.Store, runID string) []string {
	t.Helper()
	events, err := store.ListEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}
