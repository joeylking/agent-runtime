package agentrt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// The fixture's raw input: pretty-printed, keys out of order and nested,
// '&', '<', '>', U+2028, non-ASCII written as is and as an escape, and
// numbers whose literal text is not their shortest form.
const (
	rawPolicyArgs    = "{\n  \"z\": {\"b\": 1.50, \"a\": \"make && rm <x> \u2028 é \\u00e9 日本\"},\n  \"n\": 1e3\n}"
	wantPolicyArgs   = "{\"n\":1e3,\"z\":{\"a\":\"make && rm <x> \u2028 é é 日本\",\"b\":1.50}}"
	rawPolicySchema  = "{\n  \"type\": \"object\",\n  \"description\": \"a && b <c> \u2028 é\",\n  \"properties\": {\"z\": {\"type\": \"object\"}, \"n\": {\"type\": \"number\", \"maximum\": 1e4, \"multipleOf\": 0.50}}\n}"
	wantPolicySchema = "{\"description\":\"a && b <c> \u2028 é\",\"properties\":{\"n\":{\"maximum\":1e4,\"multipleOf\":0.50,\"type\":\"number\"},\"z\":{\"type\":\"object\"}},\"type\":\"object\"}"
)

// PolicyJSON sorts keys, drops insignificant whitespace, keeps numbers'
// literal text, and escapes only what JSON requires: never '<', '>', '&',
// U+2028, or U+2029, which CanonicalJSON escapes. Both forms of a value
// have the same canonical form, and PolicyJSON refuses what CanonicalJSON
// refuses.
func TestPolicyJSON_Form(t *testing.T) {
	for raw, want := range map[string]string{
		rawPolicyArgs:                 wantPolicyArgs,
		rawPolicySchema:               wantPolicySchema,
		`"\u003c\u003e\u0026"`:        `"<>&"`,
		`"a\u2028b\u2029"`:            "\"a\u2028b\u2029\"",
		`"q\"\\ \n\t\u0001 \u007f /"`: `"q\"\\ \n\t\u0001 ` + "\x7f" + ` /"`,
		`[10, 10.0, 1e1, -0, 1E+2]`:   `[10,10.0,1e1,-0,1E+2]`,
		`{"b": {"d": 1, "c": [true, null]}, "a": "\ud83d\ude42"}`: `{"a":"🙂","b":{"c":[true,null],"d":1}}`,
	} {
		got, err := agentrt.PolicyJSON(json.RawMessage(raw))
		if err != nil || string(got) != want {
			t.Fatalf("PolicyJSON(%q) = %q, %v; want %q", raw, got, err, want)
		}
		c1, err1 := agentrt.CanonicalJSON(json.RawMessage(raw))
		c2, err2 := agentrt.CanonicalJSON(got)
		if err1 != nil || err2 != nil || !bytes.Equal(c1, c2) {
			t.Fatalf("%q: canonical forms differ: %q %q %v %v", raw, c1, c2, err1, err2)
		}
		again, err := agentrt.PolicyJSON(c1)
		if err != nil || !bytes.Equal(again, got) {
			t.Fatalf("%q: from the canonical form %q, from the raw %q", raw, again, got)
		}
	}
	for _, bad := range []string{``, `{"a":1,"a":2}`, `{`, "\"\xff\"", `"\ud800"`, `1e99999`} {
		if _, err := agentrt.PolicyJSON(json.RawMessage(bad)); err == nil {
			t.Fatalf("PolicyJSON(%q) was accepted", bad)
		}
	}
}

// handedPolicy keeps the bytes each request handed it carried, and denies
// arguments that hold "&&" as bytes, as a policy reading them unparsed
// would.
type handedPolicy struct {
	args, schemas []string
}

func (p *handedPolicy) Evaluate(_ context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	p.args, p.schemas = append(p.args, string(req.Args)), append(p.schemas, string(req.Spec.InputSchema))
	if bytes.Contains(req.Args, []byte("&&")) && req.Spec.SideEffect == agentrt.ReadOnly {
		return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "chained shell"}, nil
	}
	if req.Spec.SideEffect == agentrt.RemoteMutation {
		for _, a := range view.Approvals {
			if a.StepID == req.StepID && a.Status == agentrt.ApprovalApproved {
				return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
			}
		}
		return agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: "send"}, nil
	}
	return agentrt.PolicyDecision{Outcome: agentrt.Allow}, nil
}

func (p *handedPolicy) PolicyID() string { return "handed/v1" }

// policyFormRun runs one call of a tool of the side effect given, with the
// fixture's raw schema and arguments, under p.
func policyFormRun(t *testing.T, se agentrt.SideEffect, p agentrt.Policy) (*agentrt.Store, *agentrt.Driver, agentrt.Run) {
	t.Helper()
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tool := newTool("sh", se, rawPolicySchema, func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: json.RawMessage(`{"ok":true}`), Summary: "ok"}, nil
	})
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: []agentrt.Tool{tool},
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("sh", rawPolicyArgs, ""), scripted.Complete(`{}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	return st, d, run
}

// The policy is handed PolicyJSON of what the agent wrote and of the
// schema the tool registered, and the same bytes come from what the
// runtime stored: the decision, compacted and HTML-escaped, and the spec
// in canonical form.
func TestPolicyJSON_SameFromRawAndStored(t *testing.T) {
	p := &handedPolicy{}
	st, _, run := policyFormRun(t, agentrt.ReadOnly, p)
	ctx := context.Background()
	if len(p.args) != 1 || p.args[0] != wantPolicyArgs || p.schemas[0] != wantPolicySchema {
		t.Fatalf("the policy was handed %q and %q", p.args, p.schemas)
	}
	steps, err := st.ListSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored := steps[0].Decision.Args
	spec, err := st.ToolSpec(ctx, steps[0].SpecHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`\u0026\u0026`)) || !bytes.Contains(spec.InputSchema, []byte(`\u0026\u0026`)) || strings.Contains(string(spec.InputSchema), "\n") {
		t.Fatalf("the stored forms are not the escaped compact ones this test means to cover: %s %s", stored, spec.InputSchema)
	}
	for name, raw := range map[string][]byte{"raw arguments": []byte(rawPolicyArgs), "stored arguments": stored} {
		if got, err := agentrt.PolicyJSON(raw); err != nil || string(got) != wantPolicyArgs {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	for name, raw := range map[string][]byte{"raw schema": []byte(rawPolicySchema), "stored schema": spec.InputSchema} {
		if got, err := agentrt.PolicyJSON(raw); err != nil || string(got) != wantPolicySchema {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
}

// A policy that reads the arguments' bytes, denying "&&", denied the call
// live and denies it again on a re-check, which rebuilds the request from
// the escaped record: the same.
func TestRecheck_ByteReadingPolicyDecidesAlike(t *testing.T) {
	p := &handedPolicy{}
	st, _, run := policyFormRun(t, agentrt.ReadOnly, p)
	steps, _ := st.ListSteps(context.Background(), run.ID)
	if steps[0].Policy == nil || steps[0].Policy.Outcome != agentrt.Deny {
		t.Fatalf("live: %+v", steps[0].Policy)
	}
	again := &handedPolicy{}
	rep := recheck(t, st, run.ID, again, agentrt.RecheckOptions{})
	requireLines(t, rep, "0 sh same deny->deny")
	if !rep.Same() || !rep.Complete() || again.args[0] != p.args[0] {
		t.Fatalf("re-check handed %q, live %q", again.args, p.args)
	}
}

// A pretty-printed schema is handed in one form live and on a re-check,
// the recorded spec's and, with RecheckOptions.Tools, the current tool's.
func TestRecheck_PrettySchemaHandedAlike(t *testing.T) {
	p := &handedPolicy{}
	st, _, run := policyFormRun(t, agentrt.LocalMutation, p)
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	recorded := &handedPolicy{}
	if rep := recheck(t, st, run.ID, recorded, agentrt.RecheckOptions{}); !rep.Same() || rep.Rechecked() != 1 {
		t.Fatalf("re-check:\n%s", lines(rep))
	}
	current := &handedPolicy{}
	tool := newTool("sh", agentrt.LocalMutation, rawPolicySchema, nil)
	if rep := recheck(t, st, run.ID, current, agentrt.RecheckOptions{Tools: []agentrt.Tool{tool}}); !rep.Same() || rep.Rechecked() != 1 {
		t.Fatalf("re-check with current tools:\n%s", lines(rep))
	}

	for _, got := range []*handedPolicy{recorded, current} {
		if got.schemas[0] != p.schemas[0] || got.schemas[0] != wantPolicySchema || got.args[0] != p.args[0] {
			t.Fatalf("handed %q %q, live %q %q", got.schemas, got.args, p.schemas, p.args)
		}
	}
}

// One request is handed to the policy in the same bytes in the loop, where
// the agent's raw arguments are at hand, and on resume, where only the
// approval's stored request is.
func TestPolicy_LoopAndResumeHandedTheSameBytes(t *testing.T) {
	p := &handedPolicy{}
	st, d, run := policyFormRun(t, agentrt.RemoteMutation, p)
	requireStatus(t, run, agentrt.StatusWaitingForApproval, "")
	run = approveAndResume(t, d, st, run, "op")
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if len(p.args) != 2 || p.args[0] != p.args[1] || p.schemas[0] != p.schemas[1] || p.args[0] != wantPolicyArgs {
		t.Fatalf("loop and resume were handed %q and %q", p.args, p.schemas)
	}
}

// Approval and content hashes are computed over the canonical form, which
// the policy form leaves alone: an approval asked for a call made with the
// fixture's raw, pretty arguments has the hash it had before the policy
// form existed, pinned here, and so does the tool's result.
func TestApprovalHash_UnchangedByThePolicyForm(t *testing.T) {
	st, _, run := policyFormRun(t, agentrt.RemoteMutation, agentrt.DefaultPolicy())
	ctx := context.Background()
	approvals, err := st.ListApprovals(ctx, run.ID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals %v %v", approvals, err)
	}
	const want = "8441b46fd5ab497a17788f45d057d8ec122a0bbba20f39f3d45fa37061479a6b"
	if approvals[0].Hash != want {
		t.Fatalf("approval hash %s, want %s", approvals[0].Hash, want)
	}
	if string(approvals[0].Request.Args) != string(mustCanonicalEscaped(t, rawPolicyArgs)) {
		t.Fatalf("the stored request's arguments: %s", approvals[0].Request.Args)
	}
	// DefaultPolicy copies the arguments it was handed into the capability:
	// in the policy form, keys sorted, which the hash does not see.
	var c struct{ Args json.RawMessage }
	if err := json.Unmarshal(approvals[0].Capability, &c); err != nil {
		t.Fatal(err)
	}
	if got, _ := agentrt.PolicyJSON(c.Args); string(got) != wantPolicyArgs || !bytes.HasPrefix(c.Args, []byte(`{"n":1e3,`)) {
		t.Fatalf("the capability's arguments: %s", c.Args)
	}
}

// mustCanonicalEscaped is raw as the runtime stores a decision's
// arguments: compacted and HTML-escaped, its keys in the agent's order.
func mustCanonicalEscaped(t *testing.T, raw string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(json.RawMessage(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Through a gate, Propose and the Attach of the approved request hand the
// policy the same bytes as the loop and its resume do, and a re-check
// hands them again.
func TestPolicy_GateProposeAndAttachHandedTheSameBytes(t *testing.T) {
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &handedPolicy{}
	tool := newTool("sh", agentrt.RemoteMutation, rawPolicySchema, func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: json.RawMessage(`{"ok":true}`), Summary: "ok"}, nil
	})
	g, err := agentrt.NewGate(agentrt.GateConfig{Store: st, Policy: p, Now: tick(), NewID: numbered(), Tools: []agentrt.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := g.Begin(ctx, "gated", "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	step, err := s.Step(ctx)
	if err != nil || step == nil {
		t.Fatalf("step: %v", err)
	}
	v, err := step.Propose(ctx, scripted.ToolCall("sh", rawPolicyArgs, ""))
	if err != nil || v.Approval == nil {
		t.Fatalf("propose: %+v %v", v, err)
	}
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
	s.Close()
	if len(p.args) != 2 || p.args[0] != wantPolicyArgs || p.args[1] != wantPolicyArgs || p.schemas[0] != wantPolicySchema || p.schemas[1] != wantPolicySchema {
		t.Fatalf("propose and attach were handed %q and %q", p.args, p.schemas)
	}
	again := &handedPolicy{}
	if rep := recheck(t, st, "gated", again, agentrt.RecheckOptions{}); !rep.Same() || !rep.Complete() || rep.Rechecked() != 2 {
		t.Fatalf("re-check:\n%s", lines(rep))
	}
	if strings.Join(again.args, "|") != strings.Join(p.args, "|") || strings.Join(again.schemas, "|") != strings.Join(p.schemas, "|") {
		t.Fatalf("re-check handed %q %q", again.args, again.schemas)
	}
}
