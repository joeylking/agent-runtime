package approver_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/scripted"
)

type echo struct {
	spec  agentrt.ToolSpec
	calls []agentrt.ToolCall
}

func (e *echo) Spec() agentrt.ToolSpec { return e.spec }
func (e *echo) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	e.calls = append(e.calls, c)
	return agentrt.ToolResult{Content: c.Args, Summary: "echoed"}, nil
}

func tool(name string, se agentrt.SideEffect) *echo {
	return &echo{spec: agentrt.ToolSpec{Name: name, Description: name, SideEffect: se, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string"}},"additionalProperties":false}`)}}
}

// paused starts a run that pauses on its first step, a remote mutation
// the default policy sends to approval. now, when set, is the driver's
// clock; ttl its ApprovalTTL.
func paused(t *testing.T, store *agentrt.Store, args string, now func() time.Time, ttl time.Duration, tools ...agentrt.Tool) (*agentrt.Driver, agentrt.Run, agentrt.Approval) {
	t.Helper()
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", args, "publish"), scripted.Complete(`{"done":true}`)}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: tools, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	l := agentrt.DefaultLimits()
	l.ApprovalTTL = ttl
	run, err := d.Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", run.Status)
	}
	as, err := store.ListApprovals(context.Background(), run.ID)
	if err != nil || len(as) != 1 {
		t.Fatalf("approvals = %v, %v", as, err)
	}
	return d, run, as[0]
}

func open(t *testing.T) *agentrt.Store {
	t.Helper()
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLocal_ReturnsTypedErrors(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	push := tool("push", agentrt.RemoteMutation)
	d, run, a := paused(t, st, `{"n":7}`, nil, 0, push)
	ap := approver.New(st, nil)

	if _, err := ap.Pending(ctx, "nope", 5); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := ap.Show(ctx, run.ID, "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("unknown approval: %v", err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash}); !errors.Is(err, approver.ErrDecision) {
		t.Fatalf("no identity: %v", err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, By: "joey"}); !errors.Is(err, agentrt.ErrApprovalChanged) {
		t.Fatalf("no hash: %v", err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: strings.Repeat("0", 64), By: "joey"}); !errors.Is(err, agentrt.ErrApprovalChanged) {
		t.Fatalf("stale hash: %v", err)
	}
	if got, _ := st.GetApproval(ctx, run.ID, a.ID); got.Status != agentrt.ApprovalPending {
		t.Fatalf("refused decisions changed the approval: %s", got.Status)
	}
	if err := ap.Reject(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "joey", Note: "no"}); err != nil {
		t.Fatal(err)
	}
	// Decided, and the run cancelled: a second decision is both not
	// pending and not waiting; the runtime reports the run first.
	err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "joey"})
	if !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("decided twice: %v", err)
	}
	if err := ap.Cancel(ctx, run.ID, "joey", ""); !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("cancel finished run: %v", err)
	}
	if err := ap.Cancel(ctx, run.ID, "", ""); !errors.Is(err, approver.ErrDecision) {
		t.Fatalf("cancel without identity: %v", err)
	}
	if len(push.calls) != 0 {
		t.Fatal("tool ran")
	}

	// A grant that is spent: cancel after approve, then the approval is
	// no longer pending on a run no longer waiting.
	d2, run2, a2 := paused(t, st, `{"n":8}`, nil, 0, push)
	_ = d
	if err := ap.Approve(ctx, approver.Decision{RunID: run2.ID, ApprovalID: a2.ID, Hash: a2.Hash, By: "joey"}); err != nil {
		t.Fatal(err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run2.ID, ApprovalID: a2.ID, Hash: a2.Hash, By: "joey"}); !errors.Is(err, agentrt.ErrNotPending) {
		t.Fatalf("approve a grant: %v", err)
	}
	if err := ap.Cancel(ctx, run2.ID, "joey", "unwanted"); err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Resume(ctx, run2.ID); !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("resume cancelled: %v", err)
	}
}

func TestLocal_ExpiredWrapsNotPendingAndCancelsRun(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	push := tool("push", agentrt.RemoteMutation)
	// The driver's clock is in 2023, so a ten-minute TTL has long passed
	// by the wall clock the package-level decision reads.
	past := func() time.Time { return time.Unix(1700000000, 0) }
	_, run, a := paused(t, st, `{"n":1}`, past, 10*time.Minute, push)
	ap := approver.New(st, nil)
	p, err := ap.Show(ctx, run.ID, a.ID)
	if err != nil || !p.Expired || p.Status != agentrt.ApprovalPending {
		t.Fatalf("show = %+v, %v", p, err)
	}
	err = ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "joey"})
	if !errors.Is(err, approver.ErrExpired) || !errors.Is(err, agentrt.ErrNotPending) {
		t.Fatalf("expired: %v", err)
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.Status != agentrt.StatusCancelled || got.Reason != agentrt.ReasonApprovalExpired {
		t.Fatalf("run = %s %s", got.Status, got.Reason)
	}
	if ga, _ := st.GetApproval(ctx, run.ID, a.ID); ga.Status != agentrt.ApprovalExpired {
		t.Fatalf("approval = %s", ga.Status)
	}
	if len(push.calls) != 0 {
		t.Fatal("tool ran")
	}
}

func TestLocal_PendingIsBoundedAndOnlyForAWaitingRun(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	push := tool("push", agentrt.RemoteMutation)
	big := strings.Repeat("x", 5000)
	_, run, a := paused(t, st, `{"s":"`+big+`"}`, nil, 0, push)
	ap := approver.New(st, nil)
	list, err := ap.Pending(ctx, run.ID, 10)
	if err != nil || len(list) != 1 || list[0].ID != a.ID || list[0].Hash != a.Hash {
		t.Fatalf("pending = %+v, %v", list, err)
	}
	if strings.Contains(string(list[0].Request.Args), big) || !strings.Contains(string(list[0].Request.Args), "bytes omitted") {
		t.Fatalf("args not bounded: %d bytes", len(list[0].Request.Args))
	}
	if _, err := ap.Pending(ctx, run.ID, 0); err == nil {
		t.Fatal("limit 0 accepted")
	}
	p, err := ap.Show(ctx, run.ID, a.ID)
	if err != nil || strings.Contains(string(p.Request.Args), big) || p.Hash != a.Hash {
		t.Fatalf("show = %+v, %v", p, err)
	}
	if err := ap.Cancel(ctx, run.ID, "joey", "stop"); err != nil {
		t.Fatal(err)
	}
	// Cancel leaves the approval pending on a run that is no longer
	// waiting: nothing on it can be decided, so nothing is listed.
	list, err = ap.Pending(ctx, run.ID, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("pending after cancel = %+v, %v", list, err)
	}
}

func TestOpen_UsesOpenExisting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	if _, err := approver.Open(path, nil); err == nil {
		t.Fatal("opened a database that does not exist")
	}
	st, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	push := tool("push", agentrt.RemoteMutation)
	d, run, a := paused(t, st, `{"n":3}`, nil, 0, push)
	var events []agentrt.Event
	ap, err := approver.Open(path, func(e agentrt.Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "chat:joey", Note: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := ap.Close(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != agentrt.EventApprovalDecided {
		t.Fatalf("events = %+v", events)
	}
	got, err := d.Resume(ctx, run.ID)
	if err != nil || got.Status != agentrt.StatusCompleted {
		t.Fatalf("resume = %+v, %v", got, err)
	}
	if len(push.calls) != 1 || string(push.calls[0].Args) != `{"n":3}` {
		t.Fatalf("calls = %+v", push.calls)
	}
	if ga, _ := st.GetApproval(ctx, run.ID, a.ID); ga.DecidedBy != "chat:joey" || ga.Note != "ok" {
		t.Fatalf("approval = %+v", ga)
	}
}

// A Local whose Now is set judges expiry, and stamps decisions, by it: a
// channel's tests put an approval either side of its expiry without the
// wall clock. The expiry is the runtime's ErrApprovalExpired as well as
// ErrExpired.
func TestLocal_ClockJudgesExpiry(t *testing.T) {
	ctx := context.Background()
	start := time.Unix(1700000000, 0)
	at := func(d time.Duration) func() time.Time { return func() time.Time { return start.Add(d) } }
	st := open(t)
	_, run, a := paused(t, st, `{"n":1}`, at(0), 10*time.Minute, tool("push", agentrt.RemoteMutation))
	ap := approver.New(st, nil)
	ap.Now = at(5 * time.Minute)
	if p, err := ap.Show(ctx, run.ID, a.ID); err != nil || p.Expired {
		t.Fatalf("show within the TTL = %+v, %v", p, err)
	}
	if err := ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "joey"}); err != nil {
		t.Fatalf("approve within the TTL: %v", err)
	}
	if got, _ := st.GetApproval(ctx, run.ID, a.ID); !got.DecidedAt.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("decided at %s", got.DecidedAt)
	}

	st = open(t)
	_, run, a = paused(t, st, `{"n":1}`, at(0), 10*time.Minute, tool("push", agentrt.RemoteMutation))
	ap = approver.New(st, nil)
	ap.Now = at(11 * time.Minute)
	if ps, err := ap.Pending(ctx, run.ID, 5); err != nil || len(ps) != 1 || !ps[0].Expired {
		t.Fatalf("pending past the TTL = %+v, %v", ps, err)
	}
	err := ap.Reject(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "joey"})
	if !errors.Is(err, approver.ErrExpired) || !errors.Is(err, agentrt.ErrApprovalExpired) || !errors.Is(err, agentrt.ErrNotPending) {
		t.Fatalf("reject past the TTL: %v", err)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.Reason != agentrt.ReasonApprovalExpired || !got.FinishedAt.Equal(start.Add(11*time.Minute)) {
		t.Fatalf("run = %s at %s", got.Reason, got.FinishedAt)
	}
}

// Pending and Show carry the policy's reason for the approval, bounded
// for showing.
func TestLocal_PendingCarriesThePolicyReason(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	_, run, a := paused(t, st, `{"n":1}`, nil, 0, tool("push", agentrt.RemoteMutation))
	ap := approver.New(st, nil)
	const want = "side effect remote_mutation is require_approval by policy"
	ps, err := ap.Pending(ctx, run.ID, 5)
	if err != nil || len(ps) != 1 || ps[0].Reason != want {
		t.Fatalf("pending = %+v, %v", ps, err)
	}
	if p, err := ap.Show(ctx, run.ID, a.ID); err != nil || p.Reason != want {
		t.Fatalf("show = %+v, %v", p, err)
	}

	st = open(t)
	long := strings.Repeat("why ", 2000)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{}`, "")}}
	asks := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "k", Reason: long}, nil
	})
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Agent: agent, Policy: asks, Tools: []agentrt.Tool{tool("push", agentrt.RemoteMutation)}})
	if err != nil {
		t.Fatal(err)
	}
	run, _ = d.Start(ctx, "g", agentrt.DefaultLimits())
	ps, err = approver.New(st, nil).Pending(ctx, run.ID, 5)
	if err != nil || len(ps) != 1 || len(ps[0].Reason) >= len(long) || !strings.Contains(ps[0].Reason, "bytes omitted") {
		t.Fatalf("long reason = %d bytes, %v", len(ps[0].Reason), err)
	}
}

var _ approver.RunReader = (*approver.Local)(nil)

// A channel shows the run it is about to cancel through the Local it
// decides with, without opening the store: before the cancel and after
// it, and bounded as an approval is.
func TestLocal_RunReadsTheRunWithoutTheStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runs.db")
	st, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, run, _ := paused(t, st, `{"n":1}`, nil, 0, tool("push", agentrt.RemoteMutation))
	long := strings.Repeat("x", 5000)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.Complete(`{"s":"` + long + `"}`)}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Agent: agent, Policy: agentrt.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	done, err := d.Start(ctx, long, agentrt.DefaultLimits())
	if err != nil || done.Status != agentrt.StatusCompleted {
		t.Fatalf("start = %+v, %v", done, err)
	}

	ap, err := approver.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var rr approver.RunReader = ap
	got, err := rr.Run(ctx, run.ID)
	if err != nil || got.ID != run.ID || got.Status != agentrt.StatusWaitingForApproval || got.Goal != "g" {
		t.Fatalf("run before cancel = %+v, %v", got, err)
	}
	if err := ap.Cancel(ctx, run.ID, "joey", "unwanted"); err != nil {
		t.Fatal(err)
	}
	if got, err = rr.Run(ctx, run.ID); err != nil || got.Status != agentrt.StatusCancelled || got.Reason != agentrt.ReasonOperatorCancelled || got.FinishedAt.IsZero() {
		t.Fatalf("run after cancel = %+v, %v", got, err)
	}
	big, err := rr.Run(ctx, done.ID)
	if err != nil || len(big.Goal) >= len(long) || !strings.Contains(big.Goal, "bytes omitted") || len(big.Result) >= len(long) || !strings.Contains(string(big.Result), "bytes omitted") {
		t.Fatalf("bounded run: goal %d bytes, result %d bytes, %v", len(big.Goal), len(big.Result), err)
	}
	if _, err := rr.Run(ctx, "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}

// Open never creates or migrates: what a consumer sees for a missing
// file, a database the runtime never opened, and one at another schema
// version.
func TestOpen_RefusesADatabaseWithoutTheSchema(t *testing.T) {
	dir := t.TempDir()
	if _, err := approver.Open(filepath.Join(dir, "missing.db"), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	foreign := filepath.Join(dir, "foreign.db")
	older := filepath.Join(dir, "older.db")
	for path, ddl := range map[string]string{
		foreign: `CREATE TABLE notes (body TEXT)`,
		older:   `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY); INSERT INTO schema_migrations VALUES (1)`,
	} {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	if _, err := approver.Open(foreign, nil); err == nil || !strings.Contains(err.Error(), "not an agent-runtime database") {
		t.Fatalf("foreign: %v", err)
	}
	if _, err := approver.Open(older, nil); !errors.Is(err, agentrt.ErrSchemaVersion) {
		t.Fatalf("older: %v", err)
	}
}
