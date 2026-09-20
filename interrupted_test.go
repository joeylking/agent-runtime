package agentrt

import (
	"context"
	"testing"
	"time"
)

// listAgent replays decisions by step index without importing the scripted
// package, which would create an import cycle in an internal test.
type listAgent struct{ decisions []Decision }

func (a *listAgent) Decide(_ context.Context, in StepInput) (Decision, error) {
	return a.decisions[len(in.Steps)], nil
}

// A run left RUNNING with a step in executing status is what a crashed
// process leaves behind. Resume must fail that step with an interrupted
// observation, run the consumer's reconciliation, and continue.
func TestResume_InterruptedStepIsReconciledAndContinued(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	run := Run{ID: "r1", Goal: "g", Status: StatusRunning, Limits: Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, StepCount: 1, CreatedAt: now, StartedAt: now}
	crashed := Step{ID: "s0", RunID: "r1", Index: 0, Status: StepExecuting, Decision: &Decision{Kind: DecideToolCall, Tool: "write", Args: []byte(`{}`)}, Policy: &PolicyDecision{Outcome: Allow}, StartedAt: now}
	if err := store.tx(ctx, nil, func(tx *txn) error {
		if err := tx.insertRun(ctx, run); err != nil {
			return err
		}
		return tx.insertStep(ctx, crashed)
	}); err != nil {
		t.Fatal(err)
	}

	reconciled := false
	write := &stubTool{spec: ToolSpec{Name: "write", Description: "w", InputSchema: []byte(`{"type":"object"}`), SideEffect: LocalMutation, Timeout: time.Second}}
	agent := &listAgent{decisions: []Decision{
		{Kind: DecideToolCall, Tool: "write", Args: []byte(`{}`)}, // never used: index 0 already exists
		{Kind: DecideComplete, Result: []byte(`{"ok":true}`)},
	}}
	d, err := NewDriver(Config{Store: store, Agent: agent, Policy: DefaultPolicy(), Tools: []Tool{write},
		Reconcile: func(_ context.Context, view RunView) (Reconciliation, error) {
			reconciled = true
			if len(view.Steps) != 1 || view.Steps[0].Status != StepInterrupted {
				t.Errorf("reconcile saw %+v", view.Steps)
			}
			return Reconciliation{Outcome: ReconcileContinue}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Resume(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled {
		t.Fatal("reconcile was not called")
	}
	if got.Status != StatusCompleted {
		t.Fatalf("status = %s (%s: %s)", got.Status, got.Reason, got.ReasonDetail)
	}
	if write.calls != 0 {
		t.Fatal("the interrupted side-effecting step was re-executed")
	}
	steps, _ := store.ListSteps(ctx, "r1")
	if steps[0].Status != StepInterrupted || steps[0].Observation.Kind != ObserveInterrupted {
		t.Fatalf("step 0 = %+v", steps[0])
	}
	if steps[1].Decision.Kind != DecideComplete {
		t.Fatalf("step 1 = %+v", steps[1])
	}
	events, _ := store.ListEvents(ctx, "r1")
	found := false
	for _, e := range events {
		if e.Type == EventStepInterrupted {
			found = true
		}
	}
	if !found {
		t.Fatal("no step.interrupted event")
	}
}

type stubTool struct {
	spec  ToolSpec
	calls int
}

func (s *stubTool) Spec() ToolSpec { return s.spec }
func (s *stubTool) Call(context.Context, ToolCall) (ToolResult, error) {
	s.calls++
	return ToolResult{Content: []byte(`{}`)}, nil
}

func interruptedRun(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	run := Run{ID: "r1", Goal: "g", Status: StatusRunning, Limits: Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, StepCount: 1, CreatedAt: now, StartedAt: now}
	crashed := Step{ID: "s0", RunID: "r1", Index: 0, Status: StepExecuting, Decision: &Decision{Kind: DecideToolCall, Tool: "publish", Args: []byte(`{}`)}, Policy: &PolicyDecision{Outcome: Allow}, StartedAt: now}
	if err := store.tx(ctx, nil, func(tx *txn) error {
		if err := tx.insertRun(ctx, run); err != nil {
			return err
		}
		return tx.insertStep(ctx, crashed)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResume_ReconcileOutcomes(t *testing.T) {
	cases := map[string]struct {
		rec    Reconciliation
		status RunStatus
		reason TerminalReason
	}{
		"completed": {Reconciliation{Outcome: ReconcileCompleted, Result: []byte(`{"pr":1}`), Detail: "already published"}, StatusCompleted, ReasonGoalCompleted},
		"conflict":  {Reconciliation{Outcome: ReconcileConflict, Detail: "remote ref differs"}, StatusFailed, ReasonReconcileConflict},
		"waiting":   {Reconciliation{Outcome: ReconcileWaiting}, StatusWaitingForApproval, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, _ := OpenStore(":memory:")
			defer store.Close()
			interruptedRun(t, store)
			publish := &stubTool{spec: ToolSpec{Name: "publish", Description: "p", InputSchema: []byte(`{"type":"object"}`), SideEffect: RemoteMutation, Timeout: time.Second}}
			agent := &listAgent{decisions: []Decision{{}, {Kind: DecideComplete, Result: []byte(`{}`)}}}
			d, err := NewDriver(Config{Store: store, Agent: agent, Policy: DefaultPolicy(), Tools: []Tool{publish},
				Reconcile: func(context.Context, RunView) (Reconciliation, error) { return tc.rec, nil }})
			if err != nil {
				t.Fatal(err)
			}
			got, err := d.Resume(context.Background(), "r1")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("run = %s/%s, want %s/%s", got.Status, got.Reason, tc.status, tc.reason)
			}
			if tc.rec.Outcome == ReconcileCompleted && string(got.Result) != `{"pr":1}` {
				t.Fatalf("result = %s", got.Result)
			}
			if publish.calls != 0 {
				t.Fatal("interrupted remote step was re-executed")
			}
		})
	}
}
