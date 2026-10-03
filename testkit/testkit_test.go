package testkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/testkit"
)

// tool is a consumer tool with a counter and an optional body.
type tool struct {
	spec  agentrt.ToolSpec
	calls atomic.Int32
	fn    func(agentrt.ToolCall) (agentrt.ToolResult, error)
}

const objectSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`

func newTool(name string, se agentrt.SideEffect) *tool {
	return &tool{spec: agentrt.ToolSpec{Name: name, Description: name, InputSchema: []byte(objectSchema), SideEffect: se, Timeout: time.Second}}
}

func (t *tool) Spec() agentrt.ToolSpec { return t.spec }
func (t *tool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	t.calls.Add(1)
	if t.fn != nil {
		return t.fn(c)
	}
	return agentrt.ToolResult{Content: c.Args, Summary: "done " + t.spec.Name}, nil
}

// recorder keeps the failures the kit reports instead of failing the test,
// so a test can assert on them. Fatal failures still fail the test.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
func (r *recorder) Helper() {}

func (r *recorder) joined() string { return strings.Join(r.errors, "\n") }

func scenario(tools ...agentrt.Tool) testkit.Scenario {
	return testkit.Scenario{Tools: tools, Policy: agentrt.DefaultPolicy(), Goal: "g",
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall(tools[0].Spec().Name, `{"n":1}`, "first"), scripted.Complete(`{"ok":true}`)}}}
}

func eventTypes(events []agentrt.Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return strings.Join(out, " ")
}

// A side effect whose outcome the crash lost waits for an operator, who
// approves it: the same step runs again, once, under that approval.
func TestRun_SideEffectLostToACrashRunsAgainOnlyWhenApproved(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	res := testkit.Run(t, scenario(write), testkit.Crash{Step: 0, At: testkit.AfterToolEffect})
	if res.Run.Status != agentrt.StatusCompleted || write.calls.Load() != 2 {
		t.Fatalf("run %s, %d calls: %s", res.Run.Status, write.calls.Load(), res)
	}
	if len(res.Calls) != 2 || res.Calls[0].Returned || !res.Calls[1].Returned || res.Calls[0].Step != 0 || res.Calls[1].Step != 0 {
		t.Fatalf("calls = %+v", res.Calls)
	}
	if len(res.Approvals) != 1 || res.Approvals[0].Kind != agentrt.InterruptedSideEffect || res.Approvals[0].Status != agentrt.ApprovalApproved {
		t.Fatalf("approvals = %+v", res.Approvals)
	}
	if len(res.Crashes) != 1 || res.Resumes != 2 || res.Steps[0].Status != agentrt.StepDone {
		t.Fatalf("crashes %v resumes %d: %s", res.Crashes, res.Resumes, res)
	}
	if !strings.Contains(eventTypes(res.Events), agentrt.EventLeaseTakenOver) {
		t.Fatalf("no takeover recorded: %s", eventTypes(res.Events))
	}
}

// With the consumer's reconciliation the run continues without re-running
// the lost side effect; the step stays interrupted.
func TestRun_ReconciledSideEffectIsNotRunAgain(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	sc := scenario(write)
	var seen []agentrt.StepStatus
	sc.Reconcile = func(_ context.Context, v agentrt.RunView) (agentrt.Reconciliation, error) {
		for _, st := range v.Steps {
			seen = append(seen, st.Status)
		}
		return agentrt.Reconciliation{Outcome: agentrt.ReconcileContinue}, nil
	}
	res := testkit.Run(t, sc, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})
	if res.Run.Status != agentrt.StatusCompleted || write.calls.Load() != 1 || len(res.Approvals) != 0 {
		t.Fatalf("run %s, %d calls, %d approvals: %s", res.Run.Status, write.calls.Load(), len(res.Approvals), res)
	}
	if fmt.Sprint(seen) != "[interrupted]" || res.Steps[0].Status != agentrt.StepInterrupted {
		t.Fatalf("reconcile saw %v; %s", seen, res)
	}
}

// Rejecting the interrupted side effect ends the run with the tool never
// called again.
func TestRun_OperatorRejectsTheLostSideEffect(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	sc := scenario(write)
	sc.Operator = func(a agentrt.Approval) testkit.Verdict {
		if a.Kind != agentrt.InterruptedSideEffect {
			t.Errorf("asked about %s", a.Kind)
		}
		return testkit.Reject
	}
	res := testkit.Run(t, sc, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})
	if res.Run.Status != agentrt.StatusCancelled || res.Run.Reason != agentrt.ReasonApprovalRejected || write.calls.Load() != 1 {
		t.Fatalf("run %s/%s, %d calls", res.Run.Status, res.Run.Reason, write.calls.Load())
	}
}

// Leaving the approval undecided returns the waiting run, unleased.
func TestRun_OperatorLeavesTheRunWaiting(t *testing.T) {
	push := newTool("push", agentrt.RemoteMutation)
	sc := scenario(push)
	sc.Operator = func(agentrt.Approval) testkit.Verdict { return testkit.Leave }
	res := testkit.Run(t, sc)
	if res.Run.Status != agentrt.StatusWaitingForApproval || push.calls.Load() != 0 || res.Resumes != 0 {
		t.Fatalf("run %s, %d calls, %d resumes", res.Run.Status, push.calls.Load(), res.Resumes)
	}
}

// A read-only tool interrupted mid-call, and a step interrupted before or
// after its decision was recorded, continue without an operator and
// without the tool having run twice.
func TestRun_InterruptionsThatNeedNoOperator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		se    agentrt.SideEffect
		at    testkit.Point
		calls int32
	}{
		{"read only mid-call", agentrt.ReadOnly, testkit.AfterToolEffect, 1},
		{"before deciding", agentrt.LocalMutation, testkit.AfterStepStarted, 0},
		{"decision recorded", agentrt.LocalMutation, testkit.AfterDecided, 0},
		{"between steps", agentrt.LocalMutation, testkit.AfterToolFinished, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := newTool("work", tc.se)
			sc := scenario(work)
			// A replayer keyed by index skips the interrupted step's decision,
			// so the script carries a spare.
			sc.Agent = &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("work", `{"n":1}`, ""), scripted.Complete(`{}`), scripted.Complete(`{}`)}}
			res := testkit.Run(t, sc, testkit.Crash{Step: 0, At: tc.at})
			if res.Run.Status != agentrt.StatusCompleted || work.calls.Load() != tc.calls || len(res.Approvals) != 0 {
				t.Fatalf("run %s, %d calls, %d approvals: %s", res.Run.Status, work.calls.Load(), len(res.Approvals), res)
			}
			want := agentrt.StepInterrupted
			if tc.at == testkit.AfterToolFinished {
				want = agentrt.StepDone
			}
			if res.Steps[0].Status != want {
				t.Fatalf("step 0 = %s, want %s", res.Steps[0].Status, want)
			}
		})
	}
}

// A tool start recorded for a tool that never ran looks, in the record,
// like a lost outcome: the run waits, and approving runs the tool once.
func TestRun_RecordedStartWithoutTheToolRunsOnceWhenApproved(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	res := testkit.Run(t, scenario(write), testkit.Crash{Step: 0, At: testkit.AfterToolStarted})
	if res.Run.Status != agentrt.StatusCompleted || write.calls.Load() != 1 || len(res.Calls) != 1 || !res.Calls[0].Returned {
		t.Fatalf("run %s, %d calls %+v", res.Run.Status, write.calls.Load(), res.Calls)
	}
	if len(res.Approvals) != 1 || res.Approvals[0].Kind != agentrt.InterruptedSideEffect {
		t.Fatalf("approvals = %+v", res.Approvals)
	}
}

// An approved request whose resume crashed right after committing is not
// executed twice: the next resume finds the step executing and waits for
// the operator again.
func TestRun_ApprovedRequestSurvivesACrashAfterResume(t *testing.T) {
	push := newTool("push", agentrt.RemoteMutation)
	res := testkit.Run(t, scenario(push), testkit.Crash{At: testkit.AfterResumed})
	if res.Run.Status != agentrt.StatusCompleted || push.calls.Load() != 1 {
		t.Fatalf("run %s, %d calls: %s", res.Run.Status, push.calls.Load(), res)
	}
	kinds := []string{}
	for _, a := range res.Approvals {
		kinds = append(kinds, a.Kind+":"+string(a.Status))
	}
	// The policy asks for its own approval again when the interrupted one
	// is granted, so the request waits twice more and runs once.
	if fmt.Sprint(kinds) != "[remote_mutation:approved interrupted_side_effect:approved remote_mutation:approved]" {
		t.Fatalf("approvals = %v", kinds)
	}
}

// A resume that took the run over and died before pausing leaves the
// next resume knowing the step was executing: it still waits.
func TestRun_CrashAfterTakeoverStillWaitsForTheOperator(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	res := testkit.Run(t, scenario(write), testkit.Crash{Step: 0, At: testkit.AfterToolEffect}, testkit.Crash{Step: 0, At: testkit.AfterInterrupted})
	if res.Run.Status != agentrt.StatusCompleted || write.calls.Load() != 2 || len(res.Crashes) != 2 || res.Resumes != 3 {
		t.Fatalf("run %s, %d calls, crashes %v, resumes %d: %s", res.Run.Status, write.calls.Load(), res.Crashes, res.Resumes, res)
	}
	if len(res.Approvals) != 1 || res.Approvals[0].Kind != agentrt.InterruptedSideEffect {
		t.Fatalf("approvals = %+v", res.Approvals)
	}
}

// A crash point the run never reaches is a failure, not a silent pass.
func TestRun_UnreachedCrashIsReported(t *testing.T) {
	r := &recorder{TB: t}
	testkit.Run(r, scenario(newTool("read", agentrt.ReadOnly)), testkit.Crash{Step: 7, At: testkit.AfterToolEffect})
	if !strings.Contains(r.joined(), "step 7 tool.effect was never reached") {
		t.Fatalf("errors = %q", r.joined())
	}
}

// The record and the agent's inputs are what a consumer keys its own
// assertions on.
func TestRun_ResultCarriesTheRecord(t *testing.T) {
	read := newTool("read", agentrt.ReadOnly)
	ids := 0
	sc := scenario(read)
	sc.NewID = func() string { ids++; return fmt.Sprintf("id-%d", ids) }
	sc.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	res := testkit.Run(t, sc)
	if res.RunID != "id-1" || len(res.Steps) != 2 || len(res.Inputs) != 2 || len(res.Events) == 0 || res.Steps[0].StartedAt != sc.Now() {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Inputs[1].Steps) != 1 || res.Inputs[1].Steps[0].Decision.Tool != "read" {
		t.Fatalf("second input = %+v", res.Inputs[1])
	}
}

// ---- policy ---------------------------------------------------------------

func classTools() []agentrt.Tool {
	return []agentrt.Tool{newTool("read", agentrt.ReadOnly), newTool("write", agentrt.LocalMutation), newTool("push", agentrt.RemoteMutation), newTool("wipe", agentrt.Destructive)}
}

func TestCheckPolicy_AgreeingTablePasses(t *testing.T) {
	testkit.CheckPolicy(t, agentrt.DefaultPolicy(), classTools(), []testkit.PolicyCase{
		{Name: "read", Tool: "read", Want: agentrt.Allow},
		{Name: "write", Tool: "write", Args: `{"n":1}`, Want: agentrt.Allow, Reason: "local_mutation"},
		{Name: "push", Tool: "push", Want: agentrt.RequireApproval, Kind: "remote_mutation", Capability: `"tool":"push"`, Presentation: `"args":{}`},
		{Name: "wipe", Tool: "wipe", Want: agentrt.Deny},
		{Name: "unknown property", Tool: "read", Args: `{"path":"x"}`, Want: testkit.Refused},
		{Name: "unknown tool", Tool: "nope", Want: testkit.Refused},
		{Name: "not JSON", Tool: "read", Args: `{`, Want: testkit.Refused},
	})
}

// Disagreements come back as one table naming the case, the request, and
// what the policy said; the refusal reason is the runtime's own.
func TestCheckPolicy_DisagreementsAreOneReadableTable(t *testing.T) {
	r := &recorder{TB: t}
	testkit.CheckPolicy(r, agentrt.DefaultPolicy(), classTools(), []testkit.PolicyCase{
		{Name: "read", Tool: "read", Want: agentrt.Allow},
		{Name: "wipe allowed?", Tool: "wipe", Args: `{"n":2}`, Want: agentrt.Allow},
		{Name: "push kind", Tool: "push", Want: agentrt.RequireApproval, Kind: "publication"},
		{Name: "push shows", Tool: "push", Want: agentrt.RequireApproval, Presentation: `"proposal"`},
		{Name: "typo", Tool: "read", Args: `{"m":1}`, Want: agentrt.Allow},
	})
	got := r.joined()
	for _, want := range []string{"4 of 5 policy cases disagree", "wipe allowed?", `wipe {"n":2}`, "allow", "deny", "push kind", "kind remote_mutation", `presentation lacks "\"proposal\""`, "typo", "refused", "additional properties 'm' not allowed"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "\n") != 6 {
		t.Errorf("table has %d lines, want an intro, a header, and four rows:\n%s", strings.Count(got, "\n"), got)
	}
}

// A policy that errors is reported as such, not as a denial.
func TestCheckPolicy_PolicyErrorIsReported(t *testing.T) {
	r := &recorder{TB: t}
	failing := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{}, errors.New("facts unavailable")
	})
	testkit.CheckPolicy(r, failing, classTools(), []testkit.PolicyCase{{Name: "read", Tool: "read", Want: agentrt.Deny}})
	if !strings.Contains(r.joined(), "error") || !strings.Contains(r.joined(), "facts unavailable") {
		t.Fatalf("errors = %q", r.joined())
	}
}

// The view a case names is what the policy sees, with the request's run
// id matching it.
func TestCheckPolicy_ViewReachesThePolicy(t *testing.T) {
	var views []int
	var runIDs []string
	counting := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, v agentrt.RunView) (agentrt.PolicyDecision, error) {
		views = append(views, len(v.Approvals))
		runIDs = append(runIDs, req.RunID)
		if len(v.Approvals) > 0 {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "granted"}, nil
		}
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "scope"}, nil
	})
	granted := agentrt.RunView{Run: agentrt.Run{ID: "r9"}, Approvals: []agentrt.Approval{{Kind: "scope", Status: agentrt.ApprovalApproved}}}
	testkit.CheckPolicy(t, counting, classTools(), []testkit.PolicyCase{
		{Name: "first ask", Tool: "write", Want: agentrt.RequireApproval, Kind: "scope"},
		{Name: "granted", Tool: "write", View: granted, Want: agentrt.Allow, Reason: "granted"},
	})
	if fmt.Sprint(views) != "[0 1]" || runIDs[1] != "r9" {
		t.Fatalf("views %v run ids %v", views, runIDs)
	}
}

func TestNever_HoldsAndFails(t *testing.T) {
	f := testkit.Fuzz{Count: 8}
	testkit.Never(t, agentrt.DefaultPolicy(), classTools(), agentrt.Destructive, agentrt.Allow, f)
	testkit.Never(t, agentrt.DefaultPolicy(), classTools(), agentrt.Destructive, agentrt.Allow, f, agentrt.RunView{}, agentrt.RunView{Approvals: []agentrt.Approval{{Kind: "x", Status: agentrt.ApprovalApproved}}})
	r := &recorder{TB: t}
	testkit.Never(r, agentrt.DefaultPolicy(), classTools(), agentrt.ReadOnly, agentrt.Allow, f)
	if len(r.errors) != 8 || !strings.Contains(r.errors[0], "read is allow, which a read_only tool must never be") {
		t.Fatalf("errors = %q", r.joined())
	}
	r = &recorder{TB: t}
	testkit.Never(r, agentrt.DefaultPolicy(), classTools()[:1], agentrt.Destructive, agentrt.Allow, f)
	if r.joined() != "testkit: no tool has side effect destructive" {
		t.Fatalf("errors = %q", r.joined())
	}
}

// ---- fuzz -----------------------------------------------------------------

// The consumers' schemas, verbatim: casework's conclude and
// get_processing_history, repo-steward's write_file and search_files.
const (
	concludeSchema = `{"type":"object","properties":{
"outcome":{"type":"string","enum":["corrected","explained_no_action","needs_clarification","escalate","blocked"]},
"summary":{"type":"string","minLength":1,"maxLength":2000},
"findings":{"type":"array","maxItems":20,"items":{"type":"object","properties":{"claim":{"type":"string","minLength":1,"maxLength":500},"kind":{"type":"string","enum":["observed","inferred","missing"]},"evidence_refs":{"type":"array","maxItems":10,"items":{"type":"string","maxLength":100}}},"required":["claim","kind","evidence_refs"],"additionalProperties":false}},
"next_step":{"type":"string","maxLength":1000},
"draft_id":{"type":"string","maxLength":100}
},"required":["outcome","summary","findings","next_step"],"additionalProperties":false}`
	historySchema = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	writeSchema   = `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`
	searchSchema  = `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","default":"."}},"required":["pattern"],"additionalProperties":false}`
)

func TestFuzz_ValidIsDeterministicAndBounded(t *testing.T) {
	f := testkit.Fuzz{Seed: 7, Count: 10}
	a, err := f.Valid([]byte(concludeSchema))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.Valid([]byte(concludeSchema))
	c, _ := (testkit.Fuzz{Seed: 8, Count: 10}).Valid([]byte(concludeSchema))
	if len(a) != 10 || fmt.Sprint(a) != fmt.Sprint(b) || fmt.Sprint(a) == fmt.Sprint(c) {
		t.Fatalf("a=%d same-seed equal %v, other seed equal %v", len(a), fmt.Sprint(a) == fmt.Sprint(b), fmt.Sprint(a) == fmt.Sprint(c))
	}
	for _, v := range a {
		var c struct {
			Outcome  string
			Summary  string
			Findings []struct{ Kind string }
		}
		if err := json.Unmarshal(v, &c); err != nil || c.Outcome == "" || c.Summary == "" || len(c.Summary) > 2000 || len(c.Findings) > 4 {
			t.Fatalf("value %s: %v", v, err)
		}
	}
}

func TestFuzz_InvalidCoversTheBoundaryAndNearMisses(t *testing.T) {
	inv, err := (testkit.Fuzz{Count: 120}).Invalid([]byte(concludeSchema))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 120 {
		t.Fatalf("%d invalid arguments", len(inv))
	}
	joined := ""
	for _, v := range inv {
		joined += string(v) + "\n"
	}
	for _, want := range []string{`{"_":[[[`, "\"a\xffb\"", `\ud800`, `"unexpected":1`, "not-a-member-", "not-a-member-", `"kind":"not-a-member-`, ` {}`, `"findings":[[]]`} {
		if !strings.Contains(joined, want) {
			t.Errorf("no case with %q", want)
		}
	}
	// A duplicate key: the first member twice over.
	dup := 0
	for _, v := range inv {
		s := string(v)
		if !strings.HasPrefix(s, `{"`) {
			continue
		}
		key := s[1 : strings.Index(s[2:], `"`)+3]
		if strings.Count(s, key+":") >= 3 {
			dup++
		}
	}
	if dup != 1 {
		t.Errorf("duplicate-key cases = %d", dup)
	}
	// Every case is one the schema library refuses, except the three the
	// library decodes lossily and the runtime's boundary exists to catch.
	s := compileSchema(t, concludeSchema)
	for _, v := range inv {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(v))
		if err != nil || s.Validate(doc) != nil {
			continue
		}
		if !bytes.Contains(v, []byte{0xff}) && !bytes.Contains(v, []byte(`\ud800`)) && !strings.Contains(string(v), `"`+strings.SplitN(string(v[2:]), `"`, 2)[0]+`":`+"") {
			t.Errorf("the schema accepts %s", v)
		}
	}
}

func compileSchema(t *testing.T, schema string) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("test.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("test.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A schema with nothing to violate yields the boundary cases alone.
func TestFuzz_InvalidForAnEmptyObjectSchema(t *testing.T) {
	inv, err := (testkit.Fuzz{}).Invalid([]byte(`{"type":"object","additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) < 7 || !strings.Contains(fmt.Sprint(inv), "unexpected") {
		t.Fatalf("%d cases: %s", len(inv), inv)
	}
}

func TestFuzz_UnsupportedKeywordIsNamed(t *testing.T) {
	_, err := (testkit.Fuzz{}).Valid([]byte(`{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-z]+$"}},"required":["id"]}`))
	if err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (testkit.Fuzz{}).Valid([]byte(`{"type":`)); err == nil {
		t.Fatal("a schema that is not JSON was accepted")
	}
}

func schemaTool(name, schema string) *tool {
	tl := newTool(name, agentrt.LocalMutation)
	tl.spec.InputSchema = []byte(schema)
	return tl
}

func TestFuzz_CheckPassesTheConsumersSchemas(t *testing.T) {
	conclude := schemaTool("conclude", concludeSchema)
	conclude.spec.Terminal = true
	(testkit.Fuzz{Count: 16}).Check(t, conclude, schemaTool("get_processing_history", historySchema), schemaTool("write_file", writeSchema), schemaTool("search_files", searchSchema))
	if conclude.calls.Load() != 16 {
		t.Fatalf("the terminal tool was called %d times, want every valid argument", conclude.calls.Load())
	}
}

// A tool that panics on an argument its schema admits is reported with
// the argument; a tool that merely errors is not.
func TestFuzz_CheckReportsAPanicNotAnError(t *testing.T) {
	picky := schemaTool("write_file", writeSchema)
	picky.fn = func(c agentrt.ToolCall) (agentrt.ToolResult, error) {
		var a struct{ Path string }
		json.Unmarshal(c.Args, &a)
		if len(a.Path) > 20 {
			panic("path too long for me")
		}
		return agentrt.ToolResult{}, errors.New("not today")
	}
	r := &recorder{TB: t}
	(testkit.Fuzz{Count: 24}).Check(r, picky)
	if len(r.errors) == 0 || !strings.Contains(r.errors[0], "write_file panicked on") || !strings.Contains(r.errors[0], "path too long") {
		t.Fatalf("errors = %q", r.joined())
	}
	for _, e := range r.errors {
		if strings.Contains(e, "not today") {
			t.Fatalf("a tool error was reported: %s", e)
		}
	}
}

// ---- render ---------------------------------------------------------------

// modelAgent renders like repo-steward's agent: an opening from facts,
// then the recorded steps.
type modelAgent struct {
	opening func(agentrt.StepInput) string
}

func (a modelAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	msgs := []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: a.opening(in)}}}}
	for _, st := range in.Steps {
		if st.Decision == nil {
			continue
		}
		msgs = append(msgs, agentrt.Message{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: fmt.Sprintf("step_%d", st.Index), Name: st.Decision.Tool, Input: st.Decision.Args}}})
		content := "(none)"
		if st.Observation != nil {
			content = string(st.Observation.Content)
		}
		msgs = append(msgs, agentrt.Message{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: fmt.Sprintf("step_%d", st.Index), Content: content}}})
	}
	resp, err := in.Model.Generate(ctx, agentrt.ModelRequest{System: "sys", Messages: msgs, Tools: in.Tools, MaxOutputTokens: 100})
	if err != nil {
		return agentrt.Decision{}, err
	}
	_ = resp
	return agentrt.Decision{Kind: agentrt.KindNoToolCall}, nil
}

func TestRendered_ReturnsTheRequestTheAgentMade(t *testing.T) {
	a := modelAgent{opening: func(in agentrt.StepInput) string { return "goal " + in.Run.Goal }}
	in := agentrt.StepInput{Run: agentrt.Run{Goal: "g"}, Steps: []agentrt.Step{{Index: 0, Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "read", Args: []byte(`{}`)}}}}
	req := testkit.Rendered(t, a, in)
	if req.System != "sys" || len(req.Messages) != 3 || req.Messages[0].Content[0].Text != "goal g" || req.MaxOutputTokens != 100 {
		t.Fatalf("request = %+v", req)
	}
	r := &recorder{TB: t}
	defer func() {
		if recover() == nil {
			t.Fatal("a scripted agent made no request and nothing failed")
		}
	}()
	testkit.Rendered(fatalRecorder{r}, &scripted.Agent{Decisions: []agentrt.Decision{scripted.Complete(`{}`)}}, agentrt.StepInput{})
}

// fatalRecorder turns the kit's Fatalf into a panic the test recovers.
type fatalRecorder struct{ *recorder }

func (f fatalRecorder) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }

func deterministic(sc testkit.Scenario) testkit.Scenario {
	ids := 0
	sc.NewID = func() string { ids++; return fmt.Sprintf("id-%d", ids) }
	at := time.Unix(1700000000, 0).UTC()
	sc.Now = func() time.Time { at = at.Add(time.Second); return at }
	return sc
}

func TestSameRenders_HoldsAcrossAPauseAndACrash(t *testing.T) {
	fresh := func() (testkit.Scenario, agentrt.Agent) {
		push, write := newTool("push", agentrt.RemoteMutation), newTool("write", agentrt.LocalMutation)
		sc := deterministic(scenario(write, push))
		sc.Agent = &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("write", `{"n":1}`, ""), scripted.ToolCall("push", `{"n":2}`, ""), scripted.Complete(`{}`)}}
		return sc, modelAgent{opening: func(in agentrt.StepInput) string { return fmt.Sprintf("run %s step %d", in.Run.ID, in.Run.StepCount) }}
	}
	testkit.SameRenders(t, fresh)
	testkit.SameRenders(t, fresh, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})
}

// An opening that reads the wall clock renders differently, and the kit
// names the message.
func TestSameRenders_FailsOnANondeterministicOpening(t *testing.T) {
	n := 0
	fresh := func() (testkit.Scenario, agentrt.Agent) {
		n++
		run := n
		return deterministic(scenario(newTool("read", agentrt.ReadOnly))), modelAgent{opening: func(agentrt.StepInput) string { return fmt.Sprintf("attempt %d", run) }}
	}
	r := &recorder{TB: t}
	testkit.SameRenders(r, fresh)
	if len(r.errors) != 2 || !strings.Contains(r.errors[0], "decision 0 renders differently") || !strings.Contains(r.errors[0], "message 0") || !strings.Contains(r.errors[0], "attempt 1") {
		t.Fatalf("errors = %q", r.joined())
	}
}
