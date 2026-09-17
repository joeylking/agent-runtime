package agentrt_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

func pausedRun(t *testing.T, h *harness, tools ...agentrt.Tool) (*agentrt.Driver, agentrt.Run, agentrt.Approval) {
	t.Helper()
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("push", `{"n":7}`, "publish"),
		scripted.ToolCall("read", `{"n":1}`, "after"),
		scripted.Complete(`{"done":true}`),
	}}
	d := h.driver(agent, agentrt.DefaultPolicy(), tools...)
	run, err := d.Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", run.Status)
	}
	approvals, err := h.store.ListApprovals(context.Background(), run.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals = %+v, %v", approvals, err)
	}
	return d, run, approvals[0]
}

func TestApproval_RecordedWithHash(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	_, run, a := pausedRun(t, h, push, read)
	if a.Status != agentrt.ApprovalPending || a.Kind != "remote_mutation" || a.RunID != run.ID || a.Hash == "" {
		t.Fatalf("approval = %+v", a)
	}
	if a.Request.Spec.Name != "push" || string(a.Request.Args) != `{"n":7}` {
		t.Fatalf("recorded request = %+v", a.Request)
	}
	if len(push.calls) != 0 {
		t.Fatal("tool ran before approval")
	}
}

func TestApproval_ApproveThenResumeExecutesRecordedRequest(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	d, run, a := pausedRun(t, h, push, read)
	if err := d.Approve(context.Background(), run.ID, a.ID, "joey", "looks good"); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.store.GetRun(context.Background(), run.ID); r.Status != agentrt.StatusWaitingForApproval {
		t.Fatal("approve must not resume by itself")
	}
	got, err := d.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, got, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(push.calls) != 1 || string(push.calls[0].Args) != `{"n":7}` || push.calls[0].StepID != a.StepID {
		t.Fatalf("push calls = %+v", push.calls)
	}
	if len(read.calls) != 1 {
		t.Fatal("loop did not continue after the approved step")
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	if steps[0].Status != agentrt.StepDone || steps[0].Observation.Kind != agentrt.ObserveToolResult {
		t.Fatalf("approved step = %+v", steps[0])
	}
	approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
	if approvals[0].Status != agentrt.ApprovalApproved || approvals[0].DecidedBy != "joey" {
		t.Fatalf("approval = %+v", approvals[0])
	}
	events, _ := h.store.ListEvents(context.Background(), run.ID)
	types := strings.Join(eventTypes(events), " ")
	for _, want := range []string{agentrt.EventApprovalRequested, agentrt.EventApprovalDecided, agentrt.EventRunResumed} {
		if !strings.Contains(types, want) {
			t.Fatalf("missing event %s in %s", want, types)
		}
	}
	if _, err := d.Resume(context.Background(), run.ID); err == nil {
		t.Fatal("resuming a completed run must fail")
	}
}

func TestApproval_RejectCancelsRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	d, run, a := pausedRun(t, h, push, read)
	if err := d.Reject(context.Background(), run.ID, a.ID, "joey", "not now"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.store.GetRun(context.Background(), run.ID)
	requireStatus(t, got, agentrt.StatusCancelled, agentrt.ReasonApprovalRejected)
	if len(push.calls) != 0 {
		t.Fatal("rejected tool ran")
	}
	if err := d.Approve(context.Background(), run.ID, a.ID, "joey", ""); err == nil {
		t.Fatal("approving after rejection must fail")
	}
}

func TestApproval_TamperedRowRefused(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	d, run, a := pausedRun(t, h, push, read)
	// Change the recorded request behind the runtime's back.
	if _, err := h.store.DB().Exec(`UPDATE approvals SET request_json = replace(request_json, '"n":7', '"n":99') WHERE id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Approve(context.Background(), run.ID, a.ID, "joey", ""); !errors.Is(err, agentrt.ErrApprovalHash) {
		t.Fatalf("err = %v, want ErrApprovalHash", err)
	}
	if len(push.calls) != 0 {
		t.Fatal("tampered request executed")
	}
}

func TestApproval_ResumeRepausesWhenPolicyWantsDifferentApproval(t *testing.T) {
	h := newHarness(t, ":memory:")
	push, read := echoTool("push", agentrt.RemoteMutation), echoTool("read", agentrt.ReadOnly)
	generation := 0
	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.SideEffect != agentrt.RemoteMutation {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
		}
		generation++
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "publication", Capability: json.RawMessage(`{"proposal":"p1"}`), Presentation: json.RawMessage(`{"generation":` + string(rune('0'+generation)) + `}`)}, nil
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":1}`, ""), scripted.Complete(`{}`)}}
	d := h.driver(agent, policy, push, read)
	run, _ := d.Start(context.Background(), "g", limits(10, 3))
	approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
	d.Approve(context.Background(), run.ID, approvals[0].ID, "joey", "")
	got, err := d.Resume(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s, want a second pause because the presentation changed", got.Status)
	}
	approvals, _ = h.store.ListApprovals(context.Background(), run.ID)
	if len(approvals) != 2 || approvals[1].Status != agentrt.ApprovalPending || len(push.calls) != 0 {
		t.Fatalf("approvals = %+v calls = %d", approvals, len(push.calls))
	}
}

func TestDriver_TerminalToolCompletesRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	fin := newTool("finish", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{"proposal":"p1"}`), Summary: "frozen"}, nil
	})
	fin.spec.Terminal = true
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("finish", `{}`, ""), scripted.ToolCall("finish", `{}`, "must not run")}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), fin).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if string(run.Result) != `{"proposal":"p1"}` || run.StepCount != 1 || len(fin.calls) != 1 {
		t.Fatalf("run = %+v calls = %d", run, len(fin.calls))
	}
}

func TestDriver_ToolAbortEndsRun(t *testing.T) {
	h := newHarness(t, ":memory:")
	bad := newTool("publish", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{}, agentrt.ErrAbortRun{Detail: "proposal invalidated"}
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish", `{}`, ""), scripted.Complete(`{}`)}}
	run, err := h.driver(agent, agentrt.DefaultPolicy(), bad).Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonToolAbort)
	if run.ReasonDetail != "proposal invalidated" {
		t.Fatalf("detail = %q", run.ReasonDetail)
	}
}
