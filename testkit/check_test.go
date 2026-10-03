package testkit

import (
	"encoding/json"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

// recorder keeps what check reports.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, strings.TrimPrefix(format, "testkit: "))
}
func (r *recorder) Helper() {}

func event(seq int64, typ, stepID string, payload string) agentrt.Event {
	return agentrt.Event{Seq: seq, RunID: "r", StepID: stepID, Type: typ, Payload: json.RawMessage(payload)}
}

// The invariant checks fire on a record the runtime must never produce:
// a step executed twice with no approval, an approval executed twice, a
// tool start never closed, a run left running, and a lost crash.
func TestCheck_ReportsEveryBrokenInvariant(t *testing.T) {
	r := &recorder{TB: t}
	h := &harness{t: r, crashes: []Crash{{Step: 0, At: AfterToolEffect}, {Step: 1, At: AfterToolEffect}}}
	res := Result{
		RunID: "r",
		Run:   agentrt.Run{ID: "r", Status: agentrt.StatusRunning},
		Steps: []agentrt.Step{
			{ID: "s0", Index: 0, Status: agentrt.StepDone},
			{ID: "s1", Index: 1, Status: agentrt.StepExecuting},
			{ID: "s2", Index: 2, Status: agentrt.StepAwaitingApproval},
		},
		Approvals: []agentrt.Approval{{ID: "a1", StepID: "s1", Status: agentrt.ApprovalApproved}},
		Events: []agentrt.Event{
			event(1, agentrt.EventStepToolStarted, "s0", `{"tool":"w"}`),
			event(2, agentrt.EventStepToolFinished, "s0", `{}`),
			event(3, agentrt.EventStepToolStarted, "s0", `{"tool":"w"}`),
			event(4, agentrt.EventStepToolFinished, "s0", `{}`),
			event(5, agentrt.EventStepToolStarted, "s1", `{"tool":"w","approval_id":"a1"}`),
			event(6, agentrt.EventStepToolStarted, "s1", `{"tool":"w","approval_id":"a1"}`),
		},
		Calls:   []Call{{Step: 0, Tool: "w"}, {Step: 0, Tool: "w"}, {Step: 2, Tool: "w"}},
		Crashes: []Crash{{Step: 0, At: AfterToolEffect}},
	}
	h.check(res)
	got := strings.Join(r.errors, "\n")
	for _, want := range []string{
		"crash %s was never reached",
		"run %s is left %s",
		"step %d is left %s",
		"step %d is awaiting approval under a %s run",
		"step %d started a tool again before the previous start was finished or interrupted",
		"step %d has a tool start with no finish and no interruption",
		"step %d executed %d times with %d approval(s)",
		"approval %s executed %d times",
		"step %d reached its tool %d time(s) but recorded %d start(s)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(r.errors) != 9 {
		t.Errorf("%d failures, want 9:\n%s", len(r.errors), got)
	}
}

// A clean record, with a re-execution under an approval, passes.
func TestCheck_AcceptsAnApprovedReExecution(t *testing.T) {
	r := &recorder{TB: t}
	h := &harness{t: r, crashes: []Crash{{Step: 0, At: AfterToolEffect}}}
	res := Result{
		RunID:     "r",
		Run:       agentrt.Run{ID: "r", Status: agentrt.StatusCompleted},
		Steps:     []agentrt.Step{{ID: "s0", Index: 0, Status: agentrt.StepDone}},
		Approvals: []agentrt.Approval{{ID: "a0", StepID: "s0", Kind: agentrt.InterruptedSideEffect, Status: agentrt.ApprovalApproved}},
		Events: []agentrt.Event{
			event(1, agentrt.EventStepToolStarted, "s0", `{"tool":"w"}`),
			event(2, agentrt.EventStepInterrupted, "s0", `{}`),
			event(3, agentrt.EventStepToolStarted, "s0", `{"tool":"w","approval_id":"a0"}`),
			event(4, agentrt.EventStepToolFinished, "s0", `{}`),
		},
		Calls:   []Call{{Step: 0, Tool: "w"}, {Step: 0, Tool: "w", Returned: true}},
		Crashes: []Crash{{Step: 0, At: AfterToolEffect}},
	}
	h.check(res)
	if len(r.errors) != 0 {
		t.Fatalf("failures: %v", r.errors)
	}
}
