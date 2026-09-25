package agentrt_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// ---- test tools -----------------------------------------------------------

type fakeTool struct {
	spec  agentrt.ToolSpec
	call  func(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error)
	calls []agentrt.ToolCall
}

func (f *fakeTool) Spec() agentrt.ToolSpec { return f.spec }
func (f *fakeTool) Call(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	f.calls = append(f.calls, c)
	return f.call(ctx, c)
}

const numberSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`
const emptySchema = `{"type":"object","additionalProperties":false}`

func newTool(name string, se agentrt.SideEffect, schema string, fn func(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error)) *fakeTool {
	return &fakeTool{spec: agentrt.ToolSpec{Name: name, Description: name, InputSchema: []byte(schema), SideEffect: se, Timeout: 2 * time.Second}, call: fn}
}

func echoTool(name string, se agentrt.SideEffect) *fakeTool {
	return newTool(name, se, numberSchema, func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: c.Args, Summary: "echoed"}, nil
	})
}

type harness struct {
	t      *testing.T
	store  *agentrt.Store
	events []agentrt.Event
}

func newHarness(t *testing.T, dbPath string) *harness {
	t.Helper()
	st, err := agentrt.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &harness{t: t, store: st}
}

func (h *harness) driver(agent agentrt.Agent, policy agentrt.Policy, tools ...agentrt.Tool) *agentrt.Driver {
	h.t.Helper()
	d, err := agentrt.NewDriver(agentrt.Config{
		Store: h.store, Agent: agent, Policy: policy, Tools: tools,
		Observer: func(e agentrt.Event) { h.events = append(h.events, e) },
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func limits(maxSteps, maxFail int) agentrt.Limits {
	return agentrt.Limits{MaxSteps: maxSteps, MaxConsecutiveToolFailures: maxFail, LoopThreshold: 100}
}

func eventTypes(events []agentrt.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func requireStatus(t *testing.T, run agentrt.Run, status agentrt.RunStatus, reason agentrt.TerminalReason) {
	t.Helper()
	if run.Status != status || run.Reason != reason {
		t.Fatalf("run status=%s reason=%s detail=%q, want %s/%s", run.Status, run.Reason, run.ReasonDetail, status, reason)
	}
}

// ---- tests ----------------------------------------------------------------

func TestDriver_CompletesAndPersists(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look"),
		scripted.ToolCall("read", `{"n":2}`, "look again"),
		scripted.Complete(`{"answer":42}`),
	}}
	d := h.driver(agent, agentrt.DefaultPolicy(), read)

	run, err := d.Start(context.Background(), "test goal", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if string(run.Result) != `{"answer":42}` {
		t.Fatalf("result = %s", run.Result)
	}
	if run.StepCount != 3 {
		t.Fatalf("step count = %d", run.StepCount)
	}
	if len(read.calls) != 2 || read.calls[0].RunID != run.ID || read.calls[0].StepID == "" {
		t.Fatalf("tool did not receive driver identity: %+v", read.calls)
	}

	steps, err := h.store.ListSteps(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("persisted steps = %d", len(steps))
	}
	for i, st := range steps[:2] {
		if st.Status != agentrt.StepDone || st.Observation == nil || st.Observation.Kind != agentrt.ObserveToolResult {
			t.Fatalf("step %d = %+v", i, st)
		}
		if st.Policy == nil || st.Policy.Outcome != agentrt.Allow {
			t.Fatalf("step %d policy = %+v", i, st.Policy)
		}
		if st.Observation.ContentHash == "" || st.Index != i {
			t.Fatalf("step %d missing hash or wrong index", i)
		}
	}
	if steps[2].Decision.Kind != agentrt.DecideComplete || steps[2].Status != agentrt.StepDone {
		t.Fatalf("final step = %+v", steps[2])
	}

	events, err := h.store.ListEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(eventTypes(events), " ")
	want := strings.Join([]string{
		agentrt.EventRunCreated, agentrt.EventRunStarted,
		agentrt.EventStepStarted, agentrt.EventStepDecided, agentrt.EventStepPolicy, agentrt.EventStepToolStarted, agentrt.EventStepToolFinished,
		agentrt.EventStepStarted, agentrt.EventStepDecided, agentrt.EventStepPolicy, agentrt.EventStepToolStarted, agentrt.EventStepToolFinished,
		agentrt.EventStepStarted, agentrt.EventStepDecided,
		agentrt.EventRunFinished,
	}, " ")
	if got != want {
		t.Fatalf("event sequence:\n got  %s\n want %s", got, want)
	}
	if len(h.events) != len(events) {
		t.Fatalf("observer saw %d events, store has %d", len(h.events), len(events))
	}
	for i := range events {
		if events[i].Seq != h.events[i].Seq || events[i].Type != h.events[i].Type {
			t.Fatalf("observer event %d differs from store", i)
		}
	}
}

func TestDriver_InvalidArgumentsBecomeObservation(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":"not a number"}`, ""),
		scripted.ToolCall("read", `{"n":1,"extra":true}`, ""),
		scripted.ToolCall("nope", `{}`, ""),
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.Complete(`{}`),
	}}
	d := h.driver(agent, agentrt.DefaultPolicy(), read)
	run, err := d.Start(context.Background(), "g", limits(10, 5))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(read.calls) != 1 {
		t.Fatalf("tool called %d times, want 1 (invalid args must not reach the tool)", len(read.calls))
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	for i := 0; i < 3; i++ {
		if steps[i].Status != agentrt.StepFailed || steps[i].Observation.Kind != agentrt.ObserveInvalidDecision {
			t.Fatalf("step %d = %+v", i, steps[i])
		}
		if steps[i].Decision == nil || steps[i].Decision.Kind != agentrt.DecideToolCall {
			t.Fatalf("step %d must record the invalid decision verbatim", i)
		}
		if steps[i].Policy != nil {
			t.Fatalf("step %d: policy must not be evaluated on invalid arguments", i)
		}
	}
}

// The pseudo kinds a model agent records for a reply that asked for nothing
// executable must be invalid decisions: the step fails with an
// invalid_decision observation the next render nudges on, and no tool runs.
func TestDriver_PseudoDecisionKindsAreInvalid(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		{Kind: agentrt.KindNoToolCall, Reason: "narrating instead of acting"},
		{Kind: agentrt.KindTruncated, Reason: "cut off by the output cap"},
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.Complete(`{}`),
	}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", limits(10, 5))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	for i, kind := range []agentrt.DecisionKind{agentrt.KindNoToolCall, agentrt.KindTruncated} {
		if steps[i].Status != agentrt.StepFailed || steps[i].Observation.Kind != agentrt.ObserveInvalidDecision {
			t.Fatalf("step %d = %+v", i, steps[i])
		}
		if steps[i].Decision == nil || steps[i].Decision.Kind != kind {
			t.Fatalf("step %d must record %s verbatim", i, kind)
		}
		if steps[i].Policy != nil {
			t.Fatalf("step %d: policy must not be evaluated", i)
		}
	}
	if len(read.calls) != 1 {
		t.Fatalf("tool called %d times, want 1", len(read.calls))
	}
}

func TestDriver_PolicyDenyIsObservation(t *testing.T) {
	h := newHarness(t, ":memory:")
	wipe := echoTool("wipe", agentrt.Destructive)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("wipe", `{"n":1}`, ""),
		scripted.Complete(`{}`),
	}}
	d := h.driver(agent, agentrt.DefaultPolicy(), wipe)
	run, err := d.Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(wipe.calls) != 0 {
		t.Fatal("denied tool must not execute")
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	if steps[0].Observation.Kind != agentrt.ObservePolicyDenied || steps[0].Policy.Outcome != agentrt.Deny {
		t.Fatalf("step 0 = %+v", steps[0])
	}
}

func TestDriver_PolicyAbortEndsRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	tool := echoTool("edit", agentrt.LocalMutation)
	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{Outcome: agentrt.Abort, Reason: "hard limit"}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("edit", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, policy, tool).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonPolicyAbort)
	if run.ReasonDetail != "hard limit" || len(tool.calls) != 0 || run.StepCount != 1 {
		t.Fatalf("run = %+v calls=%d", run, len(tool.calls))
	}
}

func TestDriver_RequireApprovalPausesRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	push := echoTool("push", agentrt.RemoteMutation)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":7}`, "publish")}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), push).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval || run.Reason != "" {
		t.Fatalf("run = %+v", run)
	}
	if len(push.calls) != 0 {
		t.Fatal("tool must not run before approval")
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	if steps[0].Status != agentrt.StepAwaitingApproval || steps[0].Policy.Kind != string(agentrt.RemoteMutation) {
		t.Fatalf("step = %+v", steps[0])
	}
	events, _ := h.store.ListEvents(context.Background(), run.ID)
	last := events[len(events)-1]
	if last.Type != agentrt.EventApprovalRequested {
		t.Fatalf("last event = %s", last.Type)
	}
	var payload struct {
		Kind       string          `json:"kind"`
		Capability json.RawMessage `json:"capability"`
	}
	if err := json.Unmarshal(last.Payload, &payload); err != nil || payload.Kind != "remote_mutation" || !strings.Contains(string(payload.Capability), `"n":7`) {
		t.Fatalf("approval payload = %s", last.Payload)
	}
}

func TestDriver_StepLimit(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	decisions := make([]agentrt.Decision, 0, 10)
	for i := 0; i < 10; i++ {
		decisions = append(decisions, scripted.ToolCall("read", `{"n":1}`, ""))
	}
	run, err := h.driver(&scripted.Agent{Decisions: decisions}, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", limits(4, 10))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitSteps)
	if run.StepCount != 4 || len(read.calls) != 4 {
		t.Fatalf("steps=%d calls=%d", run.StepCount, len(read.calls))
	}
	events, _ := h.store.ListEvents(context.Background(), run.ID)
	types := eventTypes(events)
	if types[len(types)-2] != agentrt.EventLimitExceeded || types[len(types)-1] != agentrt.EventRunFinished {
		t.Fatalf("tail events = %v", types[len(types)-3:])
	}
}

func TestDriver_ConsecutiveFailuresEndRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	boom := newTool("boom", agentrt.ReadOnly, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{}, errors.New("kaboom")
	})
	read := echoTool("read", agentrt.ReadOnly)
	// One success, then three failures of mixed kinds: tool error, invalid args, tool error.
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("boom", `{}`, ""),
		scripted.ToolCall("read", `{"n":"bad"}`, ""),
		scripted.ToolCall("boom", `{}`, ""),
		scripted.Complete(`{}`),
	}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), boom, read).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonRepeatedToolFailures)
	if run.StepCount != 4 {
		t.Fatalf("step count = %d, want 4 (run ends before a fifth step)", run.StepCount)
	}
}

func TestDriver_FailureStreakResetsOnSuccess(t *testing.T) {
	h := newHarness(t, ":memory:")
	boom := newTool("boom", agentrt.ReadOnly, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{}, errors.New("kaboom")
	})
	read := echoTool("read", agentrt.ReadOnly)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("boom", `{}`, ""), scripted.ToolCall("boom", `{}`, ""),
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("boom", `{}`, ""), scripted.ToolCall("boom", `{}`, ""),
		scripted.Complete(`{}`),
	}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), boom, read).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
}

func TestDriver_ToolTimeout(t *testing.T) {
	h := newHarness(t, ":memory:")
	slow := &fakeTool{spec: agentrt.ToolSpec{Name: "slow", Description: "slow", InputSchema: []byte(emptySchema), SideEffect: agentrt.ReadOnly, Timeout: 50 * time.Millisecond},
		call: func(ctx context.Context, _ agentrt.ToolCall) (agentrt.ToolResult, error) {
			select {
			case <-ctx.Done():
				return agentrt.ToolResult{}, ctx.Err()
			case <-time.After(5 * time.Second):
				return agentrt.ToolResult{Content: []byte(`{}`)}, nil
			}
		}}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("slow", `{}`, ""), scripted.Complete(`{}`)}}
	start := time.Now()
	run, err := h.driver(agent, agentrt.DefaultPolicy(), slow).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout was not enforced")
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	if steps[0].Observation.Kind != agentrt.ObserveToolError || !strings.Contains(string(steps[0].Observation.Content), `"timeout"`) {
		t.Fatalf("observation = %+v", steps[0].Observation)
	}
}

func TestDriver_ToolPanicIsObserved(t *testing.T) {
	h := newHarness(t, ":memory:")
	bad := newTool("bad", agentrt.ReadOnly, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) { panic("oops") })
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("bad", `{}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), bad).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	if steps[0].Observation.Kind != agentrt.ObserveToolError || !strings.Contains(steps[0].Observation.Summary, "panicked") {
		t.Fatalf("observation = %+v", steps[0].Observation)
	}
}

func TestDriver_AgentErrorAndFailDecision(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	// Script exhausted after one step: agent error.
	run, err := h.driver(&scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, "")}}, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonAgentError)

	run, err = h.driver(&scripted.Agent{Decisions: []agentrt.Decision{scripted.Fail("cannot do it")}}, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonGoalFailed)
	if run.ReasonDetail != "cannot do it" {
		t.Fatalf("detail = %q", run.ReasonDetail)
	}
}

func TestNewDriver_RejectsBadTools(t *testing.T) {
	h := newHarness(t, ":memory:")
	agent := &scripted.Agent{}
	cases := map[string]agentrt.ToolSpec{
		"missing timeout":   {Name: "a", InputSchema: []byte(emptySchema), SideEffect: agentrt.ReadOnly},
		"bad side effect":   {Name: "a", InputSchema: []byte(emptySchema), SideEffect: "whatever", Timeout: time.Second},
		"missing schema":    {Name: "a", SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		"schema not json":   {Name: "a", InputSchema: []byte(`{`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		"schema not schema": {Name: "a", InputSchema: []byte(`{"type":"nonsense"}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
	}
	for name, spec := range cases {
		tool := &fakeTool{spec: spec}
		if _, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{tool}}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	dup := echoTool("a", agentrt.ReadOnly)
	if _, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{dup, dup}}); err == nil {
		t.Error("duplicate tool: expected error")
	}
}

func TestStore_ReopenFileBacked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runs.db")
	var runID string
	var wantEvents int
	{
		h := newHarness(t, dbPath)
		push := echoTool("push", agentrt.RemoteMutation)
		read := echoTool("read", agentrt.ReadOnly)
		agent := &scripted.Agent{Decisions: []agentrt.Decision{
			scripted.ToolCall("read", `{"n":1}`, "inspect"),
			scripted.ToolCall("push", `{"n":2}`, "publish"),
		}}
		run, err := h.driver(agent, agentrt.DefaultPolicy(), push, read).Start(context.Background(), "durable goal", limits(10, 3))
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != agentrt.StatusWaitingForApproval {
			t.Fatalf("status = %s", run.Status)
		}
		runID = run.ID
		wantEvents = len(h.events)
		if err := h.store.Close(); err != nil {
			t.Fatal(err)
		}
	}

	st, err := agentrt.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval || run.Goal != "durable goal" || run.StepCount != 2 || run.Limits.MaxSteps != 10 {
		t.Fatalf("reopened run = %+v", run)
	}
	steps, err := st.ListSteps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Status != agentrt.StepDone || steps[1].Status != agentrt.StepAwaitingApproval {
		t.Fatalf("reopened steps = %+v", steps)
	}
	if steps[0].Observation == nil || steps[0].Observation.ContentHash == "" || steps[1].Decision.Reason != "publish" {
		t.Fatalf("reopened step detail lost: %+v", steps)
	}
	events, err := st.ListEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != wantEvents {
		t.Fatalf("events after reopen = %d, want %d", len(events), wantEvents)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatal("event sequence not monotonic")
		}
	}
	runs, err := st.ListRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].ID != runID {
		t.Fatalf("ListRuns = %+v, %v", runs, err)
	}
	if _, err := st.GetRun(context.Background(), "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("missing run error = %v", err)
	}
}

// The same call producing the same observation three times is a loop.
func TestDriver_IdenticalCallAndResultStops(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	decisions := make([]agentrt.Decision, 0, 10)
	for i := 0; i < 10; i++ {
		decisions = append(decisions, scripted.ToolCall("read", `{"n":1}`, ""))
	}
	l := limits(20, 10)
	l.LoopThreshold = 3
	run, err := h.driver(&scripted.Agent{Decisions: decisions}, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLoopDetected)
	if run.StepCount != 3 || len(read.calls) != 3 {
		t.Fatalf("steps=%d calls=%d, want 3 (run ends before a fourth identical step)", run.StepCount, len(read.calls))
	}
	events, _ := h.store.ListEvents(context.Background(), run.ID)
	types := eventTypes(events)
	if types[len(types)-2] != agentrt.EventLoopDetected {
		t.Fatalf("tail events = %v", types[len(types)-3:])
	}
}

// Repeating a call whose result changes is progress, not a loop. This is the
// shape of repeated validation after different edits.
func TestDriver_RepeatedCallWithChangingResultAllowed(t *testing.T) {
	h := newHarness(t, ":memory:")
	counter := 0
	validate := newTool("validate", agentrt.ReadOnly, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		counter++
		return agentrt.ToolResult{Content: []byte(fmt.Sprintf(`{"tree":"t%d","findings":%d}`, counter, 10-counter)), Summary: "validated"}, nil
	})
	decisions := make([]agentrt.Decision, 0, 6)
	for i := 0; i < 5; i++ {
		decisions = append(decisions, scripted.ToolCall("validate", `{}`, ""))
	}
	decisions = append(decisions, scripted.Complete(`{}`))
	l := limits(20, 10)
	l.LoopThreshold = 3
	run, err := h.driver(&scripted.Agent{Decisions: decisions}, agentrt.DefaultPolicy(), validate).Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if counter != 5 {
		t.Fatalf("validate ran %d times, want 5", counter)
	}
}

// A different observation between identical calls resets the streak.
func TestDriver_LoopStreakResets(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	decisions := []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("read", `{"n":2}`, ""),
		scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.Complete(`{}`),
	}
	l := limits(20, 10)
	l.LoopThreshold = 3
	run, err := h.driver(&scripted.Agent{Decisions: decisions}, agentrt.DefaultPolicy(), read).Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
}

func TestDriver_StartWithID(t *testing.T) {
	h := newHarness(t, ":memory:")
	read := echoTool("read", agentrt.ReadOnly)
	d := h.driver(&scripted.Agent{Decisions: []agentrt.Decision{scripted.Complete(`{}`)}}, agentrt.DefaultPolicy(), read)
	run, err := d.StartWithID(context.Background(), "task-42", "g", limits(5, 3))
	if err != nil || run.ID != "task-42" {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if _, err := d.StartWithID(context.Background(), "task-42", "g", limits(5, 3)); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := d.StartWithID(context.Background(), "", "g", limits(5, 3)); err == nil {
		t.Fatal("empty id accepted")
	}
}
