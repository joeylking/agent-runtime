package testkit_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/testkit"
)

// publishing is a scenario that reads, publishes under an approval the
// operator grants, and writes, in a file of the test's own.
func publishing(t *testing.T, p agentrt.Policy) testkit.Scenario {
	return testkit.Scenario{
		Tools:  []agentrt.Tool{newTool("read", agentrt.ReadOnly), newTool("publish", agentrt.RemoteMutation), newTool("write", agentrt.LocalMutation)},
		Policy: p, Goal: "publish", DB: filepath.Join(t.TempDir(), "kit.db"),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{
			scripted.ToolCall("read", `{"n":1}`, ""),
			scripted.ToolCall("publish", `{"n":2}`, ""),
			scripted.ToolCall("write", `{"n":3}`, ""),
			scripted.Complete(`{}`),
		}},
	}
}

func openDB(t *testing.T, path string) *agentrt.Store {
	t.Helper()
	st, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A scenario with Recheck re-checks its run under its own policy: a
// policy that decides from what it is handed passes, the resume included.
func TestRecheck_ScenarioPolicyThatDecidesFromItsInputPasses(t *testing.T) {
	sc := publishing(t, agentrt.DefaultPolicy())
	sc.Recheck = true
	res := testkit.Run(t, sc)
	if res.Run.Status != agentrt.StatusCompleted {
		t.Fatalf("run: %s", res)
	}
	rep := testkit.Recheck(t, openDB(t, sc.DB), res.RunID, sc.Policy)
	if rep.Rechecked() != 4 || !rep.Same() {
		t.Fatalf("report %+v", rep)
	}
}

// A policy that reads state of its own, here a count of what it has
// allowed, decides otherwise when asked again, and the scenario fails.
func TestRecheck_PolicyThatReadsOutsideStateFails(t *testing.T) {
	var allowed atomic.Int32
	quota := agentrt.PolicyFunc(func(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		if allowed.Add(1) > 4 {
			return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "quota spent"}, nil
		}
		return agentrt.DefaultPolicy().Evaluate(ctx, req, view)
	})
	sc := publishing(t, quota)
	sc.Recheck = true
	r := &recorder{TB: t}
	res := testkit.Run(r, sc)
	if res.Run.Status != agentrt.StatusCompleted {
		t.Fatalf("run: %s", res)
	}
	if len(r.errors) != 1 || !strings.Contains(r.joined(), "steps 0, 1, 2 differ, want none") || !strings.Contains(r.joined(), "DIFFERENT step 0 seq") {
		t.Fatalf("failures:\n%s", r.joined())
	}
}

// RecheckDiffers asserts which steps a second policy would have decided
// otherwise, and reports a wrong expectation with the report.
func TestRecheckDiffers_NamesTheStepsASecondPolicyChanges(t *testing.T) {
	sc := publishing(t, agentrt.DefaultPolicy())
	res := testkit.Run(t, sc)
	st := openDB(t, sc.DB)
	strict := agentrt.SideEffectPolicy{agentrt.ReadOnly: agentrt.Allow}
	rep := testkit.RecheckDiffers(t, st, res.RunID, strict, 1, 2)
	if rep.Differences() != 3 {
		t.Fatalf("%d differences: the publish twice and the write", rep.Differences())
	}
	r := &recorder{TB: t}
	testkit.RecheckDiffers(r, st, res.RunID, strict, 2)
	if len(r.errors) != 1 || !strings.Contains(r.errors[0], "steps 1, 2 differ, want 2") {
		t.Fatalf("failures:\n%s", r.joined())
	}
}

// A side effect lost to a crash pauses for an operator: that step.policy
// is the runtime's, not the policy's, and is reported as no policy
// evaluation, between the step's first evaluation and the one on resume.
func TestRecheck_InterruptedSideEffectPauseIsNotAPolicyEvaluation(t *testing.T) {
	write := newTool("write", agentrt.LocalMutation)
	sc := scenario(write)
	sc.DB = filepath.Join(t.TempDir(), "kit.db")
	sc.Recheck = true
	res := testkit.Run(t, sc, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})
	rep := testkit.Recheck(t, openDB(t, sc.DB), res.RunID, sc.Policy)
	var got []string
	for _, e := range rep.Evaluations {
		got = append(got, fmt.Sprintf("%d %s %v %s", e.StepIndex, e.Result, e.OnResume, e.Detail))
	}
	want := []string{
		"0 same false ",
		"0 not_a_policy_evaluation false not a policy evaluation: an interrupted side effect's pause",
		"0 same true ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("evaluations:\n%s", strings.Join(got, "\n"))
	}
}
