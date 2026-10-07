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

// A re-check that proves nothing fails Recheck, RecheckDiffers, and a
// scenario's Recheck: a run whose evaluations were recorded before v0.5.0,
// a run whose only evaluation met a policy error, and a run that asked the
// policy nothing. RecheckPartial passes all three.
func TestRecheck_FailsWhenNothingWasRechecked(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, p agentrt.Policy, decisions ...agentrt.Decision) (*agentrt.Store, string) {
		st := openDB(t, filepath.Join(t.TempDir(), "kit.db"))
		d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: p, Agent: &scripted.Agent{Decisions: decisions},
			Tools: []agentrt.Tool{newTool("read", agentrt.ReadOnly)}})
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.Start(ctx, "g", agentrt.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return st, run.ID
	}
	failing := agentrt.PolicyFunc(func(context.Context, agentrt.ToolRequest, agentrt.RunView) (agentrt.PolicyDecision, error) {
		return agentrt.PolicyDecision{}, fmt.Errorf("rules unavailable")
	})

	old, oldID := start(t, agentrt.DefaultPolicy(), scripted.ToolCall("read", `{}`, ""), scripted.Complete(`{}`))
	// What v0.4.0 recorded: a step.policy with neither spec nor view.
	if _, err := old.DB().Exec(`UPDATE events SET payload_json = json_remove(payload_json, '$.view', '$.spec_hash') WHERE type = 'step.policy'`); err != nil {
		t.Fatal(err)
	}
	errored, erroredID := start(t, failing, scripted.ToolCall("read", `{}`, ""))
	empty, emptyID := start(t, agentrt.DefaultPolicy(), scripted.Complete(`{}`))

	for _, c := range []struct {
		name  string
		st    *agentrt.Store
		runID string
		want  string
	}{
		{"recorded before v0.5.0", old, oldID, "1 evaluation(s) not re-checkable"},
		{"policy error", errored, erroredID, "1 evaluation(s) not re-checkable"},
		{"no evaluation", empty, emptyID, "no evaluation was re-checked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for name, check := range map[string]func(testing.TB) agentrt.RecheckReport{
				"Recheck": func(tb testing.TB) agentrt.RecheckReport {
					return testkit.Recheck(tb, c.st, c.runID, agentrt.DefaultPolicy())
				},
				"RecheckDiffers": func(tb testing.TB) agentrt.RecheckReport {
					return testkit.RecheckDiffers(tb, c.st, c.runID, agentrt.DefaultPolicy())
				},
			} {
				r := &recorder{TB: t}
				rep := check(r)
				if len(r.errors) != 1 || !strings.Contains(r.errors[0], c.want) || !rep.Same() {
					t.Fatalf("%s: failures:\n%s", name, r.joined())
				}
			}
			r := &recorder{TB: t}
			if testkit.RecheckPartial(r, c.st, c.runID, agentrt.DefaultPolicy()); len(r.errors) != 0 {
				t.Fatalf("RecheckPartial: failures:\n%s", r.joined())
			}
		})
	}

	// The policy error is listed, not passed over in silence.
	rep := testkit.RecheckPartial(t, errored, erroredID, agentrt.DefaultPolicy())
	if len(rep.Evaluations) != 1 || rep.Complete() || rep.Evaluations[0].Detail != "policy failed; no evaluation recorded: rules unavailable" {
		t.Fatalf("report %+v", rep)
	}

	// A scenario whose policy fails at its only evaluation fails its
	// Recheck.
	sc := scenario(newTool("read", agentrt.ReadOnly))
	sc.Policy, sc.Recheck, sc.DB = failing, true, filepath.Join(t.TempDir(), "kit.db")
	sc.Agent = &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{}`, "")}}
	r := &recorder{TB: t}
	testkit.Run(r, sc)
	if !strings.Contains(r.joined(), "1 evaluation(s) not re-checkable") {
		t.Fatalf("failures:\n%s", r.joined())
	}
}
