package view_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/view"
)

type tool struct {
	spec agentrt.ToolSpec
}

func (t tool) Spec() agentrt.ToolSpec { return t.spec }
func (t tool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return agentrt.ToolResult{Content: c.Args, Summary: "echoed " + t.spec.Name}, nil
}

func newTool(name string, se agentrt.SideEffect) tool {
	return tool{spec: agentrt.ToolSpec{Name: name, Description: name, InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`), SideEffect: se, Timeout: time.Second}}
}

// pausedRun drives the scripted agent against a real file-backed store until
// the run parks on an approval, which is the state every operator surface has
// to render.
func pausedRun(t *testing.T) (*agentrt.Store, string) {
	t.Helper()
	store, err := agentrt.OpenStore(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look first"),
		scripted.ToolCall("wipe", `{}`, "denied by policy"),
		scripted.ToolCall("push", `{"n":7}`, "needs approval"),
		scripted.Complete(`{"done":true}`),
	}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(),
		Tools: []agentrt.Tool{newTool("read", agentrt.ReadOnly), newTool("push", agentrt.RemoteMutation), newTool("wipe", agentrt.Destructive)}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "bring the counter to 5", agentrt.Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 5})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", run.Status)
	}
	return store, run.ID
}

func TestRuns_SummariesCarryTheWaitingApproval(t *testing.T) {
	store, runID := pausedRun(t)
	ctx := context.Background()
	runs, err := view.Runs(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	got := runs[0]
	if got.ID != runID || got.Goal != "bring the counter to 5" || got.Status != agentrt.StatusWaitingForApproval || got.Steps != 3 {
		t.Fatalf("summary = %+v", got)
	}
	if got.PendingApprovalID == "" || got.CreatedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Fatalf("summary = %+v", got)
	}
	one, err := view.Summary(ctx, store, runID)
	if err != nil || one != got {
		t.Fatalf("Summary = %+v, %v, want the same row as Runs", one, err)
	}
	if _, err := view.Summary(ctx, store, "nope"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSummary_TerminalRunHasNoPendingApproval(t *testing.T) {
	store, runID := pausedRun(t)
	ctx := context.Background()
	a, err := view.PendingApproval(ctx, store, runID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := agentrt.Reject(ctx, store, nil, runID, a.ID, "joey", "not now"); err != nil {
		t.Fatal(err)
	}
	got, err := view.Summary(ctx, store, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentrt.StatusCancelled || got.Reason != agentrt.ReasonApprovalRejected || got.ReasonDetail != "not now" {
		t.Fatalf("summary = %+v", got)
	}
	if got.PendingApprovalID != "" || got.FinishedAt.IsZero() {
		t.Fatalf("summary = %+v", got)
	}
}

func TestSteps_SummarizeDecisionPolicyAndObservation(t *testing.T) {
	store, runID := pausedRun(t)
	steps, err := view.Steps(context.Background(), store, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %+v", steps)
	}
	if s := steps[0]; s.Index != 0 || s.Tool != "read" || s.Decision != agentrt.DecideToolCall || s.Status != agentrt.StepDone ||
		s.Policy != agentrt.Allow || s.Observation != agentrt.ObserveToolResult || s.Summary != "echoed read" {
		t.Fatalf("step 0 = %+v", s)
	}
	if s := steps[1]; s.Tool != "wipe" || s.Status != agentrt.StepFailed || s.Policy != agentrt.Deny || s.Observation != agentrt.ObservePolicyDenied || !strings.HasPrefix(s.Summary, "denied:") {
		t.Fatalf("step 1 = %+v", s)
	}
	if s := steps[2]; s.Tool != "push" || s.Status != agentrt.StepAwaitingApproval || s.Policy != agentrt.RequireApproval || s.Observation != "" {
		t.Fatalf("step 2 = %+v", s)
	}
	if _, err := json.Marshal(steps); err != nil {
		t.Fatal(err)
	}
}

func TestPendingApproval_SelectionRule(t *testing.T) {
	store, runID := pausedRun(t)
	ctx := context.Background()
	only, err := view.PendingApproval(ctx, store, runID, "")
	if err != nil {
		t.Fatal(err)
	}
	if only.Kind != "remote_mutation" || only.Request.Spec.Name != "push" {
		t.Fatalf("approval = %+v", only)
	}
	byID, err := view.PendingApproval(ctx, store, runID, only.ID)
	if err != nil || byID.ID != only.ID {
		t.Fatalf("by id = %+v, %v", byID, err)
	}
	if _, err := view.PendingApproval(ctx, store, runID, "missing"); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	// A second pending approval makes the choice the operator's: the error
	// names both candidates rather than picking one.
	second := only.ID + "-2"
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, expires_at)
		SELECT ?, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, expires_at FROM approvals WHERE id = ?`, second, only.ID); err != nil {
		t.Fatal(err)
	}
	_, err = view.PendingApproval(ctx, store, runID, "")
	if err == nil || !strings.Contains(err.Error(), "2 pending approvals") || !strings.Contains(err.Error(), second) || !strings.Contains(err.Error(), only.ID) {
		t.Fatalf("err = %v, want both candidates named", err)
	}
	if got, err := view.Summary(ctx, store, runID); err != nil || got.PendingApprovalID != "" {
		t.Fatalf("summary = %+v, %v: an ambiguous run must not name one approval", got, err)
	}

	// A decided approval is not a candidate, by id or by rule.
	for _, id := range []string{only.ID, second} {
		if _, err := store.DB().ExecContext(ctx, `UPDATE approvals SET status = ? WHERE id = ?`, agentrt.ApprovalApproved, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := view.PendingApproval(ctx, store, runID, only.ID); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("err = %v", err)
	}
	if _, err := view.PendingApproval(ctx, store, runID, ""); err == nil || !strings.Contains(err.Error(), "no pending approval") {
		t.Fatalf("err = %v", err)
	}
}
