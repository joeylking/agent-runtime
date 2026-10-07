package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

// rewrite replaces the configuration with change applied, as an operator
// editing the file would.
func rewrite(t *testing.T, config string, change func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	change(cfg)
	raw, _ = json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recheckRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), append([]string{"recheck"}, args...), nil, &out, &errb)
	return code, out.String(), errb.String()
}

// TestCLI_Recheck: the session's calls re-check the same under the
// configuration they ran with and exit 0; once the policy denies payments
// the payment's decisions are named and it exits 1; with -current-tools
// after the operator reworded the payment's description its approvals
// are other approvals; and a usage error exits 2 before anything is read.
func TestCLI_Recheck(t *testing.T) {
	dir, config := setup(t, nil)
	p := start(t, config)
	if res := p.call("bank_balance", `{"account":"main"}`); res.IsError {
		t.Fatalf("balance: %s", text(res))
	}
	wantPrefix(t, p.call("bank_pay", `{"to":"kim","amount":5}`), "PENDING_APPROVAL")
	decide(t, operator(t, dir), pending(t, operator(t, dir))[0], true, "fine")
	if res := p.call("bank_pay", `{"to":"kim","amount":5}`); res.IsError {
		t.Fatalf("collected payment: %s", text(res))
	}
	wantPrefix(t, p.call("bank_refund", `{"payment":"p1"}`), "DENIED")
	p.stop()

	code, out, errb := recheckRun(t, "-config", config)
	if code != 0 || !strings.Contains(out, "3 run(s) re-checked under agentrt-proxy/sha256:") || !strings.Contains(out, ": 4 decision(s) re-checked, 0 not re-checkable and skipped; 0 run(s) differ\n"+recheckScope+"\n") || strings.Contains(out, "DIFFERENT") {
		t.Fatalf("same configuration: exit %d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "policy unchanged") || !strings.Contains(out, "same      step 0 seq ") {
		t.Fatalf("same configuration:\n%s", out)
	}

	rewrite(t, config, func(cfg map[string]any) {
		cfg["policy"] = map[string]any{"remote_mutation": "deny"}
	})
	code, out, errb = recheckRun(t, "-config", config)
	if code != 1 || strings.Count(out, "DIFFERENT") != 2 || !strings.Contains(out, "bank_pay: require_approval -> deny: side effect remote_mutation is deny by policy") ||
		!strings.Contains(out, "bank_pay (on resume): require_approval -> deny") || !strings.Contains(out, "policy changed") || !strings.Contains(out, "; 1 run(s) differ\n") {
		t.Fatalf("payments denied: exit %d\n%s%s", code, out, errb)
	}

	rewrite(t, config, func(cfg map[string]any) {
		delete(cfg, "policy")
		rules := cfg["servers"].([]any)[0].(map[string]any)["rules"].(map[string]any)
		rules["pay"].(map[string]any)["description"] = "Send money; reworded by the operator."
	})
	if code, out, errb = recheckRun(t, "-config", config); code != 0 {
		t.Fatalf("a reworded description against the recorded specs: exit %d\n%s%s", code, out, errb)
	}
	code, out, errb = recheckRun(t, "-config", config, "-current-tools", "-json")
	var reps []agentrt.RecheckReport
	if err := json.Unmarshal([]byte(out), &reps); err != nil {
		t.Fatalf("-json: %v\n%s", err, out)
	}
	if code != 1 || len(reps) != 3 || reps[1].SpecSource != agentrt.SpecCurrent || reps[1].Identity != agentrt.IdentityUnchanged || len(reps[1].Evaluations) != 2 ||
		reps[1].Evaluations[0].Result != agentrt.RecheckDifferent || !strings.Contains(reps[1].Evaluations[0].Detail, "tool spec") || reps[0].Evaluations[0].Result != agentrt.RecheckSame {
		t.Fatalf("-current-tools: exit %d\n%s%s", code, out, errb)
	}
	if !strings.Contains(errb, "registered pay as bank_pay") {
		t.Fatalf("the load report is not on stderr:\n%s", errb)
	}

	code, out, errb = recheckRun(t, "-config", config, "-run", reps[2].RunID)
	if code != 0 || !strings.Contains(out, "1 run(s) re-checked") || !strings.Contains(out, "bank_balance: allow") {
		t.Fatalf("-run: exit %d\n%s%s", code, out, errb)
	}
	for _, args := range [][]string{
		{"-config", config, "-run", "x", "-session", "y"},
		{"-config", config, "-limit", "0"},
		{"-run", "x"},
		{"-config", config, "extra"},
		{"-config", config, "-nope"},
	} {
		if code, _, errb := recheckRun(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, errb)
		}
	}
	if code, _, errb := recheckRun(t, "-config", config, "-run", "nope"); code != 1 || !strings.Contains(errb, "not found") {
		t.Errorf("a missing run: exit %d\n%s", code, errb)
	}
}

// A re-check that re-checked nothing fails: a -session no run was recorded
// under, a mistyped one, and a run whose only decision met a policy error,
// which left no evaluation to re-check.
func TestCLI_RecheckOfNothingFails(t *testing.T) {
	dir, config := setup(t, nil)
	p := start(t, config)
	if res := p.call("bank_balance", `{"account":"main"}`); res.IsError {
		t.Fatalf("balance: %s", text(res))
	}
	p.stop()
	code, out, errb := recheckRun(t, "-config", config, "-session", "no-such-session")
	if code != 1 || !strings.Contains(errb, `no run of session "no-such-session" was found, so nothing was re-checked`) || !strings.Contains(out, "0 run(s) re-checked") {
		t.Fatalf("a wrong session: exit %d\n%s%s", code, out, errb)
	}
	code, out, errb = recheckRun(t, "-config", config, "-session", "no-such-session", "-json")
	if code != 1 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("a wrong session, -json: exit %d\n%s%s", code, out, errb)
	}

	// The balance's decision, as a policy error leaves it: no step.policy,
	// and the step failed with the policy's error.
	store, err := agentrt.OpenStore(filepath.Join(dir, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DELETE FROM events WHERE type = 'step.policy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO events (run_id, step_id, at, type, payload_json) SELECT run_id, id, started_at, 'step.failed', '{"detail":"policy error: rules unavailable"}' FROM steps`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, out, errb = recheckRun(t, "-config", config)
	if code != 1 || !strings.Contains(out, "0 decision(s) re-checked, 1 not re-checkable and skipped") || !strings.Contains(out, "policy failed; no evaluation recorded: rules unavailable") ||
		!strings.Contains(errb, "no decision of the policy was re-checked (1 not re-checkable)") {
		t.Fatalf("a policy error: exit %d\n%s%s", code, out, errb)
	}
}
