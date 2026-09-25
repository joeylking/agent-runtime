package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// pausedDB drives the scripted agent against a real database until the run
// parks on an approval, then closes the store so the command opens it as an
// operator would.
func pausedDB(t *testing.T) (path, runID, approvalID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "runs.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look first"),
		scripted.ToolCall("push", `{"n":7}`, "needs approval"),
		scripted.Complete(`{"done":true}`),
	}}
	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.SideEffect != agentrt.RemoteMutation {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "read only"}, nil
		}
		return agentrt.NeedApproval("publication", "pushing is a remote mutation",
			map[string]any{"tool": req.Spec.Name}, map[string]any{"proposal_id": "p1", "files": 2})
	})
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: policy,
		Tools: []agentrt.Tool{newTool("read", agentrt.ReadOnly), newTool("push", agentrt.RemoteMutation)}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "publish the upgrade", agentrt.Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 5})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", run.Status)
	}
	approvals, err := store.ListApprovals(context.Background(), run.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals = %+v, %v", approvals, err)
	}
	return path, run.ID, approvals[0].ID
}

// exec runs the command and returns its status and streams.
func exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(args, &out, &errw)
	return code, out.String(), errw.String()
}

func requireOK(t *testing.T, code int, out, errw string) {
	t.Helper()
	if code != exitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errw)
	}
}

func TestRuns_ListsAndEmitsJSON(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "runs")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "RUN") || !strings.Contains(out, runID) ||
		!strings.Contains(out, string(agentrt.StatusWaitingForApproval)) || !strings.Contains(out, approvalID) {
		t.Fatalf("runs output:\n%s", out)
	}

	code, out, errw = exec(t, "-db", path, "-json", "runs")
	requireOK(t, code, out, errw)
	var rows []view.RunSummary
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].ID != runID || rows[0].PendingApprovalID != approvalID || rows[0].Steps != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestRuns_ReadsTheDatabaseFromTheEnvironment(t *testing.T) {
	path, runID, _ := pausedDB(t)
	t.Setenv("AGENTRT_DB", path)
	code, out, errw := exec(t, "runs")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, runID) {
		t.Fatalf("runs output:\n%s", out)
	}
	t.Setenv("AGENTRT_DB", "")
	if code, _, errw := exec(t, "runs"); code != exitUsage || !strings.Contains(errw, "no database") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
}

func TestShow_PutsTheWaitingApprovalInFront(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "show", runID)
	requireOK(t, code, out, errw)
	for _, want := range []string{
		"run     " + runID,
		"goal    publish the upgrade",
		"APPROVAL WAITING  " + approvalID,
		"kind    publication",
		`"proposal_id": "p1"`,
		"decide with: agentrt -db " + path + " approve " + runID,
		"STEP", "read", "push", "awaiting_approval", "require_approval",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
	// The presentation is pretty-printed, not a single line of JSON.
	if !strings.Contains(out, "presentation\n  {\n") {
		t.Fatalf("presentation was not pretty-printed:\n%s", out)
	}

	code, out, errw = exec(t, "-db", path, "-json", "show", runID)
	requireOK(t, code, out, errw)
	var payload struct {
		Run     view.RunSummary    `json:"run"`
		Steps   []view.StepSummary `json:"steps"`
		Waiting *agentrt.Approval  `json:"waiting_approval"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	if payload.Run.ID != runID || len(payload.Steps) != 2 || payload.Waiting == nil || payload.Waiting.ID != approvalID {
		t.Fatalf("payload = %+v", payload)
	}
	if got, err := agentrt.DecodePresentation[struct {
		ProposalID string `json:"proposal_id"`
	}](*payload.Waiting); err != nil || got.ProposalID != "p1" {
		t.Fatalf("presentation = %+v, %v", got, err)
	}
}

func TestEvents_BothTraceFormats(t *testing.T) {
	path, runID, _ := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "events", runID)
	requireOK(t, code, out, errw)
	for _, want := range []string{agentrt.EventRunCreated, agentrt.EventStepDecided, agentrt.EventStepPolicy, agentrt.EventApprovalRequested} {
		if !strings.Contains(out, want) {
			t.Fatalf("events output missing %q:\n%s", want, out)
		}
	}
	code, out, errw = exec(t, "-db", path, "-json", "events", runID)
	requireOK(t, code, out, errw)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 8 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	for _, l := range lines {
		var rec struct {
			Seq   int64  `json:"seq"`
			RunID string `json:"run_id"`
			Type  string `json:"type"`
		}
		if err := json.Unmarshal([]byte(l), &rec); err != nil || rec.Seq == 0 || rec.RunID != runID || rec.Type == "" {
			t.Fatalf("line %q: %+v, %v", l, rec, err)
		}
	}
}

func TestApprove_RecordsTheDecisionAndTraces(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "approve", runID, "-by", "joey", "-note", "looks good")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "approval "+approvalID+": approved by joey") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(out, "stays waiting until the consumer resumes it") {
		t.Fatalf("approve must say the run is not resumed:\n%s", out)
	}
	if !strings.Contains(errw, agentrt.EventApprovalDecided) {
		t.Fatalf("stderr trace:\n%s", errw)
	}
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := store.GetApproval(context.Background(), runID, approvalID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != agentrt.ApprovalApproved || a.DecidedBy != "joey" || a.Note != "looks good" {
		t.Fatalf("approval = %+v", a)
	}
	// A decided approval cannot be decided again, by id or by the rule.
	if code, _, errw := exec(t, "-db", path, "approve", runID); code != exitError || !strings.Contains(errw, "no pending approval") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
	if code, _, errw := exec(t, "-db", path, "reject", runID, "-approval", approvalID); code != exitError || !strings.Contains(errw, "not pending") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
}

func TestApprove_DefaultsWhoToTheEnvironment(t *testing.T) {
	path, runID, _ := pausedDB(t)
	t.Setenv("USER", "operator")
	code, out, errw := exec(t, "-db", path, "approve", runID)
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "approved by operator") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestReject_CancelsTheRun(t *testing.T) {
	path, runID, _ := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "-json", "reject", runID, "-by", "joey", "-note", "not now")
	requireOK(t, code, out, errw)
	var payload struct {
		Run      view.RunSummary   `json:"run"`
		Approval *agentrt.Approval `json:"approval"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	if payload.Run.Status != agentrt.StatusCancelled || payload.Run.Reason != agentrt.ReasonApprovalRejected {
		t.Fatalf("run = %+v", payload.Run)
	}
	if payload.Approval == nil || payload.Approval.Status != agentrt.ApprovalRejected {
		t.Fatalf("approval = %+v", payload.Approval)
	}
}

func TestCancel_EndsTheRunAndRefusesATerminalOne(t *testing.T) {
	path, runID, _ := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "cancel", runID, "-by", "joey", "-note", "changed my mind")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "run "+runID+": CANCELLED (operator_cancelled)") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(errw, agentrt.EventRunFinished) {
		t.Fatalf("stderr trace:\n%s", errw)
	}
	if code, _, errw := exec(t, "-db", path, "cancel", runID); code != exitError || !strings.Contains(errw, "already CANCELLED") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
}

func TestUsage_ExitCodes(t *testing.T) {
	path, runID, _ := pausedDB(t)
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no command", []string{"-db", path}, exitUsage},
		{"unknown command", []string{"-db", path, "resume", runID}, exitUsage},
		{"unknown flag", []string{"-db", path, "-nope"}, exitUsage},
		{"show without a run", []string{"-db", path, "show"}, exitUsage},
		{"events without a run", []string{"-db", path, "events"}, exitUsage},
		{"approve without a run", []string{"-db", path, "approve", "-by", "joey"}, exitUsage},
		{"unknown run", []string{"-db", path, "show", "nope"}, exitError},
		{"unknown run events", []string{"-db", path, "events", "nope"}, exitError},
	} {
		if code, out, errw := exec(t, tc.args...); code != tc.want {
			t.Fatalf("%s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", tc.name, code, tc.want, out, errw)
		}
	}
}

func TestHelp_SaysResumeBelongsToTheConsumer(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}} {
		code, out, errw := exec(t, args...)
		requireOK(t, code, out, errw)
		if !strings.Contains(out, "no resume command") || !strings.Contains(out, "driver.Resume") {
			t.Fatalf("%v output:\n%s", args, out)
		}
	}
}
