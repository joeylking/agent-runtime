package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/view"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProxy_ListsOnlyClassifiedToolsAndTheStatusTool: the host is offered
// the operator's classified tools under their registered names, with the
// operator's description and the schema without the fixed parameter, and
// the status tool; an unclassified tool is absent and reported, and no
// tool carries an output schema.
func TestProxy_ListsOnlyClassifiedToolsAndTheStatusTool(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		r := c.Servers[0].Rules
		delete(r, "refund")
		b := r["balance"]
		b.Description = "The operator's own words for balance."
		r["balance"] = b
		p := r["pay"]
		p.Fixed = map[string]json.RawMessage{"currency": json.RawMessage(`"EUR"`)}
		r["pay"] = p
	})
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*sdk.Tool{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		byName[tl.Name] = tl
		if tl.OutputSchema != nil {
			t.Errorf("%s has an output schema", tl.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"bank_balance", "bank_pay", "gate_status"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if d := byName["bank_balance"].Description; d != "The operator's own words for balance." {
		t.Errorf("balance description = %q", d)
	}
	schema, _ := json.Marshal(byName["bank_pay"].InputSchema)
	if strings.Contains(string(schema), "currency") || !strings.Contains(string(schema), `"amount"`) {
		t.Errorf("pay schema = %s, want amount and no currency", schema)
	}
	reports := h.p.Reports()
	if len(reports) != 1 || !slices.Equal(reports[0].Unclassified, []string{"refund"}) {
		t.Fatalf("reports = %+v, want refund unclassified", reports)
	}
}

// TestProxy_AllowedReadReturnsContent: a read the policy allows runs once
// and answers with what the server returned.
func TestProxy_AllowedReadReturnsContent(t *testing.T) {
	h := newHarness(t, nil)
	res := h.call("bank_balance", `{"account":"A-1"}`)
	if res.IsError || resultString(res) != "account A-1 holds 120.00 EUR" {
		t.Fatalf("balance = %+v %q", res.IsError, resultString(res))
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_DeniedAndInvalidRunNothing: a denied class answers DENIED and
// the server is never called; arguments the schema refuses answer
// INVALID with the runtime's reason.
func TestProxy_DeniedAndInvalidRunNothing(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Outcome = agentrt.Deny
		c.Servers[0].Rules["pay"] = p
	})
	wantPrefix(t, h.call("bank_refund", `{"payment":"p-1"}`), PrefixDenied, true)
	s := wantPrefix(t, h.call("bank_pay", `{"to":"bob","amount":5}`), PrefixDenied, true)
	if !strings.Contains(s, "operator's rule") {
		t.Errorf("denial = %q, want the rule's reason", s)
	}
	s = wantPrefix(t, h.call("bank_balance", `{"acount":"A-1"}`), PrefixInvalid, true)
	if !strings.Contains(s, "account") {
		t.Errorf("invalid = %q, want the schema's complaint", s)
	}
	if n := h.payments(); n != 0 {
		t.Fatalf("%d payments after a denial", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_ApprovalRoundTrip: a call that needs approval answers pending
// and runs nothing; once an operator approves it the identical call runs
// it exactly once and answers its result; a third identical call within
// the window is refused as a duplicate and runs nothing.
func TestProxy_ApprovalRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	args := `{"to":"bob","amount":25}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	if n := h.payments(); n != 0 {
		t.Fatalf("%d payments while pending", n)
	}
	a := h.onlyPending()
	if !strings.Contains(h.logs.String(), "approve "+a.RunID+" -approval "+a.ID) {
		t.Errorf("stderr does not give the operator the command:\n%s", h.logs.String())
	}
	// Called again before a decision, it is the same request, still
	// waiting.
	if s := wantPrefix(t, h.call("bank_pay", args), PrefixPending, false); !strings.Contains(s, a.ID) {
		t.Errorf("second pending = %q, want approval %s", s, a.ID)
	}
	decide(t, h.p.store, a, true, "ok")
	res := h.call("bank_pay", args)
	if res.IsError || resultString(res) != "paid 25.00 to bob" || h.payments() != 1 {
		t.Fatalf("collect = %v %q, %d payments", res.IsError, resultString(res), h.payments())
	}
	s := wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true)
	if !strings.Contains(s, "paid 25.00 to bob") || h.payments() != 1 {
		t.Fatalf("duplicate = %q, %d payments", s, h.payments())
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_RepeatWindowZeroAsksAgain: with the window off, the identical
// call after an executed one is a new request with its own approval.
func TestProxy_RepeatWindowZeroAsksAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RepeatWindow = &Duration{0} })
	args := `{"to":"bob","amount":25}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	first := h.onlyPending()
	decide(t, h.p.store, first, true, "")
	h.call("bank_pay", args)
	s := wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	second := h.onlyPending()
	if second.ID == first.ID || !strings.Contains(s, second.ID) || h.payments() != 1 {
		t.Fatalf("second request: %q, approval %s after %s, %d payments", s, second.ID, first.ID, h.payments())
	}
}

// TestProxy_ApprovedWithinTheHoldCompletesInOneCall: a call held for its
// approval runs as soon as it is granted and answers the result in the
// same call, sending progress while it waits.
func TestProxy_ApprovedWithinTheHoldCompletesInOneCall(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Hold = &Duration{20 * time.Second} })
	go func() {
		var a []agentrt.Approval
		for len(a) == 0 {
			time.Sleep(20 * time.Millisecond)
			a = pendingIn(t, h.p.store)
		}
		// Long enough for a progress notification.
		time.Sleep(1500 * time.Millisecond)
		decide(t, h.p.store, a[0], true, "")
	}()
	start := time.Now()
	res := h.callCtx(context.Background(), "bank_pay", `{"to":"carol","amount":3}`, "tok-1")
	if res.IsError || resultString(res) != "paid 3.00 to carol" || h.payments() != 1 {
		t.Fatalf("held call = %v %q, %d payments", res.IsError, resultString(res), h.payments())
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the held call took %s", time.Since(start))
	}
	eventually(t, 5*time.Second, "a progress notification", func() bool { return h.progress.Load() > 0 })
}

// TestProxy_CanonicalMatching: key order and whitespace do not make a new
// request; a changed string, a changed number, or a number written another
// way does, as it is another approval.
func TestProxy_CanonicalMatching(t *testing.T) {
	h := newHarness(t, nil)
	wantPrefix(t, h.call("bank_pay", `{"to":"dan","amount":10}`), PrefixPending, false)
	a := h.onlyPending()
	for _, same := range []string{`{ "amount": 10, "to": "dan" }`, `{"amount":10,"to":"\u0064an"}`} {
		if s := wantPrefix(t, h.call("bank_pay", same), PrefixPending, false); !strings.Contains(s, a.ID) {
			t.Errorf("%s opened a request of its own: %q", same, s)
		}
	}
	if n := len(h.pending()); n != 1 {
		t.Fatalf("%d pending after equivalent calls", n)
	}
	for _, other := range []string{`{"to":"Dan","amount":10}`, `{"to":"dan","amount":10.5}`, `{"amount":10.0,"to":"dan"}`, `{"to":"dan","amount":1e1}`} {
		if s := wantPrefix(t, h.call("bank_pay", other), PrefixPending, false); strings.Contains(s, a.ID) {
			t.Errorf("%s collected %s: %q", other, a.ID, s)
		}
	}
	if n := len(h.pending()); n != 5 {
		t.Fatalf("%d pending, want 5", n)
	}
}

// TestProxy_SeveralApprovalsAtOnceAndTheCap: requests wait side by side,
// each collected on its own; past max_pending a new one is blocked
// without an approval.
func TestProxy_SeveralApprovalsAtOnceAndTheCap(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxPending = 2 })
	wantPrefix(t, h.call("bank_pay", `{"to":"a","amount":1}`), PrefixPending, false)
	wantPrefix(t, h.call("bank_pay", `{"to":"b","amount":2}`), PrefixPending, false)
	wantPrefix(t, h.call("bank_pay", `{"to":"c","amount":3}`), PrefixBlocked, true)
	if n := h.approvals(); n != 2 {
		t.Fatalf("%d approvals, want 2", n)
	}
	// A read needs no approval and is not blocked.
	if res := h.call("bank_balance", `{"account":"x"}`); res.IsError {
		t.Fatalf("read while the queue is full: %q", resultString(res))
	}
	p := h.pending()
	decide(t, h.p.store, p[1], true, "")
	if res := h.call("bank_pay", `{"to":"b","amount":2}`); resultString(res) != "paid 2.00 to b" {
		t.Fatalf("b = %q", resultString(res))
	}
	wantPrefix(t, h.call("bank_pay", `{"to":"a","amount":1}`), PrefixPending, false)
	decide(t, h.p.store, p[0], true, "")
	if res := h.call("bank_pay", `{"to":"a","amount":1}`); resultString(res) != "paid 1.00 to a" {
		t.Fatalf("a = %q", resultString(res))
	}
	if n := h.payments(); n != 2 {
		t.Fatalf("%d payments, want 2", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_RejectedIsNotAskedAgain: a rejected request answers REJECTED
// with the operator's note, and no new approval is asked for inside the
// window.
func TestProxy_RejectedIsNotAskedAgain(t *testing.T) {
	h := newHarness(t, nil)
	args := `{"to":"eve","amount":900}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	decide(t, h.p.store, h.onlyPending(), false, "not to eve")
	s := wantPrefix(t, h.call("bank_pay", args), PrefixRejected, true)
	if !strings.Contains(s, "not to eve") {
		t.Errorf("rejected = %q, want the note", s)
	}
	wantPrefix(t, h.call("bank_pay", args), PrefixRejected, true)
	if n, p := h.approvals(), h.payments(); n != 1 || p != 0 {
		t.Fatalf("%d approvals, %d payments after a rejection", n, p)
	}
	// Past the window it may be asked for again.
	h.clock.add(11 * time.Minute)
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
}

// TestProxy_ExpiredIsNotAskedAgain: an approval past its ApprovalTTL is
// expired when the request is called again, which answers EXPIRED and
// asks nothing new inside the window.
func TestProxy_ExpiredIsNotAskedAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ApprovalTTL = &Duration{time.Hour} })
	args := `{"to":"fay","amount":4}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	h.clock.add(2 * time.Hour)
	wantPrefix(t, h.call("bank_pay", args), PrefixExpired, true)
	wantPrefix(t, h.call("bank_pay", args), PrefixExpired, true)
	if n, p := h.approvals(), h.payments(); n != 1 || p != 0 {
		t.Fatalf("%d approvals, %d payments after an expiry", n, p)
	}
}

// TestProxy_StaleGrantExecutesNothing: a grant older than GrantTTL by the
// proxy's clock is expired at collection and nothing runs.
func TestProxy_StaleGrantExecutesNothing(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GrantTTL = &Duration{time.Hour} })
	args := `{"to":"gil","amount":4}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	decide(t, h.p.store, h.onlyPending(), true, "")
	h.clock.add(2 * time.Hour)
	wantPrefix(t, h.call("bank_pay", args), PrefixExpired, true)
	if n := h.payments(); n != 0 {
		t.Fatalf("%d payments under a stale grant", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_PolicyChangedToDenyBeforeCollection: policy is evaluated again
// when an approved request is collected, so a class denied since is not
// executed.
func TestProxy_PolicyChangedToDenyBeforeCollection(t *testing.T) {
	h := newHarness(t, nil)
	args := `{"to":"hal","amount":4}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	decide(t, h.p.store, h.onlyPending(), true, "")
	h.p.mu.Lock()
	h.p.decider = classPolicy{agentrt.ReadOnly: agentrt.Allow, agentrt.RemoteMutation: agentrt.Deny}
	h.p.mu.Unlock()
	wantPrefix(t, h.call("bank_pay", args), PrefixDenied, true)
	if n := h.payments(); n != 0 {
		t.Fatalf("%d payments after the policy changed to deny", n)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_ChangedFixedValueAsksAgain: the operator's fixed values are in
// the approval's capability, so changing one between approval and
// collection asks for a new approval instead of executing.
func TestProxy_ChangedFixedValueAsksAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Fixed = map[string]json.RawMessage{"currency": json.RawMessage(`"EUR"`)}
		c.Servers[0].Rules["pay"] = p
	})
	args := `{"to":"ida","amount":4}`
	wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	first := h.onlyPending()
	decide(t, h.p.store, first, true, "")
	h.p.mu.Lock()
	h.p.tools["bank_pay"].fixed = map[string]json.RawMessage{"currency": json.RawMessage(`"USD"`)}
	h.p.mu.Unlock()
	s := wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	if strings.Contains(s, first.ID) || h.payments() != 0 {
		t.Fatalf("changed fixed value: %q, %d payments", s, h.payments())
	}
}

// TestProxy_UpstreamErrorAndStructuredResult: an isError result is a tool
// error result carrying the recorded text, and structured content reaches
// the host.
func TestProxy_UpstreamErrorAndStructuredResult(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Outcome = agentrt.Allow
		c.Servers[0].Rules["pay"] = p
	})
	res := h.call("bank_pay", `{"to":"jo","amount":1,"mode":"error"}`)
	if !res.IsError || !strings.Contains(resultString(res), "card declined for jo") {
		t.Fatalf("error result = %v %q", res.IsError, resultString(res))
	}
	res = h.call("bank_pay", `{"to":"jo","amount":2,"mode":"structured"}`)
	raw, _ := json.Marshal(res.StructuredContent)
	if res.IsError || string(raw) != `{"paid":2,"to":"jo"}` {
		t.Fatalf("structured result = %v %s %q", res.IsError, raw, resultString(res))
	}
	// The failed payment executed, so it is a duplicate too, with its error.
	s := wantPrefix(t, h.call("bank_pay", `{"to":"jo","amount":1,"mode":"error"}`), PrefixDuplicate, true)
	if !strings.Contains(s, "card declined") {
		t.Errorf("duplicate of an error = %q", s)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_HostCancelLetsTheCallFinish: a host that gives up on a call
// while its tool runs does not stop the tool; its outcome is recorded and
// the identical call afterwards is a duplicate carrying it.
func TestProxy_HostCancelLetsTheCallFinish(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Outcome = agentrt.Allow
		c.Servers[0].Rules["pay"] = p
	})
	args := `{"to":"kim","amount":7,"mode":"slow"}`
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := h.cs.CallTool(ctx, &sdk.CallToolParams{Name: "bank_pay", Arguments: json.RawMessage(args)}); err == nil {
		t.Fatal("the host's call did not time out")
	}
	eventually(t, 10*time.Second, "the payment's run to complete", func() bool {
		for _, r := range h.runs() {
			if r.Status == agentrt.StatusCompleted {
				return true
			}
		}
		return false
	})
	s := wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true)
	if !strings.Contains(s, "paid 7.00 to kim") || h.payments() != 1 {
		t.Fatalf("after the host cancelled: %q, %d payments", s, h.payments())
	}
}

// TestProxy_RunsRecordWhatHappened: each outcome's run has the events the
// gate writes for it, the tool call proposed by the model and any closing
// decision by the operator's proxy, and ends.
func TestProxy_RunsRecordWhatHappened(t *testing.T) {
	h := newHarness(t, nil)
	h.call("bank_balance", `{"account":"z"}`)
	h.call("bank_refund", `{"payment":"p"}`)
	h.call("bank_pay", `{"to":"lee","amount":1}`)
	decide(t, h.p.store, h.onlyPending(), true, "")
	h.call("bank_pay", `{"to":"lee","amount":1}`)
	h.call("bank_balance", `{}`)
	runs := h.runs()
	if len(runs) != 4 {
		t.Fatalf("%d runs, want one per call that was not a collection: %+v", len(runs), runs)
	}
	want := map[string][]string{
		"allowed": {"run.created", "run.started", "step.started", "step.decided", "step.policy", "step.tool_started", "step.tool_finished", "run.finished"},
		"denied": {"run.created", "run.started", "step.started", "step.decided", "step.policy", "step.failed",
			"step.started", "step.decided", "run.finished"},
		"approved": {"run.created", "run.started", "step.started", "step.decided", "step.policy", "approval.requested",
			"approval.decided", "run.resumed", "step.policy", "step.tool_started", "step.tool_finished", "run.finished"},
		"invalid": {"run.created", "run.started", "step.started", "step.decided", "step.failed",
			"step.started", "step.decided", "run.finished"},
	}
	for i, name := range []string{"allowed", "denied", "approved", "invalid"} {
		if got := eventTypes(t, h.p.store, runs[i].ID); !slices.Equal(got, want[name]) {
			t.Errorf("%s run events:\n got %v\nwant %v", name, got, want[name])
		}
		if !strings.HasPrefix(runs[i].ID, "test.") {
			t.Errorf("run id %q does not start with the session", runs[i].ID)
		}
	}
	if runs[0].Status != agentrt.StatusCompleted || runs[1].Status != agentrt.StatusFailed || runs[2].Status != agentrt.StatusCompleted || runs[3].Status != agentrt.StatusFailed {
		t.Fatalf("statuses %s %s %s %s", runs[0].Status, runs[1].Status, runs[2].Status, runs[3].Status)
	}
	steps, _ := h.p.store.ListSteps(context.Background(), runs[1].ID)
	if len(steps) != 2 || steps[0].Decision.Origin != agentrt.OriginModel || steps[1].Decision.Origin != agentrt.OriginOperator ||
		steps[1].Decision.Kind != agentrt.DecideFail {
		t.Fatalf("denied run's decisions: %+v", steps)
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_StatusToolReportsAndNeverExecutes: the status tool lists what
// waits, says what to do once approved, gives a rejection's note and an
// expiry, runs nothing, and is never recorded as a run.
func TestProxy_StatusToolReportsAndNeverExecutes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ApprovalTTL = &Duration{time.Hour} })
	wantPrefix(t, h.call("gate_status", `{}`), PrefixStatus, false)
	h.call("bank_pay", `{"to":"a","amount":1}`)
	h.call("bank_pay", `{"to":"b","amount":2}`)
	h.call("bank_pay", `{"to":"c","amount":3}`)
	p := h.pending()
	runs := len(h.runs())
	s := wantPrefix(t, h.call("gate_status", `{}`), PrefixStatus, false)
	if !strings.Contains(s, "3 requests") || !strings.Contains(s, p[0].ID) || !strings.Contains(s, "PENDING") {
		t.Fatalf("status = %q", s)
	}
	decide(t, h.p.store, p[0], true, "")
	decide(t, h.p.store, p[1], false, "wrong account")
	s = wantPrefix(t, h.call("gate_status", fmt.Sprintf(`{"approval_id":%q}`, p[0].ID)), PrefixStatus, false)
	if !strings.Contains(s, "APPROVED") || !strings.Contains(s, "Call bank_pay again with exactly the same arguments") {
		t.Fatalf("approved status = %q", s)
	}
	s = wantPrefix(t, h.call("gate_status", fmt.Sprintf(`{"approval_id":%q}`, p[1].ID)), PrefixStatus, false)
	if !strings.Contains(s, "REJECTED") || !strings.Contains(s, "wrong account") {
		t.Fatalf("rejected status = %q", s)
	}
	h.clock.add(2 * time.Hour)
	s = wantPrefix(t, h.call("gate_status", fmt.Sprintf(`{"approval_id":%q}`, p[2].ID)), PrefixStatus, false)
	if !strings.Contains(s, "EXPIRED") {
		t.Fatalf("expired status = %q", s)
	}
	wantPrefix(t, h.call("gate_status", `{"approval_id":"nope"}`), PrefixStatus, false)
	wantPrefix(t, h.call("gate_status", `{"run":"x"}`), PrefixInvalid, true)
	if n, r := h.payments(), len(h.runs()); n != 0 || r != runs {
		t.Fatalf("status tool: %d payments, %d runs after %d", n, r, runs)
	}
}

// TestProxy_StatusSaysWhetherAnApprovedRequestExecuted: the status tool
// reports an approved request executed only when its step executed:
// approved and awaiting collection, approved and executed, and approved
// but not executed, with why, when the policy denied it at collection or
// an operator cancelled it. Before, all three ended runs read "APPROVED
// and executed".
func TestProxy_StatusSaysWhetherAnApprovedRequestExecuted(t *testing.T) {
	h := newHarness(t, nil)
	status := func(id string) string {
		t.Helper()
		return wantPrefix(t, h.call("gate_status", fmt.Sprintf(`{"approval_id":%q}`, id)), PrefixStatus, false)
	}
	for _, args := range []string{`{"to":"pam","amount":1}`, `{"to":"quy","amount":2}`, `{"to":"ray","amount":3}`} {
		wantPrefix(t, h.call("bank_pay", args), PrefixPending, false)
	}
	p := h.pending()
	for _, a := range p {
		decide(t, h.p.store, a, true, "")
	}
	if s := status(p[0].ID); !strings.Contains(s, "APPROVED, awaiting collection") {
		t.Errorf("approved: %q", s)
	}
	h.call("bank_pay", `{"to":"pam","amount":1}`)
	if s := status(p[0].ID); !strings.Contains(s, "APPROVED and executed") {
		t.Errorf("collected: %q", s)
	}
	if err := view.Cancel(context.Background(), h.p.store, nil, p[1].RunID, "operator", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if s := status(p[1].ID); !strings.Contains(s, "APPROVED but not executed: an operator cancelled it") {
		t.Errorf("cancelled: %q", s)
	}
	h.p.mu.Lock()
	h.p.decider = classPolicy{agentrt.ReadOnly: agentrt.Allow, agentrt.RemoteMutation: agentrt.Deny}
	h.p.mu.Unlock()
	wantPrefix(t, h.call("bank_pay", `{"to":"ray","amount":3}`), PrefixDenied, true)
	if s := status(p[2].ID); !strings.Contains(s, "APPROVED but not executed: the policy denied it when it was collected") {
		t.Errorf("denied at collection: %q", s)
	}
	if n := h.payments(); n != 1 {
		t.Fatalf("%d payments", n)
	}
}

// TestProxy_ArgumentsTheRuntimeRefusesNeverCollect: arguments whose JSON
// the runtime refuses, a repeated key or a number past the bound, are an
// invalid request of their own: they never collect another request's
// approval, which the value-based key let them do, and never crash the
// proxy, which such a number did while another call ran.
func TestProxy_ArgumentsTheRuntimeRefusesNeverCollect(t *testing.T) {
	h := newHarness(t, nil)
	wantPrefix(t, h.call("bank_pay", `{"to":"bob","amount":25}`), PrefixPending, false)
	wantPrefix(t, h.call("bank_pay", `{"to":"bob","amount":0.1e-9223372036854775808}`), PrefixInvalid, true)
	p := h.pending()
	if len(p) != 1 {
		t.Fatalf("%d pending", len(p))
	}
	decide(t, h.p.store, p[0], true, "")
	for _, args := range []string{`{"to":"bob","amount":1,"amount":25}`, `{"to":"bob","amount":1e9223372036854775807}`, `{"to":"bob","amount":1e-10000000}`, `{"to":"bob\ud800","amount":25}`} {
		s := wantPrefix(t, h.call("bank_pay", args), PrefixInvalid, true)
		if strings.Contains(s, "paid") {
			t.Errorf("%s: %q", args, s)
		}
	}
	if n := h.payments(); n != 0 {
		t.Fatalf("%d payments", n)
	}
	if res := h.call("bank_pay", `{"to":"bob","amount":25}`); resultString(res) != "paid 25.00 to bob" {
		t.Fatalf("the approved request: %q", resultString(res))
	}
	settledOrWaiting(t, h.p.store)
}

// TestProxy_ServerTextInProseIsEscaped: an error result and a duplicate's
// quoted outcome are prose the proxy writes, so the server's text in them
// is escaped; a result returned as the call's result is the server's data
// and is passed back as recorded.
func TestProxy_ServerTextInProseIsEscaped(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		p := c.Servers[0].Rules["pay"]
		p.Outcome = agentrt.Allow
		c.Servers[0].Rules["pay"] = p
	})
	evil := `x\u001b[2J\u202e`
	res := h.call("bank_pay", `{"to":"`+evil+`","amount":1,"mode":"error"}`)
	if s := resultString(res); !res.IsError || strings.ContainsAny(s, "\x1b\u202e") || !strings.Contains(s, "card declined") {
		t.Errorf("error result: %q", s)
	}
	res = h.call("bank_pay", `{"to":"`+evil+`","amount":2}`)
	if s := resultString(res); res.IsError || !strings.Contains(s, "\x1b") {
		t.Errorf("a result is passed back as recorded: %q", s)
	}
	for _, args := range []string{`{"to":"` + evil + `","amount":1,"mode":"error"}`, `{"to":"` + evil + `","amount":2}`} {
		if s := wantPrefix(t, h.call("bank_pay", args), PrefixDuplicate, true); strings.ContainsAny(s, "\x1b\u202e") {
			t.Errorf("duplicate: %q", s)
		}
	}
}
