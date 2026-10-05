package proxy

import (
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
