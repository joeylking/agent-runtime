package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

// shortRuns makes a read, a payment an operator approves and the
// identical call collects, and a refund the policy denies.
func shortRuns(t *testing.T, h *harness) {
	t.Helper()
	if res := h.call("bank_balance", `{"account":"main"}`); res.IsError {
		t.Fatalf("the balance: %q", resultString(res))
	}
	wantPrefix(t, h.call("bank_pay", `{"to":"kim","amount":5}`), PrefixPending, false)
	decide(t, h.p.store, h.onlyPending(), true, "fine")
	if res := h.call("bank_pay", `{"to":"kim","amount":5}`); res.IsError || resultString(res) != "paid 5.00 to kim" {
		t.Fatalf("the collected payment: %q", resultString(res))
	}
	wantPrefix(t, h.call("bank_refund", `{"payment":"p1"}`), PrefixDenied, true)
}

// summary is a session's reports, newest run first, one line per
// evaluation: the tool, the result, and the recorded and re-checked
// outcomes.
func summary(reps []agentrt.RecheckReport) string {
	var out []string
	for _, r := range reps {
		for _, e := range r.Evaluations {
			now := "-"
			if e.Rechecked != nil {
				now = string(e.Rechecked.Outcome)
			}
			out = append(out, fmt.Sprintf("%s %s %s->%s", e.Tool, e.Result, e.Recorded.Outcome, now))
		}
	}
	return strings.Join(out, "\n")
}

// A session's calls re-check the same under the configuration they ran
// with, every decision identified by it; under a configuration whose
// policy now denies payments, the re-check names the payment's two
// decisions, made when it was asked for and when it was collected, and
// nothing else.
func TestRecheck_ChangedPolicyNamesTheCallsNowDenied(t *testing.T) {
	h := newHarness(t, nil)
	shortRuns(t, h)
	h.close()
	ctx := context.Background()
	reps, err := Recheck(ctx, h.cfg, RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "bank_refund same deny->deny\n" +
		"bank_pay same require_approval->require_approval\n" +
		"bank_pay same require_approval->require_approval\n" +
		"bank_balance same allow->allow"
	if got := summary(reps); got != want {
		t.Fatalf("under the same configuration:\n%s", got)
	}
	id := h.cfg.PolicyID()
	for _, r := range reps {
		if !r.Same() || r.Identity != agentrt.IdentityUnchanged || r.PolicyID != id || !strings.HasPrefix(id, "agentrt-proxy/sha256:") {
			t.Fatalf("report %+v", r)
		}
	}

	h.cfg.Policy = map[agentrt.SideEffect]agentrt.PolicyOutcome{agentrt.RemoteMutation: agentrt.Deny}
	if h.cfg.PolicyID() == id {
		t.Fatal("a changed policy kept its identity")
	}
	reps, err = Recheck(ctx, h.cfg, RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want = "bank_refund same deny->deny\n" +
		"bank_pay different require_approval->deny\n" +
		"bank_pay different require_approval->deny\n" +
		"bank_balance same allow->allow"
	if got := summary(reps); got != want {
		t.Fatalf("under a policy denying payments:\n%s", got)
	}
	if reps[1].Same() || reps[1].Identity != agentrt.IdentityChanged || !reps[0].Same() {
		t.Fatalf("reports %+v", reps)
	}

	one, err := Recheck(ctx, h.cfg, RecheckOptions{RunID: reps[2].RunID})
	if err != nil || len(one) != 1 || one[0].RunID != reps[2].RunID {
		t.Fatalf("one run: %+v %v", one, err)
	}
	if two, err := Recheck(ctx, h.cfg, RecheckOptions{Limit: 2}); err != nil || len(two) != 2 || two[0].RunID != reps[0].RunID {
		t.Fatalf("limit 2: %+v %v", two, err)
	}
	if none, err := Recheck(ctx, h.cfg, RecheckOptions{Session: "other"}); err != nil || len(none) != 0 {
		t.Fatalf("another session: %+v %v", none, err)
	}
}

// What a configuration's identity covers: the rules the policy decides
// from, and nothing it does not.
func TestConfig_PolicyIDCoversWhatThePolicyDecidesFrom(t *testing.T) {
	base := testConfig(t, t.TempDir())
	id := base.PolicyID()
	for name, change := range map[string]func(*Config){
		"a description": func(c *Config) {
			r := c.Servers[0].Rules["pay"]
			r.Description = "reworded"
			c.Servers[0].Rules["pay"] = r
		},
		"a timeout":   func(c *Config) { r := c.Servers[0].Rules["pay"]; r.Timeout = "1m"; c.Servers[0].Rules["pay"] = r },
		"the hold":    func(c *Config) { c.Hold = &Duration{5e9} },
		"the session": func(c *Config) { c.Session = "other" },
		"a default made explicit": func(c *Config) {
			c.Policy = map[agentrt.SideEffect]agentrt.PolicyOutcome{agentrt.ReadOnly: agentrt.Allow}
		},
	} {
		c := testConfig(t, t.TempDir())
		change(c)
		if c.PolicyID() != id {
			t.Errorf("%s changed the identity", name)
		}
	}
	for name, change := range map[string]func(*Config){
		"the policy": func(c *Config) {
			c.Policy = map[agentrt.SideEffect]agentrt.PolicyOutcome{agentrt.ReadOnly: agentrt.Deny}
		},
		"a class": func(c *Config) {
			r := c.Servers[0].Rules["pay"]
			r.SideEffect = agentrt.LocalMutation
			c.Servers[0].Rules["pay"] = r
		},
		"an outcome": func(c *Config) {
			r := c.Servers[0].Rules["pay"]
			r.Outcome = agentrt.Allow
			c.Servers[0].Rules["pay"] = r
		},
		"a rename": func(c *Config) { r := c.Servers[0].Rules["pay"]; r.Rename = "pay"; c.Servers[0].Rules["pay"] = r },
		"a fixed one": func(c *Config) {
			r := c.Servers[0].Rules["pay"]
			r.Fixed = map[string]json.RawMessage{"memo": json.RawMessage(`"x"`)}
			c.Servers[0].Rules["pay"] = r
		},
		"a server":    func(c *Config) { c.Servers[0].Name = "bank2" },
		"a rule less": func(c *Config) { delete(c.Servers[0].Rules, "refund") },
	} {
		c := testConfig(t, t.TempDir())
		change(c)
		if c.PolicyID() == id {
			t.Errorf("%s kept the identity", name)
		}
	}
}

// With the tools loaded now, a payment whose description the operator
// reworded asks for another approval than the one recorded, while the
// recorded specs still re-check the same: the identity does not change,
// because the policy does not decide from a description.
func TestRecheck_CurrentToolsAfterADescriptionChange(t *testing.T) {
	h := newHarness(t, nil)
	shortRuns(t, h)
	h.close()
	ctx := context.Background()
	id := h.cfg.PolicyID()
	r := h.cfg.Servers[0].Rules["pay"]
	r.Description = "Send money; reworded by the operator."
	h.cfg.Servers[0].Rules["pay"] = r
	if h.cfg.PolicyID() != id {
		t.Fatal("a description changed the identity")
	}
	if reps, err := Recheck(ctx, h.cfg, RecheckOptions{}); err != nil || summary(reps) != strings.Join([]string{
		"bank_refund same deny->deny", "bank_pay same require_approval->require_approval", "bank_pay same require_approval->require_approval", "bank_balance same allow->allow"}, "\n") {
		t.Fatalf("recorded specs: %v\n%s", err, summary(reps))
	}
	var log strings.Builder
	reps, err := Recheck(ctx, h.cfg, RecheckOptions{CurrentTools: true, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	want := "bank_refund same deny->deny\n" +
		"bank_pay different require_approval->require_approval\n" +
		"bank_pay different require_approval->require_approval\n" +
		"bank_balance same allow->allow"
	if got := summary(reps); got != want {
		t.Fatalf("current tools:\n%s", got)
	}
	if e := reps[1].Evaluations[0]; e.SpecSource != agentrt.SpecCurrent || !strings.Contains(e.Detail, "tool spec") || reps[1].Identity != agentrt.IdentityUnchanged {
		t.Fatalf("evaluation %+v", e)
	}
	if !strings.Contains(log.String(), "registered pay as bank_pay") {
		t.Fatalf("load report:\n%s", log.String())
	}
}

// A payment that timed out after it took effect is asked about as an
// interruption, re-run once approved: a re-check under the same
// configuration finds the same interruptions, although the proxy named
// the attempt they re-run only in memory, as it proposed them.
func TestRecheck_ReRunOfAnUnknownOutcomeIsTheSame(t *testing.T) {
	h := newHarness(t, timeoutPay)
	args := `{"to":"kim","amount":7,"mode":"block"}`
	wantPrefix(t, h.call("bank_pay", args), PrefixUnknown, false)
	a := unknownApproval(t, h)
	os.WriteFile(h.ledger+".release", nil, 0o600)
	decide(t, h.p.store, a, true, "it did not go through")
	if res := h.call("bank_pay", args); res.IsError || resultString(res) != "paid 7.00 to kim" {
		t.Fatalf("the approved re-run: %q", resultString(res))
	}
	h.close()
	reps, err := Recheck(context.Background(), h.cfg, RecheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "bank_pay same allow->allow\n" +
		"bank_pay same require_approval->require_approval\n" +
		"bank_pay same require_approval->require_approval"
	if got := summary(reps); got != want || len(reps) != 1 || !reps[0].Same() {
		t.Fatalf("re-check:\n%s", got)
	}
	if e := reps[0].Evaluations[1]; e.Recorded.Kind != agentrt.InterruptedSideEffect || e.Rechecked.Kind != agentrt.InterruptedSideEffect || e.RecordedHash != e.RecheckedHash {
		t.Fatalf("the re-run's evaluation %+v", e)
	}
}
