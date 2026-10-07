package agentrt

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A database written by v0.4.0 is re-checked rather than refused: its
// step.policy events, which recorded neither spec nor view, are not
// re-checkable and the report says why, a run with none says it was
// recorded before v0.5.0, and an evaluation made after the migration, the
// resume of the run it left waiting, is re-checked against what it
// recorded.
func TestRecheck_PreMigrationRunIsNotRecheckable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v040.db")
	v040(t, path)
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	const before = "recorded before v0.5.0: its decisions recorded then cannot be re-checked"

	done, err := Recheck(ctx, st, "done", DefaultPolicy(), RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Evaluations) != 0 || !done.Same() || strings.Join(done.Notes, "|") != before+"|no policy evaluation is recorded" {
		t.Fatalf("the finished run: %+v", done)
	}

	waiting, err := Recheck(ctx, st, "waiting", DefaultPolicy(), RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.Evaluations) != 1 || waiting.Rechecked() != 0 || !waiting.Same() || waiting.Identity != IdentityUnknown {
		t.Fatalf("the waiting run: %+v", waiting)
	}
	if e := waiting.Evaluations[0]; e.Result != RecheckNotRecheckable || e.Detail != "not re-checkable: recorded before v0.5.0" || e.StepIndex != 0 || e.Tool != "publish" || e.Recorded.Outcome != RequireApproval {
		t.Fatalf("its step.policy: %+v", e)
	}
	if strings.Join(waiting.Notes, "|") != "the run is WAITING_FOR_APPROVAL: re-checked up to its current state|"+before {
		t.Fatalf("notes %q", waiting.Notes)
	}

	// Resumed after the migration: the resume's evaluation is re-checked.
	if _, err := st.DB().Exec(`INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by) VALUES ('a0', 'waiting', 'w0', 'remote_mutation', '{}', '{}', '{}', '', 'approved', '2026-10-01T00:00:02Z', '2026-10-01T00:00:03Z', 'op')`); err != nil {
		t.Fatal(err)
	}
	publish := counted("publish", RemoteMutation)
	req := ToolRequest{RunID: "waiting", StepID: "w0", Spec: publish.spec, Args: []byte(`{"n":1}`)}
	h, err := approvalHash("remote_mutation", []byte(`{}`), []byte(`{}`), req)
	if err != nil {
		t.Fatal(err)
	}
	reqJSON, _ := toJSON(req)
	st.DB().Exec(`UPDATE approvals SET request_json = ?, hash = ? WHERE id = 'a0'`, string(reqJSON), h)
	allow := SideEffectPolicy{RemoteMutation: Allow}
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Policy: allow, Tools: []Tool{publish}})
	if run, err := d.Resume(ctx, "waiting"); err != nil || run.Status != StatusCompleted {
		t.Fatalf("resume: %+v %v", run, err)
	}
	var views []RunView
	seeing := PolicyFunc(func(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error) {
		views = append(views, view)
		return allow.Evaluate(ctx, req, view)
	})
	resumed, err := Recheck(ctx, st, "waiting", seeing, RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Evaluations) != 2 || resumed.Evaluations[0].Result != RecheckNotRecheckable || resumed.Evaluations[1].Result != RecheckSame || !resumed.Evaluations[1].OnResume {
		t.Fatalf("after the resume: %+v", resumed)
	}
	// The approval, which the fixture wrote with no approval.decided, is
	// judged by its decided_at against the evaluation's time.
	if len(views) != 1 || views[0].Run.Goal != "publish" || views[0].Run.Status != StatusWaitingForApproval || len(views[0].Approvals) != 1 || views[0].Approvals[0].Status != ApprovalApproved {
		t.Fatalf("the rebuilt view: %+v", views)
	}
}
