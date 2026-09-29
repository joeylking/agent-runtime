package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// sayTool answers with a fixed summary, so a test can make a tool's own
// output carry a payload of its choosing.
type sayTool struct {
	spec    agentrt.ToolSpec
	summary string
}

func (t sayTool) Spec() agentrt.ToolSpec { return t.spec }
func (t sayTool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return agentrt.ToolResult{Content: c.Args, Summary: t.summary}, nil
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

// evilEsc is the audit's probe payload: it clears the screen, homes the
// cursor, and forges a fake APPROVAL WAITING block, ending with a bare
// carriage return so a naive renderer overwrites its own line.
const evilEsc = "\x1b[2J\x1b[H\x1b[32mAPPROVAL WAITING  looks-safe\x1b[0m\rX"

// evilDB reproduces the audit's probe: a goal, a policy's approval kind, a
// tool's own summary, and a model-chosen (and unregistered) tool name all
// carry terminal escapes or a C1 control byte, exactly as a model or a
// remote server might hand them back.
func evilDB(t *testing.T) (path, runID, approvalID string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "evil.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("no\x1b[31msuch\x1b]0;pwned\x07", `{"n":1}`, "model tool name"),
		scripted.ToolCall("read", `{"n":1}`, "look"),
		scripted.ToolCall("push", `{"n":7}`, "needs approval"),
	}}
	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.SideEffect != agentrt.RemoteMutation {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "ok"}, nil
		}
		return agentrt.NeedApproval("publication"+evilEsc, "remote", map[string]any{"tool": req.Spec.Name},
			map[string]any{"preview": "\u009b31m C1 CSI and \\u001b[2J"})
	})
	spec := func(name string, se agentrt.SideEffect) agentrt.ToolSpec {
		return agentrt.ToolSpec{Name: name, Description: name, InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`), SideEffect: se, Timeout: time.Second}
	}
	tools := []agentrt.Tool{
		sayTool{spec: spec("read", agentrt.ReadOnly), summary: "sum " + evilEsc},
		sayTool{spec: spec("push", agentrt.RemoteMutation), summary: "pushed"},
	}
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: policy, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Start(context.Background(), "goal "+evilEsc, agentrt.Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("status = %s", r.Status)
	}
	approvals, err := store.ListApprovals(context.Background(), r.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals = %+v, %v", approvals, err)
	}
	return path, r.ID, approvals[0].ID
}

// tamperSchema moves the database's recorded schema version away from what
// this build writes, without touching its tables, to reproduce a database
// from a different build: delta > 0 makes it look newer, delta < 0 older.
func tamperSchema(t *testing.T, path string, delta int) {
	t.Helper()
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var current int
	if err := store.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if delta > 0 {
		if _, err := store.DB().Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, current+delta, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := store.DB().Exec(`DELETE FROM schema_migrations WHERE version > ?`, current+delta); err != nil {
		t.Fatal(err)
	}
}

// execFull runs the command with the given stdin content and terminal-ness
// and returns its status and streams.
func execFull(t *testing.T, stdin string, interactive bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errw, interactive)
	return code, out.String(), errw.String()
}

// exec runs the command with empty, non-terminal stdin, which is how a
// script or a pipeline invokes it.
func exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return execFull(t, "", false, args...)
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
		fmt.Sprintf("decide with: agentrt -db %q approve %s -approval %s", path, runID, approvalID),
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

// TestShow_ListsEveryPendingApproval reproduces a run that somehow ended up
// with more than one pending approval, which RunSummary.PendingApprovalID
// alone cannot name: text mode must not hide the second one.
func TestShow_ListsEveryPendingApproval(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	second := approvalID + "-2"
	if _, err := store.DB().ExecContext(context.Background(), `INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, expires_at)
		SELECT ?, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, expires_at FROM approvals WHERE id = ?`, second, approvalID); err != nil {
		t.Fatal(err)
	}
	store.Close()

	code, out, errw := exec(t, "-db", path, "show", runID)
	requireOK(t, code, out, errw)
	if n := strings.Count(out, "APPROVAL WAITING"); n != 2 {
		t.Fatalf("want 2 approvals shown, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, approvalID) || !strings.Contains(out, second) {
		t.Fatalf("missing one of the pending approvals:\n%s", out)
	}

	code, out, errw = exec(t, "-db", path, "-json", "show", runID)
	requireOK(t, code, out, errw)
	var payload struct {
		Approvals []agentrt.Approval `json:"approvals"`
		Waiting   *agentrt.Approval  `json:"waiting_approval"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Approvals) != 2 {
		t.Fatalf("approvals = %+v", payload.Approvals)
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

// TestEvents_JSONMarksAnUnencodablePayloadRatherThanDroppingIt: a row whose
// payload is not valid JSON, however it got that way, must still appear in
// the trace rather than vanish.
func TestEvents_JSONMarksAnUnencodablePayloadRatherThanDroppingIt(t *testing.T) {
	path, runID, _ := pausedDB(t)
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO events (run_id, step_id, at, type, payload_json) VALUES (?,?,?,?,?)`,
		runID, "", time.Now().UTC().Format(time.RFC3339Nano), "test.broken", "not json"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	code, out, errw := exec(t, "-db", path, "-json", "events", runID)
	requireOK(t, code, out, errw)
	found := false
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("line is not valid JSON: %v\n%s", err, l)
		}
		if rec["type"] == "test.broken" {
			found = true
			if rec["invalid_payload"] != true || rec["payload"] != "not json" {
				t.Fatalf("rec = %+v", rec)
			}
		}
	}
	if !found {
		t.Fatal("the event with the broken payload was dropped rather than marked")
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

// TestEvents_ExitsNonZeroWhenOutputFails: a write failure while printing
// the trace must not be swallowed.
func TestEvents_ExitsNonZeroWhenOutputFails(t *testing.T) {
	path, runID, _ := pausedDB(t)
	for _, asJSON := range []bool{false, true} {
		var errw bytes.Buffer
		args := []string{"-db", path, "events", runID}
		if asJSON {
			args = []string{"-db", path, "-json", "events", runID}
		}
		code := run(args, strings.NewReader(""), failWriter{errors.New("disk full")}, &errw, false)
		if code != exitError {
			t.Fatalf("json=%v: exit %d, want %d\nstderr:\n%s", asJSON, code, exitError, errw.String())
		}
	}
}

func TestApprove_RecordsTheDecisionAndTraces(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "approve", runID, "-by", "joey", "-note", "looks good", "-yes")
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
	if code, _, errw := exec(t, "-db", path, "approve", runID, "-yes"); code != exitError || !strings.Contains(errw, "no pending approval") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
	if code, _, errw := exec(t, "-db", path, "reject", runID, "-approval", approvalID); code != exitError || !strings.Contains(errw, "not pending") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
}

func TestApprove_DefaultsWhoToTheEnvironment(t *testing.T) {
	path, runID, _ := pausedDB(t)
	t.Setenv("USER", "operator")
	code, out, errw := exec(t, "-db", path, "approve", runID, "-yes")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "approved by operator") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestReject_CancelsTheRun(t *testing.T) {
	path, runID, _ := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "-json", "reject", runID, "-by", "joey", "-note", "not now", "-yes")
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

// TestDecide_PrintsTheWaitingApprovalBeforeDeciding: even when the command
// goes on to refuse for lack of consent, the operator has already seen what
// they would have granted.
func TestDecide_PrintsTheWaitingApprovalBeforeDeciding(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, out, errw := exec(t, "-db", path, "approve", runID, "-by", "joey")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, errw)
	}
	if !strings.Contains(out, "APPROVAL WAITING  "+approvalID) || !strings.Contains(out, "kind    publication") {
		t.Fatalf("approval was not shown before refusing:\n%s", out)
	}
	if !strings.Contains(errw, "-approval") || !strings.Contains(errw, "-yes") {
		t.Fatalf("refusal does not say how to proceed:\n%s", errw)
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
	if a.Status != agentrt.ApprovalPending {
		t.Fatalf("a refusal must not decide anything: %+v", a)
	}
}

// TestDecide_InteractiveConfirmation covers both answers at the prompt.
func TestDecide_InteractiveConfirmation(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		path, runID, _ := pausedDB(t)
		code, out, errw := execFull(t, "y\n", true, "-db", path, "approve", runID, "-by", "joey")
		requireOK(t, code, out, errw)
		if !strings.Contains(out, "approved by joey") {
			t.Fatalf("out=%s", out)
		}
	})
	t.Run("declined", func(t *testing.T) {
		path, runID, approvalID := pausedDB(t)
		code, _, errw := execFull(t, "n\n", true, "-db", path, "approve", runID, "-by", "joey")
		if code != exitError || !strings.Contains(errw, "declined") {
			t.Fatalf("exit %d, stderr:\n%s", code, errw)
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
		if a.Status != agentrt.ApprovalPending {
			t.Fatalf("declining at the prompt must not decide anything: %+v", a)
		}
	})
}

func TestDecide_RefusesAnEmptyIdentity(t *testing.T) {
	path, runID, _ := pausedDB(t)
	t.Setenv("USER", "")
	code, _, errw := exec(t, "-db", path, "approve", runID, "-yes")
	if code != exitUsage || !strings.Contains(errw, "-by is empty") {
		t.Fatalf("exit %d, stderr:\n%s", code, errw)
	}
}

// TestDecide_TrailingArgumentIsUsageErrorNotSilentlyIgnored reproduces the
// audit's exact finding: "approve <run> wrong-id -by alice" must not
// approve the only pending approval as the shell's own user.
func TestDecide_TrailingArgumentIsUsageErrorNotSilentlyIgnored(t *testing.T) {
	path, runID, approvalID := pausedDB(t)
	code, _, errw := exec(t, "-db", path, "approve", runID, "wrong-id", "-by", "alice", "-yes")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, errw)
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
	if a.Status != agentrt.ApprovalPending {
		t.Fatalf("the trailing argument was silently ignored: approval is %s by %q", a.Status, a.DecidedBy)
	}
}

func TestDecide_AcceptsFlagsBeforeOrAfterTheRunID(t *testing.T) {
	path1, runID1, _ := pausedDB(t)
	code, out, errw := exec(t, "-db", path1, "approve", "-by", "carol", "-yes", runID1)
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "approved by carol") {
		t.Fatalf("flags before the run id: out=%s", out)
	}

	path2, runID2, _ := pausedDB(t)
	code, out, errw = exec(t, "-db", path2, "approve", runID2, "-by", "carol", "-yes")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "approved by carol") {
		t.Fatalf("flags after the run id: out=%s", out)
	}
}

func TestCancel_ExtraArgumentIsUsageError(t *testing.T) {
	path, runID, _ := pausedDB(t)
	code, _, errw := exec(t, "-db", path, "cancel", runID, "extra")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, errw)
	}
}

// TestOpen_MissingDatabaseIsErrorNotCreated: agentrt must never create the
// file it was pointed at.
func TestOpen_MissingDatabaseIsErrorNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	code, out, errw := exec(t, "-db", path, "runs")
	if code != exitError {
		t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitError, errw)
	}
	if out != "" {
		t.Fatalf("stdout not empty: %s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a database file was created at %s", path)
	}
}

func TestOpen_SchemaVersionMismatchSaysWhichSideIsNewer(t *testing.T) {
	t.Run("database is newer", func(t *testing.T) {
		path, _, _ := pausedDB(t)
		tamperSchema(t, path, +1)
		code, out, errw := exec(t, "-db", path, "runs")
		if code != exitError {
			t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitError, errw)
		}
		if out != "" {
			t.Fatalf("stdout not empty: %s", out)
		}
		if !strings.Contains(errw, "is newer than this build") {
			t.Fatalf("message does not say the database is newer:\n%s", errw)
		}
		if !strings.Contains(errw, "consumer") {
			t.Fatalf("message does not say the consumer owns migrations:\n%s", errw)
		}
	})
	t.Run("build is newer", func(t *testing.T) {
		path, _, _ := pausedDB(t)
		tamperSchema(t, path, -1)
		code, _, errw := exec(t, "-db", path, "runs")
		if code != exitError {
			t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitError, errw)
		}
		if !strings.Contains(errw, "this build of agentrt is newer") {
			t.Fatalf("message does not say the build is newer:\n%s", errw)
		}
	})
}

// TestOpen_PathsWithSpacesAndURLCharactersWork: a shared or user cache
// directory can contain any of these, and the operator's -db value must not
// be mangled by however the database is opened underneath.
func TestOpen_PathsWithSpacesAndURLCharactersWork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a dir with spaces & a #hash?query=1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runs.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, out, errw := exec(t, "-db", path, "runs")
	requireOK(t, code, out, errw)
	if !strings.Contains(out, "no runs") {
		t.Fatalf("out=%s", out)
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

// TestHelp_PrintsUsageExactlyOnce: -h used to print the usage text once via
// the flag package's own auto-usage and once more explicitly, both to
// different amounts of the same text.
func TestHelp_PrintsUsageExactlyOnce(t *testing.T) {
	code, out, errw := exec(t, "-h")
	requireOK(t, code, out, errw)
	if errw != "" {
		t.Fatalf("-h must write nothing to stderr:\n%s", errw)
	}
	if n := strings.Count(out, "usage: agentrt"); n != 1 {
		t.Fatalf("usage text appears %d times:\n%s", n, out)
	}
}

// TestUsage_OneTextWithTheSpecificErrorFirst covers both a global flag
// error and a subcommand flag error, which used to each produce their own,
// different usage text in addition to the command's.
func TestUsage_OneTextWithTheSpecificErrorFirst(t *testing.T) {
	t.Run("global flag", func(t *testing.T) {
		code, out, errw := exec(t, "-nope")
		if code != exitUsage {
			t.Fatalf("exit %d, want %d", code, exitUsage)
		}
		if out != "" {
			t.Fatalf("stdout not empty: %s", out)
		}
		if n := strings.Count(errw, "agentrt is the operator surface over an agent-runtime database."); n != 1 {
			t.Fatalf("the full usage text appears %d times:\n%s", n, errw)
		}
		first := strings.SplitN(errw, "\n", 2)[0]
		if !strings.Contains(first, "nope") {
			t.Fatalf("the specific error was not printed first: %q", first)
		}
	})
	t.Run("subcommand flag", func(t *testing.T) {
		path, runID, _ := pausedDB(t)
		code, out, errw := exec(t, "-db", path, "approve", runID, "-nope")
		if code != exitUsage {
			t.Fatalf("exit %d, want %d\nstderr:\n%s", code, exitUsage, errw)
		}
		if out != "" {
			t.Fatalf("stdout not empty before the store is even opened: %s", out)
		}
		if n := strings.Count(errw, "agentrt is the operator surface over an agent-runtime database."); n != 1 {
			t.Fatalf("the full usage text appears %d times:\n%s", n, errw)
		}
		if strings.Contains(errw, "Usage of") {
			t.Fatalf("the flag package's own usage text leaked through:\n%s", errw)
		}
	})
}

// TestShow_SanitizesModelAndServerSuppliedText reproduces the audit's
// probe: a goal, an approval kind, a tool summary, and a model-chosen tool
// name all carry terminal escapes or a C1 control byte. None may reach the
// terminal unescaped, and JSON must carry them unchanged.
func TestShow_SanitizesModelAndServerSuppliedText(t *testing.T) {
	path, runID, approvalID := evilDB(t)
	code, out, errw := exec(t, "-db", path, "show", runID)
	requireOK(t, code, out, errw)
	if strings.ContainsAny(out, "\x1b\x07\r") || strings.ContainsRune(out, 0x9B) {
		t.Fatalf("a raw control byte reached the terminal:\n%q", out)
	}
	for _, want := range []string{`\x1b[2J`, `\x07`, `\x9b`, approvalID, "goal    goal "} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The literal backslash-u text the model typed (not a real escape byte)
	// must survive unaltered.
	if !strings.Contains(out, `\u001b[2J`) {
		t.Fatalf("literal (non-control) text was mangled:\n%s", out)
	}

	code, out, errw = exec(t, "-db", path, "-json", "show", runID)
	requireOK(t, code, out, errw)
	var payload struct {
		Run view.RunSummary `json:"run"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	if payload.Run.Goal != "goal "+evilEsc {
		t.Fatalf("JSON output must carry the raw bytes unchanged: %q", payload.Run.Goal)
	}
}

func TestRuns_SanitizesTheGoal(t *testing.T) {
	path, _, _ := evilDB(t)
	code, out, errw := exec(t, "-db", path, "runs")
	requireOK(t, code, out, errw)
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("raw ESC reached the terminal:\n%q", out)
	}
	if !strings.Contains(out, `\x1b[2J`) {
		t.Fatalf("the goal was not sanitized at all:\n%s", out)
	}
}

// TestClip_CutsOnAUTF8CharacterBoundary: clip used to slice by raw byte
// count and could split a multi-byte character in half.
func TestClip_CutsOnAUTF8CharacterBoundary(t *testing.T) {
	s := strings.Repeat("é", 40) // 2 bytes each; a naive s[:61] lands mid-rune.
	for n := 55; n <= 65; n++ {
		got := clip(s, n)
		body := strings.TrimSuffix(got, "…")
		if !utf8.ValidString(body) {
			t.Fatalf("clip(_, %d) cut mid-character: %q", n, got)
		}
	}
}

// TestShow_TimeHasDateAndZone: "show" used to print local time with no
// zone, which is ambiguous between the operator's own clock and whatever
// produced the record.
func TestShow_TimeHasDateAndZone(t *testing.T) {
	path, runID, _ := pausedDB(t)
	_, out, _ := exec(t, "-db", path, "show", runID)
	re := regexp.MustCompile(`created \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4}`)
	if !re.MatchString(out) {
		t.Fatalf("created line missing date and zone:\n%s", out)
	}
}

// TestEvents_TimeHasDateAndZone: "events" used to print a bare
// HH:MM:SS.mmm with no date.
func TestEvents_TimeHasDateAndZone(t *testing.T) {
	path, runID, _ := pausedDB(t)
	_, out, _ := exec(t, "-db", path, "events", runID)
	re := regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3} [+-]\d{4}  `)
	if !re.MatchString(out) {
		t.Fatalf("event line missing date and zone:\n%s", out)
	}
}

// TestRuns_JSONOmitsAZeroFinishedAt: omitempty does nothing on time.Time,
// so a run still waiting used to marshal a zero-value finished_at.
func TestRuns_JSONOmitsAZeroFinishedAt(t *testing.T) {
	path, _, _ := pausedDB(t)
	_, out, _ := exec(t, "-db", path, "-json", "runs")
	if strings.Contains(out, "finished_at") {
		t.Fatalf("a zero finished_at was not omitted:\n%s", out)
	}
}
