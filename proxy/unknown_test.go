package proxy

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// timeoutPay lets payments run on the policy alone and gives them a short
// timeout, so a payment in block mode times out after the server has
// recorded it, until the test releases the server.
func timeoutPay(c *Config) {
	p := c.Servers[0].Rules["pay"]
	p.Outcome = agentrt.Allow
	p.Timeout = "300ms"
	c.Servers[0].Rules["pay"] = p
}

// unknownApproval is the one approval waiting, which must ask whether an
// attempt that timed out runs again.
func unknownApproval(t *testing.T, h *harness) agentrt.Approval {
	t.Helper()
	a := h.onlyPending()
	var c capability
	if err := json.Unmarshal(a.Capability, &c); err != nil || a.Kind != agentrt.InterruptedSideEffect || c.Interrupted == nil || c.Interrupted.Ended != endedTimeout {
		t.Fatalf("pending approval %s %s: %v", a.Kind, a.Capability, err)
	}
	return a
}

// TestUnknown_TimeoutAfterTheEffectWaitsForAnOperator: a payment the
// policy allows times out after the server recorded it. The call is told
// its outcome is unknown and that it may have taken effect, not that it
// failed; an identical call inside the window, and long after it, runs
// nothing and is told the same; another payment is blocked; and only an
// operator's approval runs it again, once.
func TestUnknown_TimeoutAfterTheEffectWaitsForAnOperator(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"kim","amount":7,"mode":"block"}`
	res := h.call("bank_pay", args)
	s := wantPrefix(t, res, PrefixUnknown, false)
	if !strings.Contains(s, "may have") || !strings.Contains(s, "timed out") || h.payments() != 1 {
		t.Fatalf("first call: %q, %d payments", s, h.payments())
	}
	a := unknownApproval(t, h)
	if !strings.Contains(s, a.ID) {
		t.Errorf("first call does not name approval %s: %q", a.ID, s)
	}
	want := []string{"run.created", "run.started", "step.started", "step.decided", "step.policy", "step.tool_started", "step.tool_finished", "step.failed",
		"step.started", "step.decided", "step.policy", "approval.requested"}
	if got := eventTypes(t, h.p.store, a.RunID); !slices.Equal(got, want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
	if !strings.Contains(h.logs.String(), "approve "+a.RunID+" -approval "+a.ID) {
		t.Errorf("stderr does not give the operator the command:\n%s", h.logs.String())
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false); !strings.Contains(s, a.ID) {
		t.Errorf("identical call inside the window: %q", s)
	}
	wantPrefix(t, h.call("bank_pay", `{"to":"lou","amount":1}`), PrefixBlocked, true)
	h.clock.add(time.Hour)
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	if n := h.payments(); n != 1 {
		t.Fatalf("%d payments before the operator decided", n)
	}
	os.WriteFile(h.ledger+".release", nil, 0o600)
	decide(t, h.p.store, a, true, "checked: it did not go through")
	if res := h.call("bank_pay", args); res.IsError || resultString(res) != "paid 7.00 to kim" {
		t.Fatalf("the approved re-run: %v %q", res.IsError, resultString(res))
	}
	if n := h.payments(); n != 2 {
		t.Fatalf("%d payments, want the first attempt and the approved re-run", n)
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true)
	if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
		t.Fatalf("another payment once the operator decided: %q", resultString(res))
	}
	s = wantPrefix(t, h.call("gate_status", `{"approval_id":"`+a.ID+`"}`), PrefixStatus, false)
	if !strings.Contains(s, "APPROVED and executed") {
		t.Errorf("status of the re-run's approval: %q", s)
	}
	settledOrWaiting(t, h.p.store)
}

// TestUnknown_RejectionUnblocksAndAnIdenticalCallAsksAgain: rejected, the
// re-run of a payment whose outcome is unknown runs nothing, the tool is
// no longer blocked, the identical call is refused as rejected within the
// window, and after it asks an operator again rather than run on the
// policy alone; so does it after a later rejection.
func TestUnknown_RejectionUnblocksAndAnIdenticalCallAsksAgain(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"max","amount":8,"mode":"block"}`
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	decide(t, h.p.store, unknownApproval(t, h), false, "it went through")
	s := wantPrefix(t, h.call("bank_pay", args), PrefixRejected, true)
	if !strings.Contains(s, "it went through") {
		t.Errorf("rejected = %q", s)
	}
	if res := h.call("bank_pay", `{"to":"ned","amount":1}`); res.IsError || resultString(res) != "paid 1.00 to ned" {
		t.Fatalf("another payment after the rejection: %q", resultString(res))
	}
	// The operator path stamps the wall clock and the proxy's clock runs
	// ahead, so each rejection below is already outside the window.
	for range 3 {
		h.clock.add(11 * time.Minute)
		wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
		decide(t, h.p.store, unknownApproval(t, h), false, "")
	}
	if n := h.payments(); n != 2 {
		t.Fatalf("%d payments, want the first attempt and ned's", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestUnknown_ServerErrorIsAKnownFailure: an error the server returned is
// a failure whose outcome is known: the call is told it failed, no
// operator is asked, the identical call within the window is a duplicate
// carrying the error, and after the window it runs again.
func TestUnknown_ServerErrorIsAKnownFailure(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"oli","amount":2,"mode":"error"}`
	res := h.call("bank_pay", args)
	if !res.IsError || !strings.Contains(resultString(res), "card declined for oli") || h.approvals() != 0 {
		t.Fatalf("error result = %v %q, %d approvals", res.IsError, resultString(res), h.approvals())
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true); !strings.Contains(s, "card declined") {
		t.Errorf("duplicate = %q", s)
	}
	h.clock.add(11 * time.Minute)
	if res := h.call("bank_pay", args); !res.IsError || !strings.Contains(resultString(res), "card declined") {
		t.Fatalf("after the window: %q", resultString(res))
	}
	if n, a := h.payments(), h.approvals(); n != 2 || a != 0 {
		t.Fatalf("%d payments, %d approvals", n, a)
	}
}

// TestUnknown_ClassifiesRecordedErrors: a timeout, a transport or protocol
// error, or any error not known to be the server's answer leaves a
// mutating call's outcome unknown; an isError result, a refusal before
// sending, and content the runtime refused are answers; and a read has no
// effect to repeat, so its timeout is an ordinary failure.
func TestUnknown_ClassifiesRecordedErrors(t *testing.T) {
	h := newHarness(t, nil)
	pay := h.p.tools["bank_pay"]
	balance := h.p.tools["bank_balance"]
	obs := func(failure, text string) agentrt.Observation {
		raw, _ := json.Marshal(map[string]string{"tool": "bank_pay", "error": text, "failure": failure})
		return agentrt.Observation{Kind: agentrt.ObserveToolError, Content: raw}
	}
	for _, c := range []struct {
		t       *tool
		o       agentrt.Observation
		unknown bool
	}{
		{pay, obs("timeout", "context deadline exceeded"), true},
		{pay, obs("error", `mcp: server "bank": tool "pay": connection closed`), true},
		{pay, obs("error", "anything else"), true},
		{pay, obs("error", "bank/pay: 22 bytes, error: card declined for x"), false},
		{pay, obs("error", "bank/pay: 70000 bytes, truncated, error: long"), false},
		{pay, obs("error", `bank_pay: parameter "currency" is fixed by the operator`), false},
		{pay, obs("error", "bank_pay: arguments are not a JSON object: x"), false},
		{pay, obs("invalid_content", "content is not usable JSON"), false},
		{pay, obs("error", "bank/payx: 1 bytes, error: x"), true},
		{balance, obs("timeout", "context deadline exceeded"), false},
	} {
		if _, unknown := h.p.unknownEnding(c.t, c.o); unknown != c.unknown {
			t.Errorf("%s %s: unknown = %v", c.t.name, c.o.Content, unknown)
		}
	}
}

// statusOf is the status tool's answer about one approval.
func statusOf(t *testing.T, h *harness, id string) string {
	t.Helper()
	return wantPrefix(t, h.call("gate_status", `{"approval_id":"`+id+`"}`), PrefixStatus, false)
}

// TestUnknown_StatusSaysWhatCameOfAnApprovedRequest: the status tool
// reports an approved request that executed by what its step recorded: a
// success; a failure the server reported, with its error; and an outcome
// left unknown, naming the approval that now asks whether it runs again.
func TestUnknown_StatusSaysWhatCameOfAnApprovedRequest(t *testing.T) {
	h := newHarness(t, nil)
	wantPrefix(t, h.call("bank_pay", `{"to":"oli","amount":2,"mode":"error"}`), PrefixPending, false)
	declined := h.onlyPending()
	decide(t, h.p.store, declined, true, "")
	if res := h.call("bank_pay", `{"to":"oli","amount":2,"mode":"error"}`); !res.IsError {
		t.Fatalf("the declined payment: %q", resultString(res))
	}
	if s := statusOf(t, h, declined.ID); !strings.Contains(s, "APPROVED and executed, and the server reported a failure: ") ||
		!strings.Contains(s, "card declined for oli") {
		t.Errorf("status of a payment the server declined: %q", s)
	}
	wantPrefix(t, h.call("bank_pay", `{"to":"pia","amount":3}`), PrefixPending, false)
	paid := h.onlyPending()
	decide(t, h.p.store, paid, true, "")
	h.call("bank_pay", `{"to":"pia","amount":3}`)
	if s := statusOf(t, h, paid.ID); !strings.Contains(s, "APPROVED and executed, and it succeeded.") {
		t.Errorf("status of a payment that succeeded: %q", s)
	}

	h = newHarness(t, timeoutPay)
	args := `{"to":"kim","amount":7,"mode":"block"}`
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	first := unknownApproval(t, h)
	decide(t, h.p.store, first, true, "")
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	next := unknownApproval(t, h)
	s := statusOf(t, h, first.ID)
	if !strings.Contains(s, "APPROVED and executed, but it timed out before the server answered, so whether it took effect is unknown") ||
		!strings.Contains(s, "Approval "+next.ID+" now waits") {
		t.Errorf("status of an approved re-run that timed out: %q", s)
	}
}

// TestUnknown_StepLimitKeepsTheToolBlocked: an approved re-run times out,
// and so does a second; the run's step limit ends it, and the question is
// asked again in a fresh run, so the tool stays blocked on an approval an
// operator can act on, and the identical call waits on the same one.
func TestUnknown_StepLimitKeepsTheToolBlocked(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"kim","amount":7,"mode":"block"}`
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	for range 2 {
		decide(t, h.p.store, unknownApproval(t, h), true, "")
		wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	}
	a := unknownApproval(t, h)
	if n := h.payments(); n != 3 {
		t.Fatalf("%d payments, want the attempt and two approved re-runs", n)
	}
	if s := wantPrefix(t, h.call("bank_pay", `{"to":"lou","amount":1}`), PrefixBlocked, true); !strings.Contains(s, a.ID) {
		t.Errorf("another payment: %q, want it blocked on %s", s, a.ID)
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false); !strings.Contains(s, a.ID) {
		t.Errorf("the identical call: %q", s)
	}
	if n := h.payments(); n != 3 {
		t.Fatalf("%d payments, want 3", n)
	}
	decide(t, h.p.store, a, false, "")
	if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
		t.Fatalf("another payment once the operator rejected: %q", resultString(res))
	}
	settledOrWaiting(t, h.p.store)
}

// TestUnknown_CrashBeforeTheReRunKeepsTheToolBlocked: the proxy stops
// after an attempt's timeout is recorded and before it proposes the
// re-run. Another call to the tool recovers the run and finds the unknown
// outcome with nothing waiting: it asks an operator in a fresh run and is
// blocked on that approval, which the identical call then waits on too.
func TestUnknown_CrashBeforeTheReRunKeepsTheToolBlocked(t *testing.T) {
	h := newHarness(t, timeoutPay)
	ctx := context.Background()
	args := `{"to":"kim","amount":7,"mode":"block"}`
	key := mustKey(t, "bank_pay", args)
	runID := "test.crashedbeforererun"
	if err := h.p.idx.claim(ctx, "test", key, "bank_pay", agentrt.RemoteMutation, runID, nil, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	s, err := h.p.gate.Begin(ctx, runID, "call bank_pay", h.p.limits)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Step(ctx)
	if v, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "bank_pay", Args: []byte(args), Origin: agentrt.OriginModel}); err != nil || v.Outcome != agentrt.VerdictAllowed {
		t.Fatalf("propose: %+v %v", v, err)
	}
	if obs, err := st.Execute(ctx); err != nil || obs.Kind != agentrt.ObserveToolError {
		t.Fatalf("execute: %+v %v", obs, err)
	}
	s.Close() // as the crash leaves it, the lease released
	res := h.call("bank_pay", `{"to":"lou","amount":1}`)
	wantPrefix(t, res, PrefixBlocked, true)
	a := unknownApproval(t, h)
	if !strings.Contains(resultString(res), a.ID) || a.RunID == runID {
		t.Fatalf("another payment: %q, approval %s in run %s", resultString(res), a.ID, a.RunID)
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false); !strings.Contains(s, a.ID) {
		t.Errorf("the identical call: %q", s)
	}
	if n := h.payments(); n != 1 {
		t.Fatalf("%d payments before the operator decided", n)
	}
	os.WriteFile(h.ledger+".release", nil, 0o600)
	decide(t, h.p.store, a, true, "")
	if res := h.call("bank_pay", args); res.IsError || resultString(res) != "paid 7.00 to kim" {
		t.Fatalf("the approved re-run: %q", resultString(res))
	}
	if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
		t.Fatalf("another payment once the outcome is known: %q", resultString(res))
	}
	if n := h.payments(); n != 3 {
		t.Fatalf("%d payments, want the attempt, the approved re-run, and lou's", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestUnknown_ExpiryAsksAgainRatherThanUnblocks: an approval about an
// unknown outcome that expires resolves nothing. The next call to the tool
// is blocked on a new approval asking the same question, and the identical
// call waits on that one rather than being refused as expired.
func TestUnknown_ExpiryAsksAgainRatherThanUnblocks(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		timeoutPay(c)
		c.ApprovalTTL = &Duration{time.Minute}
	})
	args := `{"to":"kim","amount":7,"mode":"block"}`
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	first := unknownApproval(t, h)
	h.clock.add(2 * time.Minute)
	s := wantPrefix(t, h.call("bank_pay", `{"to":"lou","amount":1}`), PrefixBlocked, true)
	again := unknownApproval(t, h)
	if again.ID == first.ID || !strings.Contains(s, again.ID) {
		t.Fatalf("another payment after the expiry: %q, approval %s (first %s)", s, again.ID, first.ID)
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false); !strings.Contains(s, again.ID) {
		t.Errorf("the identical call: %q", s)
	}
	// Expired with nothing else calling, the identical call asks again.
	h.clock.add(2 * time.Minute)
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false); strings.Contains(s, again.ID) || len(h.pending()) != 1 {
		t.Errorf("the identical call after a second expiry: %q", s)
	}
	if n := h.payments(); n != 1 {
		t.Fatalf("%d payments", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestUnknown_CutOffBeforeExecutingIsNoAttempt: a mutating call cut off
// after the policy allowed it and before its tool started ran nothing:
// abandoned there, or its lease lost there. Its run is ended, the tool is
// not blocked, and the identical call runs under the ordinary rules,
// without asking an operator.
func TestUnknown_CutOffBeforeExecutingIsNoAttempt(t *testing.T) {
	allowPay := func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Outcome = agentrt.Allow
		c.Servers[0].Rules["pay"] = p
		c.LeaseTTL = &Duration{time.Hour}
	}
	ctx := context.Background()
	args := `{"to":"zed","amount":3}`
	allowed := func(t *testing.T, h *harness, g *agentrt.Gate, runID string) (*agentrt.Session, *agentrt.OpenStep) {
		t.Helper()
		if err := h.p.idx.claim(ctx, "test", mustKey(t, "bank_pay", args), "bank_pay", agentrt.RemoteMutation, runID, nil, h.clock.now()); err != nil {
			t.Fatal(err)
		}
		s, err := g.Begin(ctx, runID, "call bank_pay", h.p.limits)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := s.Step(ctx)
		if v, err := st.Propose(ctx, agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "bank_pay", Args: []byte(args), Origin: agentrt.OriginModel}); err != nil || v.Outcome != agentrt.VerdictAllowed {
			t.Fatalf("propose: %+v %v", v, err)
		}
		return s, st
	}
	check := func(t *testing.T, h *harness, runID string) {
		t.Helper()
		if res := h.call("bank_pay", args); res.IsError || resultString(res) != "paid 3.00 to zed" {
			t.Fatalf("the identical call: %q", resultString(res))
		}
		old, err := h.p.store.GetRun(ctx, runID)
		if err != nil || old.Status != agentrt.StatusFailed || !strings.Contains(old.ReasonDetail, "taken over after an interruption") {
			t.Fatalf("the run cut off: %+v %v", old, err)
		}
		if n := h.approvals(); n != 0 {
			t.Fatalf("%d approvals, want none", n)
		}
		settledOrWaiting(t, h.p.store)
	}
	t.Run("abandoned after it was allowed", func(t *testing.T) {
		h := newHarness(t, allowPay)
		s, _ := allowed(t, h, h.p.gate, "test.abandonedallowed")
		s.Close()
		if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
			t.Fatalf("another payment: %q", resultString(res))
		}
		check(t, h, "test.abandonedallowed")
		if n := h.payments(); n != 2 {
			t.Fatalf("%d payments, want lou's and zed's", n)
		}
	})
	t.Run("lease lost before Execute", func(t *testing.T) {
		h := newHarness(t, allowPay)
		g, err := agentrt.NewGate(agentrt.GateConfig{Store: h.p.store, Policy: gatePolicy{h.p}, Tools: []agentrt.Tool{gateTool{h.p.tools["bank_pay"]}},
			Reconcile: h.p.reconcile, Now: h.clock.now, LeaseOwner: "another-proxy", LeaseTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		runID := "test.leaselost"
		s, st := allowed(t, h, g, runID)
		defer s.Close()
		// The other process's lease runs out unrenewed.
		if _, err := h.p.store.DB().ExecContext(ctx, `UPDATE runs SET lease_expires_at = ? WHERE id = ?`,
			time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), runID); err != nil {
			t.Fatal(err)
		}
		if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
			t.Fatalf("another payment: %q", resultString(res))
		}
		if _, err := st.Execute(ctx); err == nil {
			t.Fatal("the step whose lease was lost executed")
		}
		check(t, h, runID)
		if n := h.payments(); n != 2 {
			t.Fatalf("%d payments, want lou's and zed's", n)
		}
	})
}

// TestUnknown_UnrecordedResultCountsAsExecuted: a payment whose server
// answered with content the runtime refuses executed, and its result could
// not be recorded. The model is told exactly that, not that it failed; no
// operator is asked; the identical call within the window is a duplicate,
// and other payments run.
func TestUnknown_UnrecordedResultCountsAsExecuted(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"ria","amount":4,"mode":"deep"}`
	res := h.call("bank_pay", args)
	s := wantPrefix(t, res, PrefixUnrecorded, false)
	if !strings.Contains(s, "so it executed") || !strings.Contains(s, "could not be recorded") || strings.Contains(s, "failed") || h.payments() != 1 {
		t.Fatalf("first call: %q, %d payments", s, h.payments())
	}
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true); !strings.Contains(s, "could not be recorded") || strings.Contains(s, "it failed") {
		t.Errorf("the identical call: %q", s)
	}
	if res := h.call("bank_pay", `{"to":"lou","amount":1}`); res.IsError {
		t.Fatalf("another payment: %q", resultString(res))
	}
	if n, a := h.payments(), h.approvals(); n != 2 || a != 0 {
		t.Fatalf("%d payments, %d approvals", n, a)
	}
	runs := h.runs()
	steps, _ := h.p.store.ListSteps(context.Background(), runs[0].ID)
	if len(steps) == 0 || steps[0].Observation == nil || failureOf(*steps[0].Observation) != "invalid_content" {
		t.Fatalf("the first payment's step: %+v", steps)
	}
	settledOrWaiting(t, h.p.store)
}
