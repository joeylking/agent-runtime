package trace_test

import (
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
)

// Differences come first, each evaluation is one line whatever the record
// or the policy put in it, and every such value is sanitized and cut.
func TestWriteRecheck_OneSanitizedLinePerEvaluationDifferencesFirst(t *testing.T) {
	rep := agentrt.RecheckReport{RunID: "run\x1b[31m", RunStatus: agentrt.StatusCompleted, PolicyID: "p2", Identity: agentrt.IdentityUnknown, SpecSource: agentrt.SpecCurrent,
		Notes: []string{"a note\nover two lines"},
		Evaluations: []agentrt.RecheckedEvaluation{
			{StepIndex: 0, Seq: 5, Tool: "read", Result: agentrt.RecheckSame, Recorded: agentrt.PolicyDecision{Outcome: agentrt.Allow}},
			{StepIndex: 1, Seq: 9, Tool: "pay\u202e", Result: agentrt.RecheckNotEvaluation, Detail: "not a policy evaluation: a reconciliation's pause"},
			{StepIndex: 2, Seq: 14, Tool: "send", OnResume: true, Result: agentrt.RecheckDifferent, Recorded: agentrt.PolicyDecision{Outcome: agentrt.Allow},
				Rechecked: &agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "line one\nline two " + strings.Repeat("x", 300)}},
			{StepIndex: 3, Seq: 20, Tool: "send", Result: agentrt.RecheckDifferent, Recorded: agentrt.PolicyDecision{Outcome: agentrt.Allow}, Detail: "policy failed: down"},
		}}
	var b strings.Builder
	if err := trace.WriteRecheck(&b, rep); err != nil {
		t.Fatal(err)
	}
	want := `run run\x1b[31m COMPLETED: 2 of 3 re-checked evaluations differ; policy identity unknown; current specs
DIFFERENT step 2 seq 14 send (on resume): allow -> deny: line one line two ` + strings.Repeat("x", 240-len("line one line two ")) + `…
DIFFERENT step 3 seq 20 send: allow -> none: policy failed: down
same      step 0 seq 5 read: allow
skipped   step 1 seq 9 pay\u{202e}: not a policy evaluation: a reconciliation's pause
note: a note over two lines
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}
