package agentrt_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// Lease reads the stored lease: held while a tool runs, released when the
// run pauses, and ErrNotFound for no run.
func TestStore_LeaseReadsTheStoredLease(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx := context.Background()
	var owner string
	var until time.Time
	var lerr error
	read := newTool("read", agentrt.ReadOnly, numberSchema, func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
		owner, until, lerr = h.store.Lease(ctx, c.RunID)
		return agentrt.ToolResult{Content: []byte(`{}`)}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("push", `{"n":2}`, ""), scripted.Complete(`{}`)}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{read, echoTool("push", agentrt.RemoteMutation)}, LeaseOwner: "worker-7"})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	run, err := d.Start(ctx, "g", limits(10, 3))
	if err != nil || run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("run = %s, %v", run.Status, err)
	}
	if lerr != nil || !strings.HasPrefix(owner, "worker-7/") || !until.After(before) {
		t.Fatalf("lease during the tool = %q %s, %v", owner, until, lerr)
	}
	if owner, until, err := h.store.Lease(ctx, run.ID); err != nil || owner != "" || !until.IsZero() {
		t.Fatalf("lease while waiting = %q %s, %v", owner, until, err)
	}
	if _, _, err := h.store.Lease(ctx, "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("no run: %v", err)
	}
}

// ListEventsAfter is every run's events after a Seq in commit order, or
// one run's, a bounded page at a time with payloads capped.
func TestStore_ListEventsAfterFollowsSeqAcrossRuns(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx := context.Background()
	read := echoTool("read", agentrt.ReadOnly)
	var ids []string
	for range 2 {
		agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.Complete(`{}`)}}
		run, err := h.driver(agent, agentrt.DefaultPolicy(), read).Start(ctx, "g", limits(10, 3))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	var all []agentrt.Event
	for _, id := range ids {
		evs, err := h.store.ListEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	seqs := func(evs []agentrt.Event) string {
		var b strings.Builder
		for _, e := range evs {
			fmt.Fprintf(&b, "%d:%s ", e.Seq, e.RunID[:4])
		}
		return b.String()
	}
	got, err := h.store.ListEventsAfter(ctx, "", 0, 1000)
	if err != nil || seqs(got) != seqs(all) {
		t.Fatalf("every run = %s, %v; want %s", seqs(got), err, seqs(all))
	}
	k := all[2].Seq
	if got, _ := h.store.ListEventsAfter(ctx, "", k, 3); seqs(got) != seqs(all[3:6]) {
		t.Fatalf("after %d, 3 = %s; want %s", k, seqs(got), seqs(all[3:6]))
	}
	n := len(all) / 2
	if got, _ := h.store.ListEventsAfter(ctx, ids[1], 0, 1000); seqs(got) != seqs(all[n:]) {
		t.Fatalf("one run = %s; want %s", seqs(got), seqs(all[n:]))
	}
	if got, err := h.store.ListEventsAfter(ctx, "", all[len(all)-1].Seq, 10); err != nil || len(got) != 0 {
		t.Fatalf("after the last = %v, %v", got, err)
	}
	if _, err := h.store.ListEventsAfter(ctx, "", 0, 0); err == nil {
		t.Fatal("a zero limit was accepted")
	}
	big := `{"s":"` + strings.Repeat("x", agentrt.MaxPageText) + `"}`
	if _, err := h.store.DB().Exec(`INSERT INTO events (run_id, step_id, at, type, payload_json) VALUES (?, '', 'not a time', 'x', ?)`, ids[0], big); err != nil {
		t.Fatal(err)
	}
	got, err = h.store.ListEventsAfter(ctx, ids[0], all[len(all)-1].Seq, 10)
	if err != nil || len(got) != 1 || !got[0].At.IsZero() || len(got[0].Payload) > agentrt.MaxPageText+100 || !json.Valid(got[0].Payload) {
		t.Fatalf("capped = %d bytes, %v", len(got[0].Payload), err)
	}
}

// A decision that finds its approval expired returns ErrApprovalExpired,
// which is still ErrNotPending and still names the expiry; a decided
// approval is not pending but not expired.
func TestApproval_ExpiryIsErrApprovalExpired(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx := context.Background()
	c := &clock{now: time.Unix(1700000000, 0)}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	l := limits(10, 3)
	l.ApprovalTTL = 10 * time.Minute
	d := timedDriver(t, h, c, agent, echoTool("push", agentrt.RemoteMutation))
	run, _ := d.Start(ctx, "g", l)
	as, _ := h.store.ListApprovals(ctx, run.ID)
	c.advance(11 * time.Minute)
	err := d.Approve(ctx, run.ID, as[0].ID, "joey", "")
	if !errors.Is(err, agentrt.ErrApprovalExpired) || !errors.Is(err, agentrt.ErrNotPending) || !errors.Is(agentrt.ErrApprovalExpired, agentrt.ErrNotPending) {
		t.Fatalf("expired: %v", err)
	}
	if want := "agentrt: approval is not pending: approval " + as[0].ID + " expired at " + as[0].ExpiresAt.Format(time.RFC3339); err.Error() != want {
		t.Fatalf("message = %q, want %q", err, want)
	}

	h2 := newHarness(t, ":memory:")
	d2, run2, a2 := pausedRun(t, h2, echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly))
	if err := d2.Approve(ctx, run2.ID, a2.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	err = d2.Approve(ctx, run2.ID, a2.ID, "joey", "")
	if !errors.Is(err, agentrt.ErrNotPending) || errors.Is(err, agentrt.ErrApprovalExpired) {
		t.Fatalf("decided: %v", err)
	}
}

// An Operator judges expiry, and stamps its decisions, by its own clock;
// the zero Operator and the package-level functions use the wall clock.
func TestOperator_ClockDecidesExpiry(t *testing.T) {
	ctx := context.Background()
	start := time.Unix(1700000000, 0)
	pause := func() (*harness, agentrt.Run, agentrt.Approval) {
		h := newHarness(t, ":memory:")
		agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
		l := limits(10, 3)
		l.ApprovalTTL = 10 * time.Minute
		run, err := timedDriver(t, h, &clock{now: start}, agent, echoTool("push", agentrt.RemoteMutation)).Start(ctx, "g", l)
		if err != nil {
			t.Fatal(err)
		}
		as, _ := h.store.ListApprovals(ctx, run.ID)
		return h, run, as[0]
	}
	at := func(d time.Duration) func() time.Time { return func() time.Time { return start.Add(d) } }

	h, run, a := pause()
	if err := (agentrt.Operator{Store: h.store, Now: at(5 * time.Minute)}).ApproveShown(ctx, run.ID, a.ID, a.Hash, "joey", ""); err != nil {
		t.Fatalf("within the TTL: %v", err)
	}
	if got, _ := h.store.GetApproval(ctx, run.ID, a.ID); !got.DecidedAt.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("decided at %s", got.DecidedAt)
	}
	if err := (agentrt.Operator{Store: h.store, Now: at(6 * time.Minute)}).Cancel(ctx, run.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.GetRun(ctx, run.ID); !got.FinishedAt.Equal(start.Add(6 * time.Minute)) {
		t.Fatalf("cancelled at %s", got.FinishedAt)
	}

	var events []agentrt.Event
	h, run, a = pause()
	op := agentrt.Operator{Store: h.store, Observer: func(e agentrt.Event) { events = append(events, e) }, Now: at(11 * time.Minute)}
	if err := op.RejectShown(ctx, run.ID, a.ID, a.Hash, "joey", ""); !errors.Is(err, agentrt.ErrApprovalExpired) {
		t.Fatalf("past the TTL: %v", err)
	}
	if got, _ := h.store.GetRun(ctx, run.ID); got.Reason != agentrt.ReasonApprovalExpired || !got.FinishedAt.Equal(start.Add(11*time.Minute)) || len(events) != 2 {
		t.Fatalf("run = %s at %s, %d events", got.Reason, got.FinishedAt, len(events))
	}
	if err := op.ApproveShown(ctx, run.ID, a.ID, "", "joey", ""); !errors.Is(err, agentrt.ErrApprovalChanged) {
		t.Fatalf("no hash: %v", err)
	}

	for name, decide := range map[string]func(*agentrt.Store, agentrt.Run, agentrt.Approval) error{
		"zero Now": func(s *agentrt.Store, r agentrt.Run, a agentrt.Approval) error {
			return agentrt.Operator{Store: s}.Approve(ctx, r.ID, a.ID, "joey", "")
		},
		"package-level": func(s *agentrt.Store, r agentrt.Run, a agentrt.Approval) error {
			return agentrt.Reject(ctx, s, nil, r.ID, a.ID, "joey", "")
		},
	} {
		h, run, a := pause()
		if err := decide(h.store, run, a); !errors.Is(err, agentrt.ErrApprovalExpired) {
			t.Fatalf("%s: a 2023 approval by the wall clock: %v", name, err)
		}
	}
}

// ApprovalReason is the reason of the policy decision that paused the run
// on the approval, also after the resume-time policy decided otherwise.
func TestStore_ApprovalReasonIsThePausingPolicys(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx := context.Background()
	asked := false
	policy := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		if !asked {
			asked = true
			return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "publication", Reason: "publishing needs a human"}, nil
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "granted"}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	d := h.driver(agent, policy, echoTool("push", agentrt.RemoteMutation))
	run, _ := d.Start(ctx, "g", limits(10, 3))
	as, _ := h.store.ListApprovals(ctx, run.ID)
	if r, err := h.store.ApprovalReason(ctx, run.ID, as[0].ID); err != nil || r != "publishing needs a human" {
		t.Fatalf("pending = %q, %v", r, err)
	}
	if err := d.Approve(ctx, run.ID, as[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Resume(ctx, run.ID); err != nil || got.Status != agentrt.StatusCompleted {
		t.Fatalf("resume = %s, %v", got.Status, err)
	}
	steps, _ := h.store.ListSteps(ctx, run.ID)
	if steps[0].Policy.Reason != "granted" {
		t.Fatalf("step policy = %+v", steps[0].Policy)
	}
	if r, err := h.store.ApprovalReason(ctx, run.ID, as[0].ID); err != nil || r != "publishing needs a human" {
		t.Fatalf("after resume = %q, %v", r, err)
	}
	if _, err := h.store.ApprovalReason(ctx, run.ID, "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("no approval: %v", err)
	}
	if _, err := h.store.ApprovalReason(ctx, "nope", as[0].ID); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("another run's approval: %v", err)
	}
}

// CheckArgs refuses exactly what the driver records as an invalid
// decision, with the same text, and a spec NewDriver refuses with
// NewDriver's error.
func TestCheckArgs_IsTheDriversAcceptance(t *testing.T) {
	ctx := context.Background()
	schema := `{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string"}},"additionalProperties":false}`
	cases := []string{
		``, `  `, `{}`, `{"n":1}`, `{"n":"x"}`, `{"m":1}`, `[]`, `{`, `{"n":1,"n":2}`,
		"{\"s\":\"\xff\"}", `{"s":"\ud800"}`, `{"s":"😀"}`,
		strings.Repeat(`{"s":`, 70) + `1` + strings.Repeat(`}`, 70),
	}
	h := newHarness(t, ":memory:")
	tool := newTool("t", agentrt.LocalMutation, schema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{}`)}, nil
	})
	var decisions []agentrt.Decision
	for _, c := range cases {
		decisions = append(decisions, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "t", Args: json.RawMessage(c)})
	}
	n := len(cases) + 1
	run, err := h.driver(&scripted.Agent{Decisions: append(decisions, scripted.Complete(`{}`))}, agentrt.DefaultPolicy(), tool).
		Start(ctx, "g", agentrt.Limits{MaxSteps: n, MaxConsecutiveToolFailures: n, LoopThreshold: n})
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := h.store.ListSteps(ctx, run.ID)
	ts, err := agentrt.CompileTool(tool.Spec())
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for i, c := range cases {
		want := ""
		if o := steps[i].Observation; o.Kind == agentrt.ObserveInvalidDecision {
			var e struct{ Error string }
			json.Unmarshal(o.Content, &e)
			want = e.Error
			refused++
		}
		for name, err := range map[string]error{"CheckArgs": agentrt.CheckArgs(tool.Spec(), json.RawMessage(c)), "Check": ts.Check(json.RawMessage(c))} {
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != want {
				t.Errorf("%s(%q) = %q, driver recorded %q", name, c, got, want)
			}
		}
	}
	if refused != 8 {
		t.Errorf("the driver refused %d cases, want 8", refused)
	}

	for _, spec := range []agentrt.ToolSpec{
		{Name: "", SideEffect: agentrt.ReadOnly, Timeout: time.Second, InputSchema: []byte(schema)},
		{Name: "x", SideEffect: "loud", Timeout: time.Second, InputSchema: []byte(schema)},
		{Name: "x", SideEffect: agentrt.ReadOnly, InputSchema: []byte(schema)},
		{Name: "x", SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		{Name: "x", SideEffect: agentrt.ReadOnly, Timeout: time.Second, InputSchema: []byte(`{"$ref":"file:///etc/passwd"}`)},
	} {
		_, derr := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: &scripted.Agent{}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{&fakeTool{spec: spec}}})
		_, cerr := agentrt.CompileTool(spec)
		aerr := agentrt.CheckArgs(spec, nil)
		if derr == nil || cerr == nil || aerr == nil || cerr.Error() != derr.Error() || aerr.Error() != derr.Error() {
			t.Errorf("spec %+v: NewDriver %v, CompileTool %v, CheckArgs %v", spec, derr, cerr, aerr)
		}
	}
}

// CheckArgs refuses a number past the bound with the boundary's reason,
// against every keyword whose validation panicked on one.
func TestCheckArgs_RefusesNumbersPastTheBound(t *testing.T) {
	for _, keyword := range []string{`"minimum":0`, `"maximum":0`, `"exclusiveMinimum":0`, `"exclusiveMaximum":0`, `"multipleOf":0.5`} {
		spec := agentrt.ToolSpec{Name: "t", SideEffect: agentrt.RemoteMutation, Timeout: time.Second,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"number",` + keyword + `},"l":{"type":"array","uniqueItems":true}}}`)}
		for _, args := range []string{`{"n":1e-10000000}`, `{"n":1e-1000000000}`, `{"n":0.1e-9223372036854775808}`, `{"n":-1E-1000001}`,
			`{"l":[1e-10000000` + strings.Repeat(`,1`, 30) + `]}`} {
			err := agentrt.CheckArgs(spec, json.RawMessage(args))
			if err == nil || !strings.HasPrefix(err.Error(), "arguments are not usable JSON: number ") {
				t.Errorf("%s %.40s: %v", keyword, args, err)
			}
		}
		if err := agentrt.CheckArgs(spec, json.RawMessage(`{"n":1e-10000}`)); err != nil && !strings.Contains(err.Error(), "do not match schema") {
			t.Errorf("%s: a number at the bound: %v", keyword, err)
		}
	}
}
