package agentrt_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// countingTool counts executions safely across goroutines and processes'
// worth of drivers.
type countingTool struct {
	spec agentrt.ToolSpec
	n    atomic.Int32
}

func (c *countingTool) Spec() agentrt.ToolSpec { return c.spec }
func (c *countingTool) Call(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
	c.n.Add(1)
	return agentrt.ToolResult{Content: []byte(`{}`)}, nil
}

// Two processes resume the same approved run at once. A third holds the
// write lock while both read the run as WAITING, so both reach the
// transition believing the run is theirs. The move to RUNNING is
// compare-and-set: one executes the approved side effect and the other
// gets ErrRunState.
func TestResume_ConcurrentResumeExecutesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	open := func() *agentrt.Store {
		s, err := agentrt.OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	s1, s2, holder := open(), open(), open()
	push := &countingTool{spec: agentrt.ToolSpec{Name: "push", Description: "push", InputSchema: []byte(`{"type":"object"}`), SideEffect: agentrt.RemoteMutation, Timeout: time.Second}}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{}`, ""), scripted.Complete(`{}`)}}
	d1, _ := agentrt.NewDriver(agentrt.Config{Store: s1, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{push}})
	d2, _ := agentrt.NewDriver(agentrt.Config{Store: s2, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{push}})
	ctx := context.Background()
	run, err := d1.Start(ctx, "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	approvals, _ := s1.ListApprovals(ctx, run.ID)
	if err := d1.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	lock, err := holder.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(`UPDATE runs SET goal = goal WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, d := range []*agentrt.Driver{d1, d2} {
		wg.Add(1)
		go func(i int, d *agentrt.Driver) { defer wg.Done(); _, errs[i] = d.Resume(ctx, run.ID) }(i, d)
	}
	time.Sleep(300 * time.Millisecond)
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := push.n.Load(); n != 1 {
		t.Fatalf("approved side effect executed %d times", n)
	}
	lost := 0
	for _, err := range errs {
		if errors.Is(err, agentrt.ErrRunState) {
			lost++
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if lost != 1 {
		t.Fatalf("errs = %v, want exactly one ErrRunState", errs)
	}
	got, _ := s1.GetRun(ctx, run.ID)
	requireStatus(t, got, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
}

// An operator's Cancel while a tool runs is not overwritten: the loop's
// next write fails and the loop returns the cancelled run.
func TestCancel_LiveLoopStopsAtItsNextWrite(t *testing.T) {
	h := newHarness(t, ":memory:")
	var runID string
	slow := newTool("work", agentrt.LocalMutation, emptySchema, func(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
		if err := agentrt.Cancel(ctx, h.store, nil, c.RunID, "joey", "stop"); err != nil {
			t.Errorf("cancel: %v", err)
		}
		runID = c.RunID
		return agentrt.ToolResult{Content: []byte(`{"done":true}`)}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("work", `{}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), slow).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCancelled, agentrt.ReasonOperatorCancelled)
	got, _ := h.store.GetRun(context.Background(), runID)
	requireStatus(t, got, agentrt.StatusCancelled, agentrt.ReasonOperatorCancelled)
	steps, _ := h.store.ListSteps(context.Background(), runID)
	if len(steps) != 1 || steps[0].Status != agentrt.StepFailed || steps[0].Observation.Kind != agentrt.ObserveInterrupted {
		t.Fatalf("steps = %+v", steps)
	}
}

// A policy decision with an unknown or empty outcome fails the run as
// internal_error, in the loop and on resume alike; nothing executes.
func TestPolicy_UnknownOutcomeFailsLoopAndResume(t *testing.T) {
	zero := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{}, nil
	})
	for _, via := range []string{"loop", "resume"} {
		t.Run(via, func(t *testing.T) {
			h := newHarness(t, ":memory:")
			push := echoTool("push", agentrt.RemoteMutation)
			agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
			ctx := context.Background()
			var run agentrt.Run
			if via == "loop" {
				run, _ = h.driver(agent, zero, push).Start(ctx, "g", limits(10, 3))
			} else {
				paused, _ := h.driver(agent, agentrt.DefaultPolicy(), push).Start(ctx, "g", limits(10, 3))
				approvals, _ := h.store.ListApprovals(ctx, paused.ID)
				d := h.driver(agent, zero, push)
				if err := d.Approve(ctx, paused.ID, approvals[0].ID, "joey", ""); err != nil {
					t.Fatal(err)
				}
				var err error
				if run, err = d.Resume(ctx, paused.ID); err != nil {
					t.Fatal(err)
				}
			}
			requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonInternalError)
			if len(push.calls) != 0 {
				t.Fatal("executed on an unknown policy outcome")
			}
		})
	}
}

// Arguments that are not usable JSON are recorded, as a string, on an
// invalid_decision step instead of panicking the store; so is a result.
func TestDriver_UnusableJSONDecisionsAreRecordedInvalid(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	note := newTool("note", agentrt.ReadOnly, `{"type":"object"}`, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{}`)}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		{Kind: agentrt.DecideToolCall, Tool: "read", Args: json.RawMessage(`{"n":`)},
		{Kind: agentrt.DecideToolCall, Tool: "read", Args: json.RawMessage(`{"n":1,"n":2}`)},
		{Kind: agentrt.DecideComplete, Result: json.RawMessage("\"\xff\"")},
		{Kind: agentrt.DecideToolCall, Tool: "note", Args: json.RawMessage(`{"s":"\ud800"}`)},
		scripted.Complete(`{}`),
	}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), read, note).Start(context.Background(), "g", limits(10, 5))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	for i, st := range steps[:4] {
		if st.Status != agentrt.StepFailed || st.Observation.Kind != agentrt.ObserveInvalidDecision {
			t.Fatalf("step %d = %s %+v", i, st.Status, st.Observation)
		}
	}
	var kept map[string]string
	if err := json.Unmarshal(steps[0].Decision.Args, &kept); err != nil || kept["invalid_json"] != `{"n":` {
		t.Fatalf("recorded args = %s", steps[0].Decision.Args)
	}
	if !strings.Contains(steps[1].Observation.Summary, "duplicate key") {
		t.Fatalf("duplicate key summary = %q", steps[1].Observation.Summary)
	}
	if !strings.Contains(string(steps[2].Decision.Result), "invalid_json_base64") {
		t.Fatalf("recorded result = %s", steps[2].Decision.Result)
	}
	if !strings.Contains(steps[3].Observation.Summary, "unpaired surrogate") || !strings.Contains(string(steps[3].Decision.Args), "invalid_json") {
		t.Fatalf("lone surrogate: %s, args %s", steps[3].Observation.Summary, steps[3].Decision.Args)
	}
	if len(read.calls) != 0 || len(note.calls) != 0 {
		t.Fatal("an unusable request executed")
	}
}

// Content that is not usable JSON arrives after the side effect: it is
// observed as a tool_error keeping the bytes, and the run goes on.
func TestDriver_UnusableToolContentIsToolError(t *testing.T) {
	h := newHarness(t, ":memory:")
	write := newTool("write", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: json.RawMessage("not json"), Summary: "wrote"}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("write", `{}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), write).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	var content map[string]string
	json.Unmarshal(steps[0].Observation.Content, &content)
	if steps[0].Observation.Kind != agentrt.ObserveToolError || content["content"] != "not json" || content["failure"] != "invalid_content" || len(write.calls) != 1 {
		t.Fatalf("step = %+v calls = %d", steps[0].Observation, len(write.calls))
	}
}

// A capability or presentation that is not usable JSON fails the run as
// internal_error before anything executes.
func TestDriver_UnusablePolicyJSONFailsWithoutExecuting(t *testing.T) {
	h := newHarness(t, ":memory:")
	push := echoTool("push", agentrt.RemoteMutation)
	policy := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "k", Capability: json.RawMessage(`{"tool":`)}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, "")}}
	run, err := h.driver(agent, policy, push).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonInternalError)
	if approvals, _ := h.store.ListApprovals(context.Background(), run.ID); len(approvals) != 0 || len(push.calls) != 0 {
		t.Fatalf("approvals = %d calls = %d", len(approvals), len(push.calls))
	}
}

// A crash right after the run resumes must not drop the approved request:
// the resume, the resume-time policy decision, and the tool start commit
// together, so the next Resume finds the step executing or finished,
// never waiting with the run RUNNING.
func TestResume_CrashAfterResumeKeepsTheApprovedRequest(t *testing.T) {
	h := newHarness(t, ":memory:")
	push := echoTool("push", agentrt.RemoteMutation)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	ctx := context.Background()
	run, _ := h.driver(agent, agentrt.DefaultPolicy(), push).Start(ctx, "g", limits(10, 3))
	approvals, _ := h.store.ListApprovals(ctx, run.ID)
	if err := agentrt.Approve(ctx, h.store, nil, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	// The resume's policy evaluation is where the old code crashed after
	// committing run.resumed; cancelling there is the crash.
	cctx, cancel := context.WithCancel(ctx)
	crashing := agentrt.PolicyFunc(func(c context.Context, req agentrt.ToolRequest, v agentrt.RunView) (agentrt.PolicyDecision, error) {
		cancel()
		return agentrt.DefaultPolicy().Evaluate(c, req, v)
	})
	if _, err := h.driver(agent, crashing, push).Resume(cctx, run.ID); err == nil {
		t.Fatal("the cancelled resume reported success")
	}
	got, err := h.driver(agent, agentrt.DefaultPolicy(), push).Resume(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, got, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(push.calls) != 1 {
		t.Fatalf("approved request executed %d times", len(push.calls))
	}
	steps, _ := h.store.ListSteps(ctx, run.ID)
	if steps[0].Status != agentrt.StepDone || steps[0].Policy == nil || steps[0].Policy.Outcome != agentrt.RequireApproval {
		t.Fatalf("approved step = %+v policy %+v", steps[0], steps[0].Policy)
	}
	events, _ := h.store.ListEvents(ctx, run.ID)
	types := strings.Join(eventTypes(events), " ")
	if !strings.Contains(types, agentrt.EventRunResumed+" "+agentrt.EventStepPolicy+" "+agentrt.EventStepToolStarted) {
		t.Fatalf("resume events = %s", types)
	}
}

// A terminal tool's step and the run's completion commit together, so a
// crash after the step cannot leave the run RUNNING to re-run the tool.
func TestDriver_TerminalToolCommitsWithTheRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	pub := newTool("publish", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{"pr":1}`)}, nil
	})
	pub.spec.Terminal = true
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish", `{}`, ""), scripted.ToolCall("publish", `{}`, "")}}
	ctx, cancel := context.WithCancel(context.Background())
	d, _ := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{pub},
		Observer: func(e agentrt.Event) {
			if e.Type == agentrt.EventStepToolFinished {
				cancel() // the crash, right after the step's commit
			}
		}})
	d.Start(ctx, "g", limits(10, 3))
	runs, _ := h.store.ListRuns(context.Background())
	requireStatus(t, runs[0], agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if _, err := h.driver(agent, agentrt.DefaultPolicy(), pub).Resume(context.Background(), runs[0].ID); !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("resume of the completed run: %v", err)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("terminal tool ran %d times", len(pub.calls))
	}
}

// The run's active time is the sum of its step durations, including the
// step that ends it; finishing must not overwrite it with a stale copy.
func TestLimits_ActiveTimeSurvivesTheFinish(t *testing.T) {
	h := newHarness(t, ":memory:")
	var mu sync.Mutex
	at := time.Unix(1700000000, 0)
	tick := func() time.Time { mu.Lock(); defer mu.Unlock(); at = at.Add(time.Second); return at }
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	d, _ := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{echoTool("read", agentrt.ReadOnly)}, Now: tick})
	run, err := d.Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	var sum time.Duration
	for _, st := range steps {
		sum += st.FinishedAt.Sub(st.StartedAt)
	}
	stored, _ := h.store.GetRun(context.Background(), run.ID)
	if sum == 0 || stored.ActiveTime != sum || run.ActiveTime != sum {
		t.Fatalf("step durations %s, stored %s, returned %s", sum, stored.ActiveTime, run.ActiveTime)
	}
}

// A context cancelled while a tool runs does not lose what the tool did:
// the observation is recorded and the cancellation returned.
func TestDriver_CancelledContextAfterToolKeepsTheObservation(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx, cancel := context.WithCancel(context.Background())
	write := newTool("write", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		cancel()
		return agentrt.ToolResult{Content: []byte(`{"written":true}`), Summary: "wrote"}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("write", `{}`, ""), scripted.Complete(`{}`)}}
	d := h.driver(agent, agentrt.DefaultPolicy(), write)
	if _, err := d.Start(ctx, "g", limits(10, 3)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	runs, _ := h.store.ListRuns(context.Background())
	steps, _ := h.store.ListSteps(context.Background(), runs[0].ID)
	if steps[0].Status != agentrt.StepDone || steps[0].Observation == nil || string(steps[0].Observation.Content) != `{"written":true}` {
		t.Fatalf("step = %+v", steps[0])
	}
	run, err := d.Resume(context.Background(), runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(write.calls) != 1 {
		t.Fatalf("write ran %d times", len(write.calls))
	}
}

// Approve, Reject, Cancel, and expiry use the driver's clock.
func TestApproval_DriverClockDecidesExpiry(t *testing.T) {
	h := newHarness(t, ":memory:")
	c := &clock{now: time.Unix(1700000000, 0)} // long before the wall clock
	push := echoTool("push", agentrt.RemoteMutation)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	l := limits(20, 3)
	l.ApprovalTTL = 10 * time.Minute
	d := timedDriver(t, h, c, agent, push)
	ctx := context.Background()
	run, _ := d.Start(ctx, "g", l)
	approvals, _ := h.store.ListApprovals(ctx, run.ID)
	c.advance(5 * time.Minute)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatalf("approve within the TTL by the driver's clock: %v", err)
	}
	approvals, _ = h.store.ListApprovals(ctx, run.ID)
	if !approvals[0].DecidedAt.Equal(c.now) {
		t.Fatalf("decided at %s, want %s", approvals[0].DecidedAt, c.now)
	}
	if err := d.Cancel(ctx, run.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.GetRun(ctx, run.ID)
	if !got.FinishedAt.Equal(c.now) {
		t.Fatalf("cancelled at %s, want %s", got.FinishedAt, c.now)
	}
}

// The operator paths fail with typed errors.
func TestErrors_OperatorPathsAreTyped(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	d, run, a := pausedRun(t, h, push, read)
	ctx := context.Background()
	if err := d.Approve(ctx, run.ID, a.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.Approve(ctx, run.ID, a.ID, "joey", ""); !errors.Is(err, agentrt.ErrNotPending) || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("second approve: %v", err)
	}
	if err := d.Cancel(ctx, run.ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.Approve(ctx, run.ID, a.ID, "joey", ""); !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("approve on a cancelled run: %v", err)
	}
	if err := d.Cancel(ctx, run.ID, "joey", ""); !errors.Is(err, agentrt.ErrRunState) || !strings.Contains(err.Error(), "already CANCELLED") {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := d.Resume(ctx, run.ID); !errors.Is(err, agentrt.ErrRunState) {
		t.Fatalf("resume of a cancelled run: %v", err)
	}
}
