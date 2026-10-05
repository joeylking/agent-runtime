package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// twin is a store with a deterministic clock and ids and a record of the
// events committed, so a run through a Driver and the same run through a
// Gate can be compared row for row and reading for reading.
type twin struct {
	store  *Store
	at     time.Time
	reads  int
	ids    int
	events []Event
}

func newTwin(t *testing.T) *twin {
	return &twin{store: memStore(t), at: time.Unix(1700000000, 0)}
}

func (w *twin) now() time.Time  { w.reads++; w.at = w.at.Add(time.Second); return w.at }
func (w *twin) newID() string   { w.ids++; return fmt.Sprintf("id-%03d", w.ids) }
func (w *twin) observe(e Event) { w.events = append(w.events, e) }

func (w *twin) gate(t *testing.T, tools []Tool, policy Policy) *Gate {
	t.Helper()
	g, err := NewGate(GateConfig{Store: w.store, Policy: policy, Tools: tools, Observer: w.observe, Now: w.now, NewID: w.newID})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// countedTool echoes its arguments and counts its calls, keeping the last
// arguments it was given.
type countedTool struct {
	spec  ToolSpec
	calls atomic.Int32
	args  atomic.Value
}

func (c *countedTool) Spec() ToolSpec { return c.spec }
func (c *countedTool) Call(_ context.Context, call ToolCall) (ToolResult, error) {
	c.calls.Add(1)
	c.args.Store(string(call.Args))
	return ToolResult{Content: call.Args, Summary: "echoed " + c.spec.Name}, nil
}

func counted(name string, se SideEffect) *countedTool {
	return &countedTool{spec: ToolSpec{Name: name, Description: name, SideEffect: se, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`)}}
}

func gateTools() []Tool {
	finish := counted("finish", LocalMutation)
	finish.spec.Terminal = true
	return []Tool{counted("read", ReadOnly), counted("push", RemoteMutation), counted("wipe", Destructive), finish}
}

func call(tool, args string) Decision {
	return Decision{Kind: DecideToolCall, Tool: tool, Args: []byte(args)}
}

// drive is a loop the runtime does not own: at each step it proposes the
// decision for that step's index and executes what is allowed, until the
// session is over. It returns the last verdict.
func drive(t *testing.T, ctx context.Context, s *Session, decisions []Decision) Verdict {
	t.Helper()
	var last Verdict
	for !s.Done() {
		st, err := s.Step(ctx)
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if st == nil {
			break
		}
		if last, err = st.Propose(ctx, decisions[st.Index()]); err != nil {
			t.Fatalf("propose %d: %v", st.Index(), err)
		}
		if last.Outcome == VerdictAllowed {
			if _, err := st.Execute(ctx); err != nil {
				t.Fatalf("execute %d: %v", st.Index(), err)
			}
		}
	}
	return last
}

// A loop written outside the runtime, driving a run through the gate,
// leaves the same rows, the same events in the same transactions and
// order with the same payloads, and the same clock readings as the Driver
// running the same decisions: an allowed read, a denial, an invalid
// decision, a pause, the approval, the approved request, a terminal tool.
func TestGate_ExternalLoopMatchesTheDriver(t *testing.T) {
	ctx := context.Background()
	decisions := []Decision{
		call("read", `{"n":1}`),
		call("wipe", `{}`),
		call("nope", `{}`),
		call("push", `{"n":7}`),
		call("read", `{"n":2}`),
		call("finish", `{}`),
	}
	l := Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 5, LoopThreshold: 3}

	driven := newTwin(t)
	d := mustDriver(t, Config{Store: driven.store, Agent: &listAgent{decisions: decisions}, Tools: gateTools(), Observer: driven.observe, Now: driven.now, NewID: driven.newID})
	run, err := d.StartWithID(ctx, "r", "g", l)
	if err != nil || run.Status != StatusWaitingForApproval {
		t.Fatalf("start: %s %v", run.Status, err)
	}
	paused := driven.reads
	approvals, _ := driven.store.ListApprovals(ctx, "r")
	if err := d.Approve(ctx, "r", approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	want, err := d.Resume(ctx, "r")
	if err != nil || want.Status != StatusCompleted {
		t.Fatalf("resume: %s %v", want.Status, err)
	}

	gated := newTwin(t)
	g := gated.gate(t, gateTools(), DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", l)
	if err != nil {
		t.Fatal(err)
	}
	v := drive(t, ctx, s, decisions)
	if v.Outcome != VerdictPending || v.Run.Status != StatusWaitingForApproval || v.Approval == nil || gated.reads != paused {
		t.Fatalf("paused: %+v after %d readings, want %d", v, gated.reads, paused)
	}
	if err := (Operator{Store: gated.store, Observer: gated.observe, Now: gated.now}).Approve(ctx, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("attach: %+v %v", v, err)
	}
	if _, err := s.Current().Execute(ctx); err != nil {
		t.Fatal(err)
	}
	drive(t, ctx, s, decisions)
	if got := s.Run(); !s.Done() || !reflect.DeepEqual(got, want) {
		t.Fatalf("ended: %+v, want %+v", got, want)
	}
	s.Close()

	if got, want := snapshot(t, gated.store, "r"), snapshot(t, driven.store, "r"); got != want {
		t.Errorf("stored rows differ:\n gate   %s\n driver %s", got, want)
	}
	if !reflect.DeepEqual(gated.events, driven.events) {
		t.Errorf("events differ:\n gate   %v\n driver %v", eventTypes(gated.events), eventTypes(driven.events))
	}
	if gated.reads != driven.reads {
		t.Errorf("%d clock readings, the driver took %d", gated.reads, driven.reads)
	}
}

func eventTypes(events []Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// A denial is an observation and the session goes on; an abort ends the
// run, and the session refuses every call after it.
func TestGate_DenyContinuesAndAbortEnds(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	policy := PolicyFunc(func(_ context.Context, req ToolRequest, _ RunView) (PolicyDecision, error) {
		switch req.Spec.Name {
		case "wipe":
			return PolicyDecision{Outcome: Deny, Reason: "never"}, nil
		case "push":
			return PolicyDecision{Outcome: Abort, Reason: "stop everything"}, nil
		}
		return PolicyDecision{Outcome: Allow}, nil
	})
	tools := gateTools()
	g := w.gate(t, tools, policy)
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, _ := s.Step(ctx)
	v, err := st.Propose(ctx, call("wipe", `{}`))
	if err != nil || v.Outcome != VerdictDenied || v.Observation == nil || v.Observation.Kind != ObservePolicyDenied || s.Done() {
		t.Fatalf("deny: %+v %v", v, err)
	}
	if tools[2].(*countedTool).calls.Load() != 0 {
		t.Fatal("a denied tool ran")
	}
	st, err = s.Step(ctx)
	if err != nil || st == nil || st.Index() != 1 {
		t.Fatalf("step after a denial: %v %v", st, err)
	}
	v, err = st.Propose(ctx, call("push", `{"n":1}`))
	if err != nil || v.Outcome != VerdictEnded || v.Run.Status != StatusFailed || v.Run.Reason != ReasonPolicyAbort || !s.Done() {
		t.Fatalf("abort: %+v %v", v, err)
	}
	if _, err := s.Step(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("step after the run ended: %v", err)
	}
	if _, err := st.Execute(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("execute after the run ended: %v", err)
	}
}

// A request that needs approval pauses the run and ends the session.
// Approved, Attach hands back that step, allowed, and Execute runs exactly
// the recorded request, whatever the caller does to the verdict it was
// shown; the run then goes on to completion.
func TestGate_ApprovedStepExecutesTheRecordedRequest(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	push := tools[1].(*countedTool)
	g := w.gate(t, tools, DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	v, err := st.Propose(ctx, call("push", `{"n":7}`))
	if err != nil || v.Outcome != VerdictPending || v.Approval == nil || v.Approval.Kind != string(RemoteMutation) || !s.Done() {
		t.Fatalf("pause: %+v %v", v, err)
	}
	if owner, _ := storedLease(t, w.store, "r"); owner != "" {
		t.Fatalf("a paused run kept its lease: %q", owner)
	}
	if _, err := s.Step(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("step on a paused session: %v", err)
	}
	if _, err := st.Propose(ctx, call("read", `{}`)); !errors.Is(err, ErrRunState) {
		t.Fatalf("propose on a paused session: %v", err)
	}
	if err := Approve(ctx, w.store, nil, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed || v.Request == nil || v.Request.Spec.Name != "push" || string(v.Request.Args) != `{"n":7}` {
		t.Fatalf("attach: %+v %v", v, err)
	}
	defer s.Close()
	if got, _ := w.store.GetRun(ctx, "r"); got.Status != StatusWaitingForApproval {
		t.Fatalf("an allowed verdict moved the run to %s before Execute", got.Status)
	}
	copy(v.Request.Args, `{"n":9}`)
	open := s.Current()
	if open == nil || open.Index() != 0 || open.Model() != nil {
		t.Fatalf("current = %+v", open)
	}
	if _, err := s.Step(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("a second step while the approved one is open: %v", err)
	}
	obs, err := open.Execute(ctx)
	if err != nil || obs.Kind != ObserveToolResult || push.calls.Load() != 1 || push.args.Load() != `{"n":7}` {
		t.Fatalf("execute: %+v %v, %d calls with %v", obs, err, push.calls.Load(), push.args.Load())
	}
	if _, err := open.Execute(ctx); !errors.Is(err, ErrRunState) || push.calls.Load() != 1 {
		t.Fatalf("second execute: %v, %d calls", err, push.calls.Load())
	}
	if v := drive(t, ctx, s, []Decision{{}, {Kind: DecideComplete, Result: []byte(`{"ok":true}`)}}); v.Outcome != VerdictEnded || v.Run.Status != StatusCompleted {
		t.Fatalf("ended: %+v", v)
	}
}

// A policy that wants a different approval at Attach pauses the run again
// and nothing runs; rejecting that approval cancels the run, which Attach
// then refuses.
func TestGate_ChangedPolicyRepausesAndRejectCancels(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	push := tools[1].(*countedTool)
	shown := "first"
	policy := PolicyFunc(func(_ context.Context, req ToolRequest, _ RunView) (PolicyDecision, error) {
		if req.Spec.SideEffect == ReadOnly {
			return PolicyDecision{Outcome: Allow}, nil
		}
		return NeedApproval("release", "an operator decides", nil, map[string]string{"shown": shown})
	})
	g := w.gate(t, tools, policy)
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	v, err := st.Propose(ctx, call("push", `{"n":1}`))
	if err != nil || v.Outcome != VerdictPending {
		t.Fatalf("pause: %+v %v", v, err)
	}
	first := v.Approval.ID
	if err := Approve(ctx, w.store, nil, "r", first, "joey", ""); err != nil {
		t.Fatal(err)
	}
	shown = "second"
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictPending || v.Approval == nil || v.Approval.ID == first || !s.Done() || push.calls.Load() != 0 {
		t.Fatalf("attach under a changed policy: %+v %v, %d calls", v, err, push.calls.Load())
	}
	if err := Reject(ctx, w.store, nil, "r", v.Approval.ID, "joey", "no"); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.store.GetRun(ctx, "r"); got.Status != StatusCancelled || got.Reason != ReasonApprovalRejected {
		t.Fatalf("rejected: %s %s", got.Status, got.Reason)
	}
	if _, _, err := g.Attach(ctx, "r"); !errors.Is(err, ErrRunState) || push.calls.Load() != 0 {
		t.Fatalf("attach of a cancelled run: %v, %d calls", err, push.calls.Load())
	}
}

// The step, consecutive failure, and loop limits fire in Step: the run
// ends, Step returns no step and no error, and Run reports why.
func TestGate_LimitsFireThroughStep(t *testing.T) {
	cases := map[string]struct {
		limits    Limits
		decisions []Decision
		reason    TerminalReason
	}{
		"steps":    {Limits{MaxSteps: 2, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, []Decision{call("read", `{"n":1}`), call("read", `{"n":2}`)}, ReasonLimitSteps},
		"failures": {Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 2, LoopThreshold: 3}, []Decision{call("wipe", `{}`), call("nope", `{}`)}, ReasonRepeatedToolFailures},
		"loop":     {Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 2}, []Decision{call("read", `{"n":1}`), call("read", `{"n":1}`)}, ReasonLoopDetected},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w := newTwin(t)
			s, err := w.gate(t, gateTools(), DefaultPolicy()).Begin(ctx, "r", "g", c.limits)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			drive(t, ctx, s, c.decisions)
			if run := s.Run(); run.Status != StatusFailed || run.Reason != c.reason || run.StepCount != len(c.decisions) {
				t.Fatalf("run = %s %s after %d steps", run.Status, run.Reason, run.StepCount)
			}
			if _, err := s.Step(ctx); !errors.Is(err, ErrRunState) {
				t.Fatalf("step after the limit: %v", err)
			}
		})
	}
}

// The model call, token, and cost limits fire in the step's ModelCaller,
// before anything is sent, and Fail ends the run with the limit's reason.
func TestGate_ModelLimitsFireThroughTheStepsModel(t *testing.T) {
	base := Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}
	cases := map[string]struct {
		limits func(Limits) Limits
		reason TerminalReason
		sent   int32
	}{
		"calls":  {func(l Limits) Limits { l.MaxModelCalls = 1; return l }, ReasonLimitModelCalls, 1},
		"tokens": {func(l Limits) Limits { l.MaxTotalTokens = 5; return l }, ReasonLimitTokens, 0},
		"cost":   {func(l Limits) Limits { l.MaxEstimatedCost = 1; return l }, ReasonLimitCost, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w := newTwin(t)
			m := &countingModel{}
			g, err := NewGate(GateConfig{Store: w.store, Policy: DefaultPolicy(), Tools: gateTools(), Now: w.now, NewID: w.newID,
				Model: &ModelConfig{Model: m, Prices: PriceTable{"m": {InputPerMTok: 1e9, OutputPerMTok: 1e9}}}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := g.Begin(ctx, "r", "g", c.limits(base))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var lim ErrLimit
			for !s.Done() {
				st, err := s.Step(ctx)
				if err != nil || st == nil {
					t.Fatalf("step: %v", err)
				}
				if _, err := st.Model().Generate(ctx, ModelRequest{MaxOutputTokens: 10}); err != nil {
					if !errors.As(err, &lim) {
						t.Fatalf("generate: %v", err)
					}
					v, err := st.Fail(ctx, err)
					if err != nil || v.Outcome != VerdictEnded || v.Run.Reason != c.reason {
						t.Fatalf("fail: %+v %v", v, err)
					}
					break
				}
				if _, err := st.Propose(ctx, call("read", fmt.Sprintf(`{"n":%d}`, st.Index()))); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Execute(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if lim.Reason != c.reason || m.calls.Load() != c.sent {
				t.Fatalf("limit %q, %d calls sent", lim.Reason, m.calls.Load())
			}
			calls, _ := w.store.ListModelCalls(ctx, "r")
			for _, mc := range calls {
				if mc.StepID == "" {
					t.Fatalf("a model call charged to no step: %+v", mc)
				}
			}
		})
	}
}

// A session refuses, with typed errors and without changing anything, a
// second open step, an Execute with no Allowed verdict, a second Execute,
// a second decision, and every call after Close.
func TestGate_MisuseIsRefused(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	read := tools[0].(*countedTool)
	g := w.gate(t, tools, DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	before := snapshot(t, w.store, "r")
	if _, err := s.Step(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("second open step: %v", err)
	}
	if _, err := st.Execute(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("execute before a verdict: %v", err)
	}
	if _, err := st.Fail(ctx, nil); err == nil {
		t.Fatal("Fail without an error was accepted")
	}
	if after := snapshot(t, w.store, "r"); after != before {
		t.Fatalf("refused calls changed the record:\n%s\n%s", before, after)
	}
	if v, err := st.Propose(ctx, call("wipe", `{}`)); err != nil || v.Outcome != VerdictDenied {
		t.Fatalf("deny: %+v %v", v, err)
	}
	if _, err := st.Execute(ctx); !errors.Is(err, ErrRunState) {
		t.Fatalf("execute after a denial: %v", err)
	}
	st, _ = s.Step(ctx)
	if v, err := st.Propose(ctx, call("read", `{"n":1}`)); err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("allow: %+v %v", v, err)
	}
	if _, err := st.Propose(ctx, call("read", `{"n":2}`)); !errors.Is(err, ErrRunState) {
		t.Fatalf("second decision: %v", err)
	}
	if _, err := st.Fail(ctx, errors.New("late")); !errors.Is(err, ErrRunState) {
		t.Fatalf("fail after a decision: %v", err)
	}
	if _, err := st.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execute(ctx); !errors.Is(err, ErrRunState) || read.calls.Load() != 1 {
		t.Fatalf("second execute: %v, %d calls", err, read.calls.Load())
	}
	st, _ = s.Step(ctx)
	s.Close()
	s.Close()
	if owner, _ := storedLease(t, w.store, "r"); owner != "" {
		t.Fatalf("Close kept the lease: %q", owner)
	}
	if _, err := s.Step(ctx); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("step after Close: %v", err)
	}
	if _, err := st.Propose(ctx, call("read", `{"n":3}`)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("propose after Close: %v", err)
	}
	if !s.Done() || s.Current() != nil {
		t.Fatal("a closed session is not done")
	}
}

// leaseTwins are two gates on one store, as two processes, whose wall
// clock a test moves; the first renews nothing, as a process that died.
// The lease is long, so nothing expires unless the test moves the clock.
func leaseTwins(t *testing.T, tools []Tool) (*twin, *Gate, *Gate, *wallClock) {
	t.Helper()
	w := newTwin(t)
	wall := newWallClock()
	gates := make([]*Gate, 2)
	for i := range gates {
		g, err := NewGate(GateConfig{Store: w.store, Policy: DefaultPolicy(), Tools: tools, Now: w.now, NewID: w.newID, LeaseTTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		g.d.wall = wall.Now
		gates[i] = g
	}
	gates[0].d.noHeartbeat = true
	return w, gates[0], gates[1], wall
}

// A session whose lease was lost, taken over by another process or
// expired unrenewed, refuses every call with ErrLeaseLost and starts
// nothing.
func TestGate_CallsAfterTheLeaseIsLostAreRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("taken over", func(t *testing.T) {
		tools := gateTools()
		w, g1, g2, wall := leaseTwins(t, tools)
		s, err := g1.Begin(ctx, "r", "g", leaseLimits())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		st, _ := s.Step(ctx)
		wall.advance(2 * time.Minute)
		s2, v, err := g2.Attach(ctx, "r")
		if err != nil || v.Outcome != "" {
			t.Fatalf("takeover: %+v %v", v, err)
		}
		defer s2.Close()
		before := snapshot(t, w.store, "r")
		if _, err := st.Propose(ctx, call("read", `{"n":1}`)); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("propose: %v", err)
		}
		if _, err := s.Step(ctx); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("step: %v", err)
		}
		if after := snapshot(t, w.store, "r"); after != before || tools[0].(*countedTool).calls.Load() != 0 {
			t.Fatalf("the old session changed the record:\n%s\n%s", before, after)
		}
	})
	t.Run("expired", func(t *testing.T) {
		tools := gateTools()
		_, g1, _, wall := leaseTwins(t, tools)
		s, err := g1.Begin(ctx, "r", "g", leaseLimits())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		st, _ := s.Step(ctx)
		if v, err := st.Propose(ctx, call("read", `{"n":1}`)); err != nil || v.Outcome != VerdictAllowed {
			t.Fatalf("allow: %+v %v", v, err)
		}
		wall.advance(2 * time.Minute)
		if _, err := st.Execute(ctx); !errors.Is(err, ErrLeaseLost) || tools[0].(*countedTool).calls.Load() != 0 {
			t.Fatalf("execute on an expired lease: %v, %d calls", err, tools[0].(*countedTool).calls.Load())
		}
		if _, err := s.Step(ctx); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("step: %v", err)
		}
	})
}

// A session abandoned between an Allowed verdict and Execute, as by a
// crash, left the step deciding with nothing about the request committed
// but its decision: a fresh gate taking the run over once the lease has
// expired marks it interrupted and runs nothing, and the abandoned step
// can no longer execute either.
func TestGate_AbandonedAfterAllowedRunsNothing(t *testing.T) {
	ctx := context.Background()
	write := counted("write", LocalMutation)
	w, g1, g2, wall := leaseTwins(t, []Tool{write})
	s, err := g1.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	if v, err := st.Propose(ctx, call("write", `{"n":1}`)); err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("allow: %+v %v", v, err)
	}
	steps, _ := w.store.ListSteps(ctx, "r")
	if len(steps) != 1 || steps[0].Status != StepDeciding || steps[0].Policy != nil {
		t.Fatalf("an allowed step was recorded as %+v", steps[0])
	}
	wall.advance(2 * time.Minute)
	s2, v, err := g2.Attach(ctx, "r")
	if err != nil || v.Outcome != "" {
		t.Fatalf("attach: %+v %v", v, err)
	}
	if _, err := st.Execute(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the abandoned step executed: %v", err)
	}
	if v := drive(t, ctx, s2, []Decision{{}, {Kind: DecideComplete}}); v.Outcome != VerdictEnded || v.Run.Status != StatusCompleted {
		t.Fatalf("ended: %+v", v)
	}
	steps, _ = w.store.ListSteps(ctx, "r")
	if steps[0].Status != StepInterrupted || write.calls.Load() != 0 {
		t.Fatalf("step 0 = %s, %d calls", steps[0].Status, write.calls.Load())
	}
	events, _ := w.store.ListEvents(ctx, "r")
	for _, e := range events {
		if e.Type == EventStepToolStarted || e.Type == EventStepPolicy {
			t.Fatalf("an undecided request left %s", e.Type)
		}
	}
}

// A session abandoned while a tool that is not ReadOnly runs leaves the
// step executing. A fresh gate with no Reconcile, once the lease has
// expired, pauses the run as interrupted_side_effect; the tool runs again
// only when an operator approves, and the abandoned session cannot record
// over the run.
func TestGate_AbandonedDuringExecutePausesForAnOperator(t *testing.T) {
	ctx := context.Background()
	work := newGate("work")
	w, g1, g2, wall := leaseTwins(t, []Tool{work})
	s, err := g1.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	if v, err := st.Propose(ctx, call("work", `{}`)); err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("allow: %+v %v", v, err)
	}
	done := make(chan error, 1)
	go func() { _, err := st.Execute(ctx); done <- err }()
	<-work.entered
	wall.advance(2 * time.Minute)
	s2, v, err := g2.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictPending || v.Approval.Kind != InterruptedSideEffect || !s2.Done() {
		t.Fatalf("attach: %+v %v", v, err)
	}
	close(work.release)
	if err := <-done; !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the abandoned execute: %v", err)
	}
	if got, _ := w.store.GetRun(ctx, "r"); got.Status != StatusWaitingForApproval || work.calls.Load() != 1 {
		t.Fatalf("run %s, %d calls", got.Status, work.calls.Load())
	}
	if err := Approve(ctx, w.store, nil, "r", v.Approval.ID, "joey", "checked by hand"); err != nil {
		t.Fatal(err)
	}
	s2, v, err = g2.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed || v.Request.Spec.Name != "work" {
		t.Fatalf("attach after approval: %+v %v", v, err)
	}
	defer s2.Close()
	if _, err := s2.Current().Execute(ctx); err != nil || work.calls.Load() != 2 {
		t.Fatalf("approved re-run: %v, %d calls", err, work.calls.Load())
	}
}

// While one session holds a run, another Begin or Attach of it, by the
// same gate or another process, is refused and changes nothing; once the
// first is closed the run is free without waiting for the lease.
func TestGate_SecondSessionIsRefused(t *testing.T) {
	ctx := context.Background()
	w, g1, g2, _ := leaseTwins(t, gateTools())
	s, err := g1.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, w.store, "r")
	var leased ErrRunLeased
	if _, _, err := g1.Attach(ctx, "r"); !errors.As(err, &leased) {
		t.Fatalf("same gate attach: %v", err)
	}
	if _, err := g1.Begin(ctx, "r", "g", leaseLimits()); !errors.As(err, &leased) {
		t.Fatalf("same gate begin: %v", err)
	}
	if _, _, err := g2.Attach(ctx, "r"); !errors.As(err, &leased) || leased.Owner != g1.d.terms.owner {
		t.Fatalf("other gate attach: %v", err)
	}
	if _, err := g2.Begin(ctx, "r", "g", leaseLimits()); err == nil {
		t.Fatal("other gate began a run that exists")
	}
	if after := snapshot(t, w.store, "r"); after != before {
		t.Fatalf("refused sessions changed the record:\n%s\n%s", before, after)
	}
	s.Close()
	s2, _, err := g2.Attach(ctx, "r")
	if err != nil {
		t.Fatalf("attach after Close: %v", err)
	}
	s2.Close()
	events, _ := w.store.ListEvents(ctx, "r")
	for _, e := range events {
		if e.Type == EventLeaseTakenOver {
			t.Fatal("a released lease was recorded as taken over")
		}
	}
}

// NewGate checks what NewDriver checks, without an agent.
func TestGate_ConfigIsValidated(t *testing.T) {
	st := memStore(t)
	for name, cfg := range map[string]GateConfig{
		"store":  {Policy: DefaultPolicy()},
		"policy": {Store: st},
		"model":  {Store: st, Policy: DefaultPolicy(), Model: &ModelConfig{}},
		"tool":   {Store: st, Policy: DefaultPolicy(), Tools: []Tool{&stubTool{spec: ToolSpec{Name: "t"}}}},
	} {
		if _, err := NewGate(cfg); err == nil || !strings.HasPrefix(err.Error(), "agentrt:") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An approved step Attach left Allowed and the caller abandoned is still
// approved and WAITING, with nothing written: the next Attach finds the
// grant again and runs it once.
func TestGate_AbandonedApprovedStepStaysApproved(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	push := tools[1].(*countedTool)
	g := w.gate(t, tools, DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	v, _ := st.Propose(ctx, call("push", `{"n":7}`))
	if err := Approve(ctx, w.store, nil, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, w.store, "r")
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("attach: %+v %v", v, err)
	}
	s.Close()
	if after := snapshot(t, w.store, "r"); after != before || push.calls.Load() != 0 {
		t.Fatalf("an abandoned grant changed the record:\n%s\n%s", before, after)
	}
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("second attach: %+v %v", v, err)
	}
	defer s.Close()
	if _, err := s.Current().Execute(ctx); err != nil || push.calls.Load() != 1 {
		t.Fatalf("execute: %v, %d calls", err, push.calls.Load())
	}
	if got, _ := w.store.GetRun(ctx, "r"); got.Status != StatusRunning {
		t.Fatalf("run is %s after the approved step", got.Status)
	}
}

// grantPaused is a gate session paused on a push that an operator then
// approved on the twin's clock, and the Attach that allowed it.
func grantPaused(t *testing.T, w *twin, tools []Tool, l Limits) (*Gate, *Session, Verdict) {
	t.Helper()
	ctx := context.Background()
	g := w.gate(t, tools, DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", l)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	v, err := st.Propose(ctx, call("push", `{"n":7}`))
	if err != nil || v.Outcome != VerdictPending {
		t.Fatalf("pause: %+v %v", v, err)
	}
	if err := (Operator{Store: w.store, Now: w.now}).Approve(ctx, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, v, err = g.Attach(ctx, "r")
	if err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("attach: %+v %v", v, err)
	}
	return g, s, v
}

// A grant Attach allowed is judged against GrantTTL again at Execute: a
// caller that waits past it executes nothing, and the run is cancelled as
// approval_expired exactly as Attach would have cancelled it; within the
// TTL, or with none, the grant executes however long the wait.
func TestGate_GrantTTLHoldsUntilExecute(t *testing.T) {
	ctx := context.Background()
	withTTL := leaseLimits()
	withTTL.GrantTTL = time.Minute
	t.Run("expired", func(t *testing.T) {
		w := newTwin(t)
		tools := gateTools()
		push := tools[1].(*countedTool)
		g, s, _ := grantPaused(t, w, tools, withTTL)
		defer s.Close()
		w.at = w.at.Add(2 * time.Minute)
		_, err := s.Current().Execute(ctx)
		if !errors.Is(err, ErrApprovalExpired) || !errors.Is(err, ErrNotPending) || push.calls.Load() != 0 {
			t.Fatalf("execute of an expired grant: %v, %d calls", err, push.calls.Load())
		}
		run, _ := w.store.GetRun(ctx, "r")
		if run.Status != StatusCancelled || run.Reason != ReasonApprovalExpired || !s.Done() || s.Run().Reason != ReasonApprovalExpired {
			t.Fatalf("run %s %s, session run %s, done %v", run.Status, run.Reason, s.Run().Reason, s.Done())
		}
		approvals, _ := w.store.ListApprovals(ctx, "r")
		if approvals[0].Status != ApprovalExpired || approvals[0].DecidedBy != "joey" {
			t.Fatalf("approval %+v", approvals[0])
		}
		events, _ := w.store.ListEvents(ctx, "r")
		for _, e := range events {
			if e.Type == EventStepToolStarted || e.Type == EventRunResumed {
				t.Fatalf("an expired grant left %s", e.Type)
			}
		}
		if _, err := s.Step(ctx); !errors.Is(err, ErrSessionClosed) || !errors.Is(err, ErrApprovalExpired) {
			t.Fatalf("step after the expiry: %v", err)
		}
		if _, _, err := g.Attach(ctx, "r"); !errors.Is(err, ErrRunState) {
			t.Fatalf("attach of the cancelled run: %v", err)
		}
	})
	for name, c := range map[string]struct {
		limits Limits
		wait   time.Duration
	}{
		"within the TTL": {withTTL, 30 * time.Second},
		"no TTL":         {leaseLimits(), 10 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			w := newTwin(t)
			tools := gateTools()
			push := tools[1].(*countedTool)
			_, s, _ := grantPaused(t, w, tools, c.limits)
			defer s.Close()
			w.at = w.at.Add(c.wait)
			if obs, err := s.Current().Execute(ctx); err != nil || obs.Kind != ObserveToolResult || push.calls.Load() != 1 {
				t.Fatalf("execute: %+v %v, %d calls", obs, err, push.calls.Load())
			}
			if run, _ := w.store.GetRun(ctx, "r"); run.Status != StatusRunning {
				t.Fatalf("run is %s", run.Status)
			}
		})
	}
}

// Origin is recorded with the decision and nothing else: a decision
// without one encodes exactly as before Origin existed, and the approval a
// request pauses on hashes the same with or without it, because the hash
// covers the tool request and not the decision.
func TestDecision_OriginIsRecordedAndChangesNothingElse(t *testing.T) {
	ctx := context.Background()
	b, err := json.Marshal(Decision{Kind: DecideToolCall, Tool: "push", Args: []byte(`{"n":7}`), Reason: "r"})
	if err != nil || string(b) != `{"kind":"tool_call","tool":"push","args":{"n":7},"reason":"r"}` {
		t.Fatalf("encoded %s %v", b, err)
	}
	paused := func(origin DecisionOrigin) (*twin, Approval) {
		w := newTwin(t)
		s, err := w.gate(t, gateTools(), DefaultPolicy()).Begin(ctx, "r", "g", leaseLimits())
		if err != nil {
			t.Fatal(err)
		}
		st, _ := s.Step(ctx)
		d := call("push", `{"n":7}`)
		d.Origin = origin
		v, err := st.Propose(ctx, d)
		if err != nil || v.Outcome != VerdictPending {
			t.Fatalf("pause: %+v %v", v, err)
		}
		return w, *v.Approval
	}
	plain, a := paused("")
	_, b2 := paused(OriginModel)
	if a.Hash != b2.Hash || !reflect.DeepEqual(a, b2) {
		t.Fatalf("approvals differ with an origin:\n %+v\n %+v", a, b2)
	}
	events, _ := plain.store.ListEvents(ctx, "r")
	for _, e := range events {
		if e.Type == EventStepDecided && string(e.Payload) != `{"kind":"tool_call","tool":"push","args":{"n":7}}` {
			t.Fatalf("step.decided payload %s", e.Payload)
		}
	}
	var stored string
	if err := plain.store.DB().QueryRow(`SELECT decision_json FROM steps WHERE run_id = 'r'`).Scan(&stored); err != nil || stored != `{"kind":"tool_call","tool":"push","args":{"n":7}}` {
		t.Fatalf("stored decision %s %v", stored, err)
	}
}

// An origin outside the known set is an invalid decision, recorded
// verbatim and ended with an invalid_decision observation, through a gate
// and a Driver alike; each known origin is accepted.
func TestDecision_UnknownOriginIsInvalid(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	s, err := w.gate(t, tools, DefaultPolicy()).Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, origin := range []DecisionOrigin{"robot", OriginModel, OriginPlan, OriginOperator} {
		st, _ := s.Step(ctx)
		d := call("read", fmt.Sprintf(`{"n":%d}`, i))
		d.Origin = origin
		v, err := st.Propose(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if v.Outcome != VerdictDenied || v.Observation.Kind != ObserveInvalidDecision || !strings.Contains(v.Observation.Summary, `unknown decision origin "robot"`) {
				t.Fatalf("unknown origin: %+v", v)
			}
			continue
		}
		if v.Outcome != VerdictAllowed {
			t.Fatalf("origin %s: %+v", origin, v)
		}
		if _, err := st.Execute(ctx); err != nil {
			t.Fatal(err)
		}
	}
	steps, _ := w.store.ListSteps(ctx, "r")
	if steps[0].Decision.Origin != "robot" || steps[3].Decision.Origin != OriginOperator || tools[0].(*countedTool).calls.Load() != 3 {
		t.Fatalf("recorded %+v / %+v", steps[0].Decision, steps[3].Decision)
	}

	driven := newTwin(t)
	d := mustDriver(t, Config{Store: driven.store, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete, Origin: "robot"}, {Kind: DecideComplete}}}, Tools: gateTools(), Now: driven.now, NewID: driven.newID})
	run, err := d.Start(ctx, "g", leaseLimits())
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("driver: %s %v", run.Status, err)
	}
	steps, _ = driven.store.ListSteps(ctx, run.ID)
	if steps[0].Observation == nil || steps[0].Observation.Kind != ObserveInvalidDecision {
		t.Fatalf("driver step 0 = %+v", steps[0])
	}
}

// inputAgent records what a Driver hands its agent at every decision.
type inputAgent struct {
	listAgent
	inputs []StepInput
}

func (a *inputAgent) Decide(ctx context.Context, in StepInput) (Decision, error) {
	a.inputs = append(a.inputs, in)
	return a.listAgent.Decide(ctx, in)
}

// Session.Input while a step is open is exactly what a Driver's agent is
// handed at that step, through a denial, an invalid decision, a pause, the
// approval, and the approved request; asking for it reads no clock, so the
// two runs stay reading for reading alike.
func TestGate_InputMatchesTheAgentsStepInput(t *testing.T) {
	ctx := context.Background()
	decisions := []Decision{
		call("read", `{"n":1}`),
		call("wipe", `{}`),
		call("nope", `{}`),
		call("push", `{"n":7}`),
		call("read", `{"n":2}`),
		call("finish", `{}`),
	}
	l := Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 5, LoopThreshold: 3}

	driven := newTwin(t)
	agent := &inputAgent{listAgent: listAgent{decisions: decisions}}
	d := mustDriver(t, Config{Store: driven.store, Agent: agent, Tools: gateTools(), Observer: driven.observe, Now: driven.now, NewID: driven.newID})
	if _, err := d.StartWithID(ctx, "r", "g", l); err != nil {
		t.Fatal(err)
	}
	approvals, _ := driven.store.ListApprovals(ctx, "r")
	if err := d.Approve(ctx, "r", approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	if run, err := d.Resume(ctx, "r"); err != nil || run.Status != StatusCompleted {
		t.Fatalf("resume: %s %v", run.Status, err)
	}

	gated := newTwin(t)
	g := gated.gate(t, gateTools(), DefaultPolicy())
	var inputs []StepInput
	loop := func(s *Session) Verdict {
		var v Verdict
		for !s.Done() {
			st, err := s.Step(ctx)
			if err != nil || st == nil {
				t.Fatalf("step: %v", err)
			}
			in, err := s.Input(ctx)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, in)
			if v, err = st.Propose(ctx, decisions[st.Index()]); err != nil {
				t.Fatal(err)
			}
			if v.Outcome == VerdictAllowed {
				if _, err := st.Execute(ctx); err != nil {
					t.Fatal(err)
				}
			}
		}
		return v
	}
	s, err := g.Begin(ctx, "r", "g", l)
	if err != nil {
		t.Fatal(err)
	}
	v := loop(s)
	if err := (Operator{Store: gated.store, Observer: gated.observe, Now: gated.now}).Approve(ctx, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, _, err = g.Attach(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Current().Execute(ctx); err != nil {
		t.Fatal(err)
	}
	loop(s)

	if len(inputs) != len(agent.inputs) || len(inputs) != 6 {
		t.Fatalf("%d inputs, the agent was handed %d", len(inputs), len(agent.inputs))
	}
	for i := range inputs {
		if !reflect.DeepEqual(inputs[i], agent.inputs[i]) {
			t.Errorf("input %d differs:\n gate  %+v\n agent %+v", i, inputs[i], agent.inputs[i])
		}
	}
	if gated.reads != driven.reads || snapshot(t, gated.store, "r") != snapshot(t, driven.store, "r") {
		t.Errorf("asking for input changed the run: %d readings, the driver took %d", gated.reads, driven.reads)
	}
}

// Between steps Input is the run as the session left it and every step
// recorded, as the store has them: before the first step, after a denial,
// and after Execute. For the approved step Attach left open, it is the
// WAITING run and the steps before it. After the first call no call loads
// the steps again.
func TestGate_InputAtEveryPointOfASession(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	g := w.gate(t, gateTools(), DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	check := func(at string, wantRun Run, n int) {
		t.Helper()
		in, err := s.Input(ctx)
		if err != nil {
			t.Fatal(err)
		}
		steps, _ := w.store.ListSteps(ctx, "r")
		approvals, _ := w.store.ListApprovals(ctx, "r")
		var want []Step
		if n > 0 {
			want = steps[:n]
		}
		if !reflect.DeepEqual(in.Run, wantRun) || !reflect.DeepEqual(in.Steps, want) || !reflect.DeepEqual(in.Approvals, approvals) || !reflect.DeepEqual(in.Tools, g.d.specs) {
			t.Fatalf("%s: input %+v\n want run %+v, %d steps, approvals %+v", at, in, wantRun, n, approvals)
		}
	}
	check("before the first step", s.Run(), 0)
	c := s.c
	st, _ := s.Step(ctx)
	check("open", st.run, 0)
	if v, err := st.Propose(ctx, call("wipe", `{}`)); err != nil || v.Outcome != VerdictDenied {
		t.Fatalf("deny: %+v %v", v, err)
	}
	check("after a denial", s.Run(), 1)
	st, _ = s.Step(ctx)
	if v, err := st.Propose(ctx, call("read", `{"n":1}`)); err != nil || v.Outcome != VerdictAllowed {
		t.Fatalf("allow: %+v %v", v, err)
	}
	check("allowed", st.run, 1)
	if _, err := st.Execute(ctx); err != nil {
		t.Fatal(err)
	}
	check("after Execute", s.Run(), 2)
	if s.c != c {
		t.Fatal("Input loaded the steps again")
	}
	st, _ = s.Step(ctx)
	v, err := st.Propose(ctx, call("push", `{"n":7}`))
	if err != nil || v.Outcome != VerdictPending {
		t.Fatalf("pause: %+v %v", v, err)
	}
	if err := Approve(ctx, w.store, nil, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, _, err = g.Attach(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waiting, _ := w.store.GetRun(ctx, "r")
	check("approved and open", waiting, 2)
	if _, err := s.Current().Execute(ctx); err != nil {
		t.Fatal(err)
	}
	running, _ := w.store.GetRun(ctx, "r")
	if run := s.Run(); run.Status != StatusWaitingForApproval || running.Status != StatusRunning {
		t.Fatalf("session run %s, stored %s", run.Status, running.Status)
	}
	check("after the approved step", s.Run(), 3)
}

// What Input returns is the caller's: writing into it changes neither the
// session's copy, which the policy's view and the next write rest on, nor
// the record.
func TestGate_InputIsACopy(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	g := w.gate(t, gateTools(), DefaultPolicy())
	s, err := g.Begin(ctx, "r", "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, _ := s.Step(ctx)
	v, _ := st.Propose(ctx, call("push", `{"n":7}`))
	if err := Approve(ctx, w.store, nil, "r", v.Approval.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	s, _, err = g.Attach(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Current().Execute(ctx); err != nil {
		t.Fatal(err)
	}
	in, err := s.Input(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copy(in.Steps[0].Decision.Args, "XXXXXXX")
	copy(in.Steps[0].Observation.Content, "XXXXXXX")
	in.Steps[0].Decision.Tool = "wipe"
	copy(in.Approvals[0].Request.Args, "XXXXXXX")
	copy(in.Approvals[0].Capability, "XXXXXXX")
	copy(in.Tools[0].InputSchema, "XXXXXXX")
	in.Tools[0].Name = "x"
	steps, _ := w.store.ListSteps(ctx, "r")
	approvals, _ := w.store.ListApprovals(ctx, "r")
	if !reflect.DeepEqual(s.c.steps, steps) || !reflect.DeepEqual(s.c.approvals, approvals) || g.d.specs[0].Name != "finish" || string(g.d.specs[0].InputSchema[:1]) != "{" {
		t.Fatal("writing into the input changed the session's copy")
	}
	pv, pa := s.c.policy.view(s.c, 1)
	if !reflect.DeepEqual(pv, steps[:1]) || !reflect.DeepEqual(pa, approvals) {
		t.Fatal("writing into the input changed the policy's view")
	}
	st, _ = s.Step(ctx)
	if v, err := st.Propose(ctx, Decision{Kind: DecideComplete}); err != nil || v.Run.Status != StatusCompleted {
		t.Fatalf("complete: %+v %v", v, err)
	}
	if err := verifyHash(approvals[0]); err != nil {
		t.Fatal(err)
	}
}

// A Gate whose sessions are all over, ended by the run or closed by the
// caller, holds no goroutine: each session's heartbeat stops with it.
func TestGate_ClosedSessionsLeaveNoGoroutine(t *testing.T) {
	ctx := context.Background()
	w := newTwin(t)
	tools := gateTools()
	g := w.gate(t, tools, DefaultPolicy())
	g.d.renewEvery = 5 * time.Millisecond
	base := runtime.NumGoroutine()
	var sessions []*Session
	for i := range 3 {
		id := fmt.Sprintf("r%d", i)
		s, err := g.Begin(ctx, id, "g", leaseLimits())
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
		switch i {
		case 0:
			// Completed: the run ends the session.
			drive(t, ctx, s, []Decision{call("read", `{"n":1}`), {Kind: DecideComplete}})
		case 1:
			// Paused, approved, and executed under a second session.
			v := drive(t, ctx, s, []Decision{call("push", `{"n":1}`)})
			if err := Approve(ctx, w.store, nil, id, v.Approval.ID, "joey", ""); err != nil {
				t.Fatal(err)
			}
			s, v, err = g.Attach(ctx, id)
			if err != nil || v.Outcome != VerdictAllowed {
				t.Fatalf("attach: %+v %v", v, err)
			}
			sessions = append(sessions, s)
			if _, err := s.Current().Execute(ctx); err != nil {
				t.Fatal(err)
			}
		case 2:
			// Left mid-step.
			st, _ := s.Step(ctx)
			if _, err := st.Propose(ctx, call("read", `{"n":1}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if runtime.NumGoroutine() <= base {
		t.Fatal("no heartbeat is running; the test would prove nothing")
	}
	for _, s := range sessions {
		s.Close()
	}
	settled(t, base)
}
