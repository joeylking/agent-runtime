package agentrt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/trace"
)

// numbered is an id sequence, so a fixture's ids are the same every run.
func numbered() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("id-%02d", n) }
}

// recheckTools are the fixture's tools, one for each side-effect class.
func recheckTools() []agentrt.Tool {
	return []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("wipe", agentrt.Destructive), echoTool("publish", agentrt.RemoteMutation), echoTool("write", agentrt.LocalMutation)}
}

// recheckScript reads, wipes, publishes, writes, and completes.
func recheckScript() *scripted.Agent {
	return &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("wipe", `{"n":2}`, ""),
		scripted.ToolCall("publish", `{"n":3}`, ""),
		scripted.ToolCall("write", `{"n":4}`, ""),
		scripted.Complete(`{"ok":true}`),
	}}
}

// recheckFixture runs recheckScript under policy with a ticking clock and
// numbered ids: under DefaultPolicy the read and the write are allowed,
// the wipe denied, and the publish waits for an approval, which is
// granted, and is allowed again on resume. With approve false the run is
// left waiting.
func recheckFixture(t *testing.T, policy agentrt.Policy, tools []agentrt.Tool, approve bool) (*agentrt.Store, agentrt.Run) {
	t.Helper()
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Agent: recheckScript(), Policy: policy, Tools: tools, Now: tick(), NewID: numbered()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := d.Start(ctx, "publish the notes", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != agentrt.StatusWaitingForApproval || !approve {
		return st, run
	}
	approvals, _ := st.ListApprovals(ctx, run.ID)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "op", "fine"); err != nil {
		t.Fatal(err)
	}
	if run, err = d.Resume(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return st, run
}

var (
	lenient = namedPolicy{agentrt.DefaultPolicy(), "default/v1"}
	strict  = namedPolicy{agentrt.SideEffectPolicy{agentrt.ReadOnly: agentrt.Allow, agentrt.LocalMutation: agentrt.Deny, agentrt.RemoteMutation: agentrt.Deny, agentrt.Destructive: agentrt.Deny}, "strict/v1"}
)

func recheck(t *testing.T, st *agentrt.Store, runID string, p agentrt.Policy, opts agentrt.RecheckOptions) agentrt.RecheckReport {
	t.Helper()
	rep, err := agentrt.Recheck(context.Background(), st, runID, p, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// line is an evaluation as the tests compare it.
func line(e agentrt.RecheckedEvaluation) string {
	now := "-"
	if e.Rechecked != nil {
		now = string(e.Rechecked.Outcome)
	}
	s := fmt.Sprintf("%d %s %s %s->%s", e.StepIndex, e.Tool, e.Result, e.Recorded.Outcome, now)
	if e.OnResume {
		s += " resume"
	}
	return s
}

func lines(rep agentrt.RecheckReport) string {
	var out []string
	for _, e := range rep.Evaluations {
		out = append(out, line(e))
	}
	return strings.Join(out, "\n")
}

func requireLines(t *testing.T, rep agentrt.RecheckReport, want ...string) {
	t.Helper()
	if got := lines(rep); got != strings.Join(want, "\n") {
		t.Fatalf("evaluations:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

// Re-checked under the policy it ran with, every evaluation of a run with
// an allow, a deny, and an approval granted and resumed is the same, the
// resume a line of its own, and the identity unchanged.
func TestRecheck_SamePolicyIsSame(t *testing.T) {
	st, run := recheckFixture(t, lenient, recheckTools(), true)
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	rep := recheck(t, st, run.ID, lenient, agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 read same allow->allow",
		"1 wipe same deny->deny",
		"2 publish same require_approval->require_approval",
		"2 publish same require_approval->require_approval resume",
		"3 write same allow->allow",
	)
	if !rep.Same() || rep.Identity != agentrt.IdentityUnchanged || rep.PolicyID != "default/v1" || rep.SpecSource != agentrt.SpecRecorded || len(rep.Notes) != 0 || rep.Rechecked() != 5 {
		t.Fatalf("report %+v", rep)
	}
	for _, e := range rep.Evaluations {
		if e.SpecSource != agentrt.SpecRecorded || e.PolicyID != "default/v1" {
			t.Fatalf("evaluation %+v", e)
		}
		if e.Recorded.Outcome == agentrt.RequireApproval && (e.RecordedHash == "" || e.RecordedHash != e.RecheckedHash) {
			t.Fatalf("approval hashes %q %q", e.RecordedHash, e.RecheckedHash)
		}
	}
	approvals, _ := st.ListApprovals(context.Background(), run.ID)
	if rep.Evaluations[2].RecordedHash != approvals[0].Hash {
		t.Fatalf("the recorded hash %s is not the approval's %s", rep.Evaluations[2].RecordedHash, approvals[0].Hash)
	}
}

// A stricter policy names exactly the evaluations it would have decided
// otherwise, and what they would have become.
func TestRecheck_StricterPolicyNamesTheStepsThatDiffer(t *testing.T) {
	st, run := recheckFixture(t, lenient, recheckTools(), true)
	rep := recheck(t, st, run.ID, strict, agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 read same allow->allow",
		"1 wipe same deny->deny",
		"2 publish different require_approval->deny",
		"2 publish different require_approval->deny resume",
		"3 write different allow->deny",
	)
	if rep.Same() || rep.Differences() != 3 || rep.Identity != agentrt.IdentityChanged {
		t.Fatalf("report %+v", rep)
	}
	if r := rep.Evaluations[4].Rechecked.Reason; r != "side effect local_mutation is deny by policy" {
		t.Fatalf("reason %q", r)
	}
}

// A policy whose approval says something else differs on every
// require_approval by the approval's hash, although the outcome matches.
func TestRecheck_ChangedPresentationDiffersByHash(t *testing.T) {
	st, run := recheckFixture(t, agentrt.DefaultPolicy(), recheckTools(), true)
	reworded := agentrt.PolicyFunc(func(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		pd, err := agentrt.DefaultPolicy().Evaluate(ctx, req, view)
		if pd.Outcome == agentrt.RequireApproval {
			pd.Presentation = json.RawMessage(`{"question":"publish it?"}`)
			pd.Reason = "worded otherwise"
		}
		return pd, err
	})
	rep := recheck(t, st, run.ID, reworded, agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 read same allow->allow",
		"1 wipe same deny->deny",
		"2 publish different require_approval->require_approval",
		"2 publish different require_approval->require_approval resume",
		"3 write same allow->allow",
	)
	for _, e := range rep.Evaluations[2:4] {
		if !strings.Contains(e.Detail, "another approval") || e.RecordedHash == e.RecheckedHash {
			t.Fatalf("evaluation %+v", e)
		}
	}
	if rep.Identity != agentrt.IdentityUnknown {
		t.Fatalf("identity %s with neither side identified", rep.Identity)
	}
	// A reason alone is never a difference.
	reasoned := agentrt.PolicyFunc(func(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		pd, err := agentrt.DefaultPolicy().Evaluate(ctx, req, view)
		pd.Reason = "another reason"
		return pd, err
	})
	if rep := recheck(t, st, run.ID, reasoned, agentrt.RecheckOptions{}); !rep.Same() {
		t.Fatalf("a changed reason differs:\n%s", lines(rep))
	}
}

// A tool whose description changed between runs: each run re-checks the
// same against the spec it recorded, while today's spec makes the earlier
// run's approvals other approvals, and a request naming a tool that is no
// longer registered, or whose arguments today's schema refuses, differs.
func TestRecheck_CurrentSpecsAgainstRecordedSpecs(t *testing.T) {
	before := recheckTools()
	st, run := recheckFixture(t, agentrt.DefaultPolicy(), before, true)
	after := recheckTools()
	after[2].(*fakeTool).spec.Description = "publish, reworded"

	if rep := recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{}); !rep.Same() {
		t.Fatalf("recorded specs:\n%s", lines(rep))
	}
	if rep := recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{Tools: before}); !rep.Same() || rep.SpecSource != agentrt.SpecCurrent {
		t.Fatalf("unchanged current specs:\n%s", lines(rep))
	}
	rep := recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{Tools: after})
	requireLines(t, rep,
		"0 read same allow->allow",
		"1 wipe same deny->deny",
		"2 publish different require_approval->require_approval",
		"2 publish different require_approval->require_approval resume",
		"3 write same allow->allow",
	)
	if e := rep.Evaluations[2]; e.SpecSource != agentrt.SpecCurrent || !strings.Contains(e.Detail, "tool spec") {
		t.Fatalf("evaluation %+v", e)
	}

	// A later run with the reworded tool records the new spec, and is the
	// same under it.
	st2, run2 := recheckFixture(t, agentrt.DefaultPolicy(), after, true)
	if rep := recheck(t, st2, run2.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{}); !rep.Same() {
		t.Fatalf("the later run:\n%s", lines(rep))
	}

	strictSchema := recheckTools()
	strictSchema[0].(*fakeTool).spec.InputSchema = []byte(`{"type":"object","properties":{"n":{"type":"string"}},"required":["n"]}`)
	rep = recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{Tools: strictSchema[:3]})
	if e := rep.Evaluations[0]; e.Result != agentrt.RecheckDifferent || e.Rechecked != nil || !strings.Contains(e.Detail, "current tool's schema") {
		t.Fatalf("args the schema now refuses: %+v", e)
	}
	if e := rep.Evaluations[4]; e.Result != agentrt.RecheckDifferent || !strings.Contains(e.Detail, `tool "write" is not among the current tools`) {
		t.Fatalf("a tool no longer registered: %+v", e)
	}
}

// A policy that fails is a difference, a run not terminal is re-checked up
// to its current state with a note, and a missing run is ErrNotFound.
func TestRecheck_PolicyErrorAndRunNotTerminal(t *testing.T) {
	st, run := recheckFixture(t, agentrt.DefaultPolicy(), recheckTools(), false)
	requireStatus(t, run, agentrt.StatusWaitingForApproval, "")
	failing := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{}, errors.New("rules unavailable")
	})
	rep := recheck(t, st, run.ID, failing, agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 read different allow->-",
		"1 wipe different deny->-",
		"2 publish different require_approval->-",
	)
	if rep.Evaluations[0].Detail != "policy failed: rules unavailable" {
		t.Fatalf("detail %q", rep.Evaluations[0].Detail)
	}
	if len(rep.Notes) != 1 || rep.Notes[0] != "the run is WAITING_FOR_APPROVAL: re-checked up to its current state" {
		t.Fatalf("notes %q", rep.Notes)
	}
	if _, err := agentrt.Recheck(context.Background(), st, "nope", failing, agentrt.RecheckOptions{}); !errors.Is(err, agentrt.ErrNotFound) {
		t.Fatalf("a missing run: %v", err)
	}
	if _, err := agentrt.Recheck(context.Background(), st, run.ID, namedPolicy{failing, "bad\nid"}, agentrt.RecheckOptions{}); err == nil {
		t.Fatal("an identity NewDriver refuses was accepted")
	}
}

// sameView fails unless got, what a policy was handed in a re-check, is
// field by field what want, handed it in the run, held. No steps or no
// approvals is nil on both sides, never an empty slice on one: a policy
// that encodes its view sees null either way.
func sameView(t *testing.T, i int, got, want agentrt.RunView) {
	t.Helper()
	if (got.Steps == nil) != (want.Steps == nil) || (got.Approvals == nil) != (want.Approvals == nil) {
		t.Fatalf("evaluation %d: steps nil %v and approvals nil %v, the policy saw %v and %v", i, got.Steps == nil, got.Approvals == nil, want.Steps == nil, want.Approvals == nil)
	}
	g, w := got.Run, want.Run
	if !g.CreatedAt.Equal(w.CreatedAt) || !g.StartedAt.Equal(w.StartedAt) {
		t.Fatalf("evaluation %d: times %v %v, the policy saw %v %v", i, g.CreatedAt, g.StartedAt, w.CreatedAt, w.StartedAt)
	}
	g.CreatedAt, g.StartedAt = w.CreatedAt, w.StartedAt
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("evaluation %d: run\n%+v\nthe policy saw\n%+v", i, g, w)
	}
	if len(got.Steps) != len(want.Steps) || len(got.Approvals) != len(want.Approvals) {
		t.Fatalf("evaluation %d: %d steps and %d approvals, the policy saw %d and %d", i, len(got.Steps), len(got.Approvals), len(want.Steps), len(want.Approvals))
	}
	for j := range got.Steps {
		if !reflect.DeepEqual(got.Steps[j], want.Steps[j]) {
			t.Fatalf("evaluation %d: step %d\n%+v\nthe policy saw\n%+v", i, j, got.Steps[j], want.Steps[j])
		}
	}
	for j := range got.Approvals {
		if !reflect.DeepEqual(got.Approvals[j], want.Approvals[j]) {
			t.Fatalf("evaluation %d: approval %d\n%+v\nthe policy saw\n%+v", i, j, got.Approvals[j], want.Approvals[j])
		}
	}
}

// sameRequests fails unless every request handed in the re-check is the one
// handed in the run: the same ids and arguments, and the same spec, which
// is stored in canonical form and so compared by its hash.
func sameRequests(t *testing.T, got, want *seeingPolicy) {
	t.Helper()
	if len(got.reqs) != len(want.reqs) || len(want.reqs) == 0 {
		t.Fatalf("%d evaluations re-checked of %d", len(got.reqs), len(want.reqs))
	}
	for i := range want.reqs {
		g, w := got.reqs[i], want.reqs[i]
		if g.RunID != w.RunID || g.StepID != w.StepID || !bytes.Equal(g.Args, w.Args) || specHash(t, g.Spec) != specHash(t, w.Spec) {
			t.Fatalf("evaluation %d: request %+v, the policy was handed %+v", i, g, w)
		}
		sameView(t, i, got.views[i], want.views[i])
	}
}

// The view a re-check hands the policy is, field by field, the one it was
// handed in a model-backed loop whose totals grow under it.
func TestRecheck_RebuildsTheViewOfTheLoop(t *testing.T) {
	h := newHarness(t, ":memory:")
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	m := &replay.Scripted{ModelName: "test-model", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), toolUse("write", `{"n":2}`), toolUse("read", `{"n":3}`), done}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: modelAgent{}, Policy: p, Now: tick(),
		Tools: []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("write", agentrt.LocalMutation)},
		Model: &agentrt.ModelConfig{Model: m, Prices: agentrt.PriceTable{"test-model": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}, Backoff: time.Millisecond, CallTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "read and write", modelLimits())
	if err != nil {
		t.Fatal(err)
	}
	again := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	if rep := recheck(t, h.store, run.ID, again, agentrt.RecheckOptions{}); !rep.Same() {
		t.Fatalf("re-check:\n%s", lines(rep))
	}
	sameRequests(t, again, p)
	if again.views[2].Run.ModelCalls == 0 || len(again.views[2].Steps) != 2 {
		t.Fatalf("the last view rebuilt nothing to compare: %+v", again.views[2])
	}
}

// On resume the rebuilt view is the WAITING run, the steps before the
// approved one, and the approval as approved, as the policy saw it then,
// although the run went on afterwards.
func TestRecheck_RebuildsTheViewOnResume(t *testing.T) {
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	st, run := recheckFixture(t, p, recheckTools(), true)
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	again := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	if rep := recheck(t, st, run.ID, again, agentrt.RecheckOptions{}); !rep.Same() {
		t.Fatalf("re-check:\n%s", lines(rep))
	}
	sameRequests(t, again, p)
	if v := again.views[3]; v.Run.Status != agentrt.StatusWaitingForApproval || len(v.Approvals) != 1 || v.Approvals[0].Status != agentrt.ApprovalApproved {
		t.Fatalf("the resume-time view: %+v", v)
	}
	if v := again.views[2]; len(v.Approvals) != 0 {
		t.Fatalf("the publish's first evaluation saw %d approvals", len(v.Approvals))
	}
}

// Through a gate the rebuilt views are the ones the policy saw: on each
// Propose, on the Attach of an approved step, and on a Propose after it.
func TestRecheck_RebuildsTheViewThroughAGate(t *testing.T) {
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	g, err := agentrt.NewGate(agentrt.GateConfig{Store: st, Policy: namedPolicy{p, "gate-policy"}, Now: tick(), NewID: numbered(), Tools: recheckTools()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := g.Begin(ctx, "gated", "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	propose := func(s *agentrt.Session, dec agentrt.Decision) agentrt.Verdict {
		t.Helper()
		step, err := s.Step(ctx)
		if err != nil || step == nil {
			t.Fatalf("step: %v", err)
		}
		v, err := step.Propose(ctx, dec)
		if err != nil {
			t.Fatal(err)
		}
		if v.Outcome == agentrt.VerdictAllowed {
			if _, err := step.Execute(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	propose(s, scripted.ToolCall("read", `{"n":1}`, ""))
	propose(s, scripted.ToolCall("wipe", `{"n":1}`, ""))
	v := propose(s, scripted.ToolCall("publish", `{"n":1}`, ""))
	s.Close()
	if err := (agentrt.Operator{Store: st, Now: tick()}).Approve(ctx, "gated", v.Approval.ID, "op", ""); err != nil {
		t.Fatal(err)
	}
	s, av, err := g.Attach(ctx, "gated")
	if err != nil || av.Outcome != agentrt.VerdictAllowed {
		t.Fatalf("attach: %+v %v", av, err)
	}
	if _, err := s.Current().Execute(ctx); err != nil {
		t.Fatal(err)
	}
	propose(s, scripted.ToolCall("publish", `{"n":2}`, ""))
	s.Close()

	again := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	rep := recheck(t, st, "gated", namedPolicy{again, "gate-policy"}, agentrt.RecheckOptions{})
	if !rep.Same() || rep.Identity != agentrt.IdentityUnchanged || rep.Rechecked() != 5 {
		t.Fatalf("re-check:\n%s\n%+v", lines(rep), rep)
	}
	sameRequests(t, again, p)
}

// The text and JSON renderings are pinned: ids and times come from the
// fixture's clock and id sequence.
func TestRecheck_RenderingsArePinned(t *testing.T) {
	st, run := recheckFixture(t, lenient, recheckTools(), true)
	rep := recheck(t, st, run.ID, strict, agentrt.RecheckOptions{})
	var text bytes.Buffer
	if err := trace.WriteRecheck(&text, rep); err != nil {
		t.Fatal(err)
	}
	const wantText = `run id-01 COMPLETED: 3 of 5 re-checked evaluations differ; policy changed: recorded "default/v1", given "strict/v1"; recorded specs
DIFFERENT step 2 seq 14 publish: require_approval -> deny: side effect remote_mutation is deny by policy
DIFFERENT step 2 seq 18 publish (on resume): require_approval -> deny: side effect remote_mutation is deny by policy
DIFFERENT step 3 seq 23 write: allow -> deny: side effect local_mutation is deny by policy
same      step 0 seq 5 read: allow
same      step 1 seq 10 wipe: deny
`
	if text.String() != wantText {
		t.Fatalf("text:\n%s\nwant:\n%s", text.String(), wantText)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"run_id":"id-01","run_status":"COMPLETED","policy_id":"strict/v1","identity":"changed","spec_source":"recorded","evaluations":[{"step_index":0,"step_id":"id-02","seq":5,"tool":"read","result":"same","recorded":{"outcome":"allow","reason":"side effect read_only is allow by policy"},"rechecked":{"outcome":"allow","reason":"side effect read_only is allow by policy"},"spec_source":"recorded","policy_id":"default/v1"},{"step_index":1,"step_id":"id-03","seq":10,"tool":"wipe","result":"same","recorded":{"outcome":"deny","reason":"side effect destructive is deny by policy"},"rechecked":{"outcome":"deny","reason":"side effect destructive is deny by policy"},"spec_source":"recorded","policy_id":"default/v1"},{"step_index":2,"step_id":"id-04","seq":14,"tool":"publish","result":"different","recorded":{"outcome":"require_approval","reason":"side effect remote_mutation is require_approval by policy","kind":"remote_mutation","capability":{"args":{"n":3},"tool":"publish"},"presentation":{"args":{"n":3},"tool":"publish"}},"rechecked":{"outcome":"deny","reason":"side effect remote_mutation is deny by policy"},"recorded_hash":"d93da328f90c100de532004da432ba577ae799ed30a607eb87bed7e3b6bf73e0","spec_source":"recorded","policy_id":"default/v1"},{"step_index":2,"step_id":"id-04","seq":18,"tool":"publish","on_resume":true,"result":"different","recorded":{"outcome":"require_approval","reason":"side effect remote_mutation is require_approval by policy","kind":"remote_mutation","capability":{"args":{"n":3},"tool":"publish"},"presentation":{"args":{"n":3},"tool":"publish"}},"rechecked":{"outcome":"deny","reason":"side effect remote_mutation is deny by policy"},"recorded_hash":"d93da328f90c100de532004da432ba577ae799ed30a607eb87bed7e3b6bf73e0","spec_source":"recorded","policy_id":"default/v1"},{"step_index":3,"step_id":"id-06","seq":23,"tool":"write","result":"different","recorded":{"outcome":"allow","reason":"side effect local_mutation is allow by policy"},"rechecked":{"outcome":"deny","reason":"side effect local_mutation is deny by policy"},"spec_source":"recorded","policy_id":"default/v1"}],"same":false,"differences":3,"rechecked":5,"not_recheckable":0,"complete":true}`
	if string(raw) != wantJSON {
		t.Fatalf("JSON:\n%s\nwant:\n%s", raw, wantJSON)
	}
}

// The built-in SideEffectPolicy has no identity: a map has none that stays
// the same while its contents change.
func TestSideEffectPolicy_IsNotIdentified(t *testing.T) {
	if _, ok := any(agentrt.DefaultPolicy()).(agentrt.IdentifiedPolicy); ok {
		t.Fatal("SideEffectPolicy implements IdentifiedPolicy")
	}
}

// approveAndResume approves the run's latest approval and resumes it.
func approveAndResume(t *testing.T, d *agentrt.Driver, st *agentrt.Store, run agentrt.Run, by string) agentrt.Run {
	t.Helper()
	ctx := context.Background()
	approvals, err := st.ListApprovals(ctx, run.ID)
	if err != nil || len(approvals) == 0 {
		t.Fatalf("approvals: %v %v", approvals, err)
	}
	if err := d.Approve(ctx, run.ID, approvals[len(approvals)-1].ID, by, ""); err != nil {
		t.Fatal(err)
	}
	if run, err = d.Resume(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	return run
}

// An approval whose capability is longer than MaxPageText is handed to
// the re-checked policy whole, as the policy saw it: a policy that allows
// a read only under a grant carrying the whole capability decides the
// same again, rather than denying it for a capability a page read cut.
func TestRecheck_ApprovalLongerThanAPageIsReadWhole(t *testing.T) {
	blob := strings.Repeat("x", agentrt.MaxPageText+100)
	grant, _ := json.Marshal(map[string]string{"blob": blob})
	p := namedPolicy{agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		for _, a := range view.Approvals {
			if a.Status != agentrt.ApprovalApproved {
				continue
			}
			if req.Spec.Name == "publish" && a.StepID == req.StepID {
				return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
			}
			var c struct{ Blob string }
			if req.Spec.Name == "read" && json.Unmarshal(a.Capability, &c) == nil && c.Blob == blob {
				return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
			}
		}
		if req.Spec.Name == "publish" {
			return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "grant", Capability: grant}, nil
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "no grant"}, nil
	}), "grant/v1"}
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: recheckTools(),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":2}`, ""), scripted.Complete(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "publish", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	run = approveAndResume(t, d, st, run, "op")
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	steps, _ := st.ListSteps(context.Background(), run.ID)
	if steps[1].Policy == nil || steps[1].Policy.Outcome != agentrt.Allow {
		t.Fatalf("the read was not allowed under the grant: %+v", steps[1].Policy)
	}
	rep := recheck(t, st, run.ID, p, agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 publish not_recheckable ->-",
		"0 publish same allow->allow resume",
		"1 read same allow->allow",
	)
	if !rep.Same() || rep.Complete() {
		t.Fatalf("same %v complete %v", rep.Same(), rep.Complete())
	}
	// The first evaluation's own step.policy carries the capability and is
	// longer than a page carries: that one cannot be rebuilt.
	if e := rep.Evaluations[0]; e.Detail != "not re-checkable: its step.policy is longer than MaxPageText" {
		t.Fatalf("detail %q", e.Detail)
	}
}

// A policy that failed recorded no evaluation, in the loop or on resume,
// and the re-check lists each as not re-checkable at its step.failed
// rather than leaving it out; the report is not complete.
func TestRecheck_PolicyErrorIsListed(t *testing.T) {
	ctx := context.Background()
	calls := 0
	p := namedPolicy{agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		calls++
		switch {
		case req.Spec.Name == "read" && calls == 1:
			return agentrt.PolicyDecision{}, errors.New("rules unavailable")
		case req.Spec.Name == "publish" && view.Run.Status == agentrt.StatusWaitingForApproval:
			return agentrt.PolicyDecision{}, errors.New("rules gone")
		}
		return agentrt.DefaultPolicy().Evaluate(ctx, req, view)
	}), "flaky/v1"}

	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: recheckTools(),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":2}`, "")}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(ctx, "read", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonInternalError)
	rep := recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{})
	requireLines(t, rep, "0 read not_recheckable ->-")
	if e := rep.Evaluations[0]; e.Detail != "policy failed; no evaluation recorded: rules unavailable" || e.PolicyID != "flaky/v1" || e.Seq == 0 {
		t.Fatalf("evaluation %+v", e)
	}
	if rep.Complete() || rep.Rechecked() != 0 || !rep.Same() {
		t.Fatalf("complete %v rechecked %d", rep.Complete(), rep.Rechecked())
	}

	st, err = agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	calls = 0
	d2, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: recheckTools(),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish", `{"n":1}`, "")}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err = d2.Start(ctx, "publish", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	run = approveAndResume(t, d2, st, run, "op")
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonInternalError)
	rep = recheck(t, st, run.ID, agentrt.DefaultPolicy(), agentrt.RecheckOptions{})
	requireLines(t, rep,
		"0 publish same require_approval->require_approval",
		"0 publish not_recheckable ->- resume",
	)
	if rep.Evaluations[1].Detail != "policy failed; no evaluation recorded: rules gone" || rep.Complete() {
		t.Fatalf("report %+v", rep)
	}
}

// A decision the runtime refused as invalid, re-checked under a policy
// that returns it again, is the same, with why it was refused; under a
// policy that now returns a valid decision it differs.
func TestRecheck_InvalidDecisionRefusedAlikeIsSame(t *testing.T) {
	invalid := namedPolicy{agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "k", Capability: json.RawMessage(`{"a":1,"a":2}`)}, nil
	}), "invalid/v1"}
	st, run := recheckFixture(t, invalid, recheckTools(), false)
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonInternalError)
	rep := recheck(t, st, run.ID, invalid, agentrt.RecheckOptions{})
	requireLines(t, rep, "0 read same require_approval->require_approval")
	if e := rep.Evaluations[0]; e.Detail != `an invalid decision, refused as recorded: capability is not usable JSON: duplicate key "a"` || !rep.Same() || !rep.Complete() {
		t.Fatalf("evaluation %+v", e)
	}
	var b strings.Builder
	trace.WriteRecheck(&b, rep)
	if !strings.Contains(b.String(), `same      step 0 seq 5 read: require_approval: an invalid decision, refused as recorded: capability is not usable JSON: duplicate key "a"`) {
		t.Fatalf("rendered:\n%s", b.String())
	}
	valid := namedPolicy{agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "k", Capability: json.RawMessage(`{"a":2}`)}, nil
	}), "invalid/v1"}
	rep = recheck(t, st, run.ID, valid, agentrt.RecheckOptions{})
	requireLines(t, rep, "0 read different require_approval->require_approval")
	if e := rep.Evaluations[0]; !strings.HasPrefix(e.Detail, "the recorded decision was refused as invalid") {
		t.Fatalf("detail %q", e.Detail)
	}
}

// A step resumed at the first step was handed no steps: nil in the
// resume's view as in the loop's and the re-check's, never an empty
// slice on one side. The views are compared as sameView compares them,
// nil and empty told apart, and as JSON, where nil is null and empty [].
func TestRecheck_ResumeAtTheFirstStepHasNoSteps(t *testing.T) {
	approvedBy := func(view agentrt.RunView) int {
		n := 0
		for _, a := range view.Approvals {
			if a.Status == agentrt.ApprovalApproved {
				n++
			}
		}
		return n
	}
	// The publish asks for a second approval after the first, then is
	// allowed: three evaluations of step 0, two of them on resume.
	base := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		if n := approvedBy(view); n < 2 {
			return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "k", Capability: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}, nil
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
	})
	p := &seeingPolicy{Policy: base}
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: recheckTools(),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("publish", `{"n":1}`, ""), scripted.Complete(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "publish", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	run = approveAndResume(t, d, st, run, "alice")
	run = approveAndResume(t, d, st, run, "bob")
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(p.views) != 3 {
		t.Fatalf("%d evaluations", len(p.views))
	}
	for i, v := range p.views {
		if v.Steps != nil {
			t.Fatalf("evaluation %d was handed steps %#v, not nil", i, v.Steps)
		}
	}
	again := &seeingPolicy{Policy: base}
	if rep := recheck(t, st, run.ID, again, agentrt.RecheckOptions{}); !rep.Same() || !rep.Complete() || rep.Rechecked() != 3 {
		t.Fatalf("re-check:\n%s", lines(rep))
	}
	sameRequests(t, again, p)
	for i := range p.views {
		w, _ := json.Marshal(p.views[i])
		g, _ := json.Marshal(again.views[i])
		if !bytes.Equal(g, w) {
			t.Fatalf("evaluation %d as JSON:\n%s\nthe policy saw\n%s", i, g, w)
		}
	}
}
