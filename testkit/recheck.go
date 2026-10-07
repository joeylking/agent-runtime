package testkit

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
)

// Recheck re-checks a run under p with agentrt.Recheck and reports every
// evaluation that differs with t.Errorf, the report's text with it. Under
// the policy the run ran with it is a determinism check: a policy that
// decides from what it is handed alone decides the same again, and one
// that reads a clock, a file, a counter, or a service may not. The report
// is returned regardless.
func Recheck(t testing.TB, store *agentrt.Store, runID string, p agentrt.Policy) agentrt.RecheckReport {
	t.Helper()
	return RecheckDiffers(t, store, runID, p)
}

// RecheckDiffers re-checks a run under p and reports, with t.Errorf, unless
// the steps whose evaluations differ are exactly steps, by index: the way
// to assert what a second policy would have decided otherwise. A step
// evaluated twice, before and after an approval, is named once.
func RecheckDiffers(t testing.TB, store *agentrt.Store, runID string, p agentrt.Policy, steps ...int) agentrt.RecheckReport {
	t.Helper()
	rep, err := agentrt.Recheck(context.Background(), store, runID, p, agentrt.RecheckOptions{})
	if err != nil {
		t.Errorf("testkit: recheck run %s: %v", runID, err)
		return rep
	}
	var got []int
	for _, e := range rep.Evaluations {
		if e.Result == agentrt.RecheckDifferent && !slices.Contains(got, e.StepIndex) {
			got = append(got, e.StepIndex)
		}
	}
	want := slices.Clone(steps)
	slices.Sort(got)
	slices.Sort(want)
	want = slices.Compact(want)
	if !slices.Equal(got, want) {
		var b strings.Builder
		trace.WriteRecheck(&b, rep)
		t.Errorf("testkit: recheck run %s: steps %v differ, want %v:\n%s", runID, fmtSteps(got), fmtSteps(want), b.String())
	}
	return rep
}

func fmtSteps(steps []int) string {
	if len(steps) == 0 {
		return "none"
	}
	out := make([]string, len(steps))
	for i, n := range steps {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ", ")
}

// recheck re-checks the scenario's run under its own policy, when the
// scenario asks for it.
func (h *harness) recheck(res Result) {
	h.t.Helper()
	if !h.sc.Recheck || res.RunID == "" || h.sc.Policy == nil {
		return
	}
	st, err := agentrt.OpenStore(h.path)
	if err != nil {
		h.t.Fatalf("testkit: open %s: %v", h.path, err)
	}
	defer st.Close()
	Recheck(h.t, st, res.RunID, h.sc.Policy)
}
