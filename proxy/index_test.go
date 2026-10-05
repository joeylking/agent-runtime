package proxy

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

func mustKey(t *testing.T, tool, args string) string {
	t.Helper()
	key, ok := requestKey(tool, []byte(args))
	if !ok {
		t.Fatalf("%s %s has no key", tool, args)
	}
	return key
}

// TestIndex_RowWithoutARunIsBegunUnderItsID: a call that claimed its row
// and stopped before beginning its run left nothing started, so the next
// identical call begins that run, under the row's id, and the row stays
// the key's open row.
func TestIndex_RowWithoutARunIsBegunUnderItsID(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	args := `{"to":"xia","amount":2}`
	key := mustKey(t, "bank_pay", args)
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, "test.never-begun", nil, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	runs := h.runs()
	if len(runs) != 1 || runs[0].ID != "test.never-begun" {
		t.Fatalf("runs = %+v, want the row's own run only", runs)
	}
	r, err := h.p.idx.byRun(ctx, "test.never-begun")
	if err != nil || !r.open {
		t.Fatalf("the row: %+v %v", r, err)
	}
}

// TestIndex_OneCallBeginsARow: of two calls that would begin the same
// row's run, the store lets one: the other's Begin fails, and a call that
// finds the run begun and leased is told it is in progress. A claim from a
// stale read of the open row fails as well.
func TestIndex_OneCallBeginsARow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	args := `{"to":"yan","amount":1}`
	key := mustKey(t, "bank_pay", args)
	now := h.clock.now()
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, "test.first", nil, now); err != nil {
		t.Fatal(err)
	}
	s, err := h.p.gate.Begin(ctx, "test.first", "call bank_pay", h.p.limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := h.p.gate.Begin(ctx, "test.first", "call bank_pay", h.p.limits); err == nil {
		t.Fatal("a second Begin of the row's run succeeded")
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixInProgress, false)
	if n := len(h.runs()); n != 1 {
		t.Fatalf("%d runs", n)
	}
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, "test.third", nil, now); !errors.Is(err, errRace) {
		t.Fatalf("claim from a stale read: %v", err)
	}
}

// TestIndex_ALateBeginLeavesNoRunBehind: a call that claimed its row and
// stalled before Begin, while the next identical call took the row over,
// must not leave a run running that nothing will end. Before, the second
// call closed and settled the first's row and began a run of its own; the
// first then began its run, found its row closed, and a crash before it
// ended that run left it running for good, read by no rule. Now the second
// call begins the row's own run, and the first's Begin fails.
func TestIndex_ALateBeginLeavesNoRunBehind(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	args := `{"to":"zoe","amount":3}`
	key := mustKey(t, "bank_pay", args)
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, "test.stalled", nil, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	// The stalled call wakes, begins, and dies without ending anything.
	if s, err := h.p.gate.Begin(ctx, "test.stalled", "call bank_pay", h.p.limits); err == nil {
		s.Close()
		t.Fatal("the stalled call began a run of its own")
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	if n := len(h.runs()); n != 1 {
		t.Fatalf("%d runs, want one", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestIndex_RowWithoutARunIsSettledWhenFound: a row whose run never began
// is settled the first time a scan finds it, so it is not read again on
// every mutating call, and unsettled again by the call that begins its run.
func TestIndex_RowWithoutARunIsSettledWhenFound(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	args := `{"to":"ann","amount":4}`
	key := mustKey(t, "bank_pay", args)
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, "test.empty", nil, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	wantPrefix(t, h.call("bank_pay", `{"to":"ben","amount":5}`), PrefixPending, false)
	r, err := h.p.idx.byRun(ctx, "test.empty")
	if err != nil || r.settled.IsZero() || r.outcome != outcomeNone {
		t.Fatalf("the empty row after a scan: %+v %v", r, err)
	}
	rows, err := h.p.idx.unsettled(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rows {
		if u.runID == "test.empty" {
			t.Fatal("the empty row is still scanned")
		}
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	if r, err = h.p.idx.byRun(ctx, "test.empty"); err != nil || !r.settled.IsZero() {
		t.Fatalf("the row once its run began: %+v %v", r, err)
	}
}

// TestProxy_AbandonedCallIsEndedAndRunFresh: a call whose process went
// away before its tool ran left a run with nothing in it to execute. The
// next identical call ends that run, recording why, and runs the request
// in a run of its own.
func TestProxy_AbandonedCallIsEndedAndRunFresh(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	args := `{"account":"zed"}`
	key := mustKey(t, "bank_balance", args)
	if err := h.p.idx.claim(ctx, "test", key, "bank_balance", agentrt.ReadOnly, "test.abandoned", nil, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	s, err := h.p.gate.Begin(ctx, "test.abandoned", "call bank_balance", h.p.limits)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	if v, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "bank_balance", Args: []byte(args), Origin: agentrt.OriginModel}); err != nil || v.Outcome != agentrt.VerdictAllowed {
		t.Fatalf("propose: %+v %v", v, err)
	}
	s.Close()
	if res := h.call("bank_balance", args); res.IsError || !strings.Contains(resultString(res), "zed") {
		t.Fatalf("after the abandoned call: %q", resultString(res))
	}
	old, err := h.p.store.GetRun(ctx, "test.abandoned")
	if err != nil || old.Status != agentrt.StatusFailed || !strings.Contains(old.ReasonDetail, "taken over after an interruption") {
		t.Fatalf("the abandoned run: %+v %v", old, err)
	}
	if n := len(h.runs()); n != 2 {
		t.Fatalf("%d runs, want the abandoned one and a fresh one", n)
	}
	settledOrWaiting(t, h.p.store)
}
