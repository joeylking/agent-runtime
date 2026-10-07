package agentrt_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"
	"github.com/joeylking/agent-runtime/scripted"
)

// specHash is the documented hash of a tool spec, computed here from the
// public pieces: the SHA-256 of the canonical JSON of the whole spec.
func specHash(t *testing.T, spec agentrt.ToolSpec) string {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agentrt.CanonicalJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:])
}

// policyPayload is a step.policy payload as this release writes it.
type policyPayload struct {
	Outcome  agentrt.PolicyOutcome `json:"outcome"`
	PolicyID *string               `json:"policy_id"`
	SpecHash string                `json:"spec_hash"`
	View     *struct {
		Status            agentrt.RunStatus `json:"status"`
		StepCount         int               `json:"step_count"`
		ModelCalls        int               `json:"model_calls"`
		InputTokens       int               `json:"input_tokens"`
		OutputTokens      int               `json:"output_tokens"`
		CachedInputTokens int               `json:"cached_input_tokens"`
		EstimatedCost     agentrt.Micros    `json:"estimated_cost_micros"`
		ActiveMS          int64             `json:"active_ms"`
		Steps             int               `json:"steps"`
		Approvals         int               `json:"approvals"`
	} `json:"view"`
}

func policyEvents(t *testing.T, events []agentrt.Event) ([]agentrt.Event, []policyPayload) {
	t.Helper()
	var evs []agentrt.Event
	var out []policyPayload
	for _, e := range events {
		if e.Type != agentrt.EventStepPolicy {
			continue
		}
		var p policyPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("step.policy payload %s: %v", e.Payload, err)
		}
		evs, out = append(evs, e), append(out, p)
	}
	return evs, out
}

type createdPayload struct {
	Goal   string         `json:"goal"`
	Limits agentrt.Limits `json:"limits"`
	Tools  []string       `json:"tools"`
}

func created(t *testing.T, events []agentrt.Event) (agentrt.Event, createdPayload) {
	t.Helper()
	for _, e := range events {
		if e.Type == agentrt.EventRunCreated {
			var c createdPayload
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				t.Fatal(err)
			}
			return e, c
		}
	}
	t.Fatal("no run.created")
	return agentrt.Event{}, createdPayload{}
}

// seeingPolicy keeps every request and view it is handed.
type seeingPolicy struct {
	agentrt.Policy
	reqs  []agentrt.ToolRequest
	views []agentrt.RunView
}

func (p *seeingPolicy) Evaluate(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	p.reqs, p.views = append(p.reqs, req), append(p.views, view)
	return p.Policy.Evaluate(ctx, req, view)
}

// namedPolicy is a policy with an identity.
type namedPolicy struct {
	agentrt.Policy
	id string
}

func (p namedPolicy) PolicyID() string { return p.id }

// requireSeen checks each step.policy event against the view the policy
// was handed for it: the run rebuilt from run.created and the recorded
// view is exactly the run the policy saw, and the recorded counts are the
// steps and approvals it saw, which are the run's first ones.
func requireSeen(t *testing.T, store *agentrt.Store, events []agentrt.Event, p *seeingPolicy) {
	t.Helper()
	ctx := context.Background()
	ce, c := created(t, events)
	evs, payloads := policyEvents(t, events)
	if len(payloads) != len(p.views) || len(payloads) == 0 {
		t.Fatalf("%d step.policy events for %d evaluations", len(payloads), len(p.views))
	}
	steps, err := store.ListSteps(ctx, ce.RunID)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := store.ListApprovals(ctx, ce.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i, pl := range payloads {
		saw, v := p.views[i], pl.View
		if v == nil {
			t.Fatalf("step.policy %d has no view: %s", i, evs[i].Payload)
		}
		rebuilt := agentrt.Run{ID: ce.RunID, Goal: c.Goal, Status: v.Status, Limits: c.Limits, StepCount: v.StepCount, CreatedAt: ce.At, StartedAt: ce.At,
			ModelCalls: v.ModelCalls, Usage: agentrt.Usage{InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, CachedInputTokens: v.CachedInputTokens},
			EstimatedCost: v.EstimatedCost, ActiveTime: time.Duration(v.ActiveMS) * time.Millisecond}
		got := saw.Run
		if !got.CreatedAt.Equal(rebuilt.CreatedAt) || !got.StartedAt.Equal(rebuilt.StartedAt) {
			t.Fatalf("evaluation %d: created %v started %v, run.created at %v", i, got.CreatedAt, got.StartedAt, ce.At)
		}
		got.CreatedAt, got.StartedAt = rebuilt.CreatedAt, rebuilt.StartedAt
		if !reflect.DeepEqual(got, rebuilt) {
			t.Fatalf("evaluation %d: the policy saw\n%+v\nthe record rebuilds\n%+v", i, got, rebuilt)
		}
		if v.Steps != len(saw.Steps) || v.Approvals != len(saw.Approvals) {
			t.Fatalf("evaluation %d: recorded %d steps and %d approvals, the policy saw %d and %d", i, v.Steps, v.Approvals, len(saw.Steps), len(saw.Approvals))
		}
		for j, st := range saw.Steps {
			if st.ID != steps[j].ID {
				t.Fatalf("evaluation %d: step %d is %s, the run's is %s", i, j, st.ID, steps[j].ID)
			}
		}
		for j, a := range saw.Approvals {
			if a.ID != approvals[j].ID {
				t.Fatalf("evaluation %d: approval %d is %s, the run's is %s", i, j, a.ID, approvals[j].ID)
			}
		}
		if pl.SpecHash != specHash(t, p.reqs[i].Spec) {
			t.Fatalf("evaluation %d: spec_hash %s is not the spec the policy was handed", i, pl.SpecHash)
		}
	}
}

// tick is a clock that moves on at every reading, so steps take time.
func tick() func() time.Time {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return func() time.Time { at = at.Add(1500 * time.Millisecond); return at }
}

// Each distinct tool spec is stored once, under the hash of its canonical
// JSON, and every tool_call step naming a registered tool records it, valid
// or not; the step.policy event names the same spec, and run.created lists
// every registered tool's spec in name order. Store.ToolSpec reads each
// back as registered.
func TestToolSpec_StoredOnceAndReferencedByEachStep(t *testing.T) {
	h := newHarness(t, ":memory:")
	read, write := echoTool("read", agentrt.ReadOnly), echoTool("write", agentrt.LocalMutation)
	write.spec.Terminal, write.spec.Timeout = true, 3*time.Second
	d := h.driver(&scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("read", `{"n":"x"}`, "invalid arguments"),
		scripted.ToolCall("missing", `{}`, "unknown tool"),
		scripted.ToolCall("read", `{"n":2}`, ""),
		scripted.ToolCall("write", `{"n":3}`, ""),
	}}, agentrt.DefaultPolicy(), write, read)
	run, err := d.Start(context.Background(), "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	ctx := context.Background()
	readHash, writeHash := specHash(t, read.spec), specHash(t, write.spec)
	if readHash == writeHash {
		t.Fatal("two specs share a hash")
	}
	var n int
	h.store.DB().QueryRow(`SELECT COUNT(*) FROM tool_specs`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d spec rows, want 2", n)
	}
	steps, err := h.store.ListSteps(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{readHash, readHash, "", readHash, writeHash}
	for i, st := range steps {
		if st.SpecHash != want[i] {
			t.Fatalf("step %d spec hash %q, want %q", i, st.SpecHash, want[i])
		}
	}
	events, _ := h.store.ListEvents(ctx, run.ID)
	_, c := created(t, events)
	if strings.Join(c.Tools, " ") != readHash+" "+writeHash {
		t.Fatalf("run.created tools %v, want read then write", c.Tools)
	}
	evs, payloads := policyEvents(t, events)
	for i, p := range payloads {
		var stepHash string
		for _, st := range steps {
			if st.ID == evs[i].StepID {
				stepHash = st.SpecHash
			}
		}
		if p.SpecHash == "" || p.SpecHash != stepHash {
			t.Fatalf("step.policy %d spec_hash %q, its step's %q", i, p.SpecHash, stepHash)
		}
	}
	for _, tool := range []*fakeTool{read, write} {
		got, err := h.store.ToolSpec(ctx, specHash(t, tool.spec))
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != tool.spec.Name || got.Description != tool.spec.Description || got.SideEffect != tool.spec.SideEffect || got.Timeout != tool.spec.Timeout ||
			got.Terminal != tool.spec.Terminal || specHash(t, got) != specHash(t, tool.spec) {
			t.Fatalf("ToolSpec read back %+v, registered %+v", got, tool.spec)
		}
		var a, b any
		json.Unmarshal(got.InputSchema, &a)
		json.Unmarshal(tool.spec.InputSchema, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("schema read back %s, registered %s", got.InputSchema, tool.spec.InputSchema)
		}
	}
	if _, err := h.store.ToolSpec(ctx, strings.Repeat("0", 64)); err != agentrt.ErrNotFound {
		t.Fatalf("an unknown hash: %v", err)
	}
	h.store.DB().Exec(`UPDATE tool_specs SET spec_json = replace(spec_json, '"read"', '"reed"') WHERE hash = ?`, readHash)
	if _, err := h.store.ToolSpec(ctx, readHash); err == nil {
		t.Fatal("a spec altered in the database was read back")
	}
}

// A tool registered again with another description, timeout, or terminal
// flag is another spec: each run records the one it ran with, and both
// stay readable.
func TestToolSpec_ChangedSpecIsAnotherHash(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx := context.Background()
	runWith := func(spec func(*agentrt.ToolSpec)) (agentrt.Run, string) {
		read := echoTool("read", agentrt.ReadOnly)
		spec(&read.spec)
		d := h.driver(&scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.Complete(`{}`)}}, agentrt.DefaultPolicy(), read)
		run, err := d.Start(ctx, "g", limits(10, 3))
		if err != nil {
			t.Fatal(err)
		}
		return run, specHash(t, read.spec)
	}
	seen := map[string]agentrt.Run{}
	for _, change := range []func(*agentrt.ToolSpec){
		func(*agentrt.ToolSpec) {},
		func(s *agentrt.ToolSpec) { s.Description = "read, reworded" },
		func(s *agentrt.ToolSpec) { s.Timeout = time.Minute },
		func(s *agentrt.ToolSpec) { s.Terminal = true },
	} {
		run, hash := runWith(change)
		if _, dup := seen[hash]; dup {
			t.Fatalf("a changed spec kept the hash %s", hash)
		}
		seen[hash] = run
	}
	for hash, run := range seen {
		steps, _ := h.store.ListSteps(ctx, run.ID)
		if steps[0].SpecHash != hash {
			t.Fatalf("run %s step 0 spec hash %s, want %s", run.ID, steps[0].SpecHash, hash)
		}
		if _, err := h.store.ToolSpec(ctx, hash); err != nil {
			t.Fatalf("spec %s: %v", hash, err)
		}
	}
}

// A policy's identity is recorded on each step it evaluated and in that
// step's step.policy event; a step the policy never saw has none, and a
// plain policy records none at all.
func TestPolicyID_RecordedOnStepAndEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy agentrt.Policy
		id     string
	}{
		{"identified", namedPolicy{agentrt.DefaultPolicy(), "side-effects/v2 ✓"}, "side-effects/v2 ✓"},
		{"plain", agentrt.DefaultPolicy(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, ":memory:")
			d := h.driver(&scripted.Agent{Decisions: []agentrt.Decision{
				scripted.ToolCall("read", `{"n":1}`, ""),
				scripted.ToolCall("read", `{"n":"x"}`, "never reaches the policy"),
				scripted.ToolCall("wipe", `{"n":1}`, "denied"),
				scripted.Complete(`{}`),
			}}, tc.policy, echoTool("read", agentrt.ReadOnly), echoTool("wipe", agentrt.Destructive))
			run, err := d.Start(context.Background(), "g", limits(10, 3))
			if err != nil {
				t.Fatal(err)
			}
			steps, _ := h.store.ListSteps(context.Background(), run.ID)
			for i, want := range []string{tc.id, "", tc.id, ""} {
				if steps[i].PolicyID != want {
					t.Fatalf("step %d policy id %q, want %q", i, steps[i].PolicyID, want)
				}
			}
			events, _ := h.store.ListEvents(context.Background(), run.ID)
			_, payloads := policyEvents(t, events)
			if len(payloads) != 2 {
				t.Fatalf("%d step.policy events", len(payloads))
			}
			for _, p := range payloads {
				if tc.id == "" && p.PolicyID != nil || tc.id != "" && (p.PolicyID == nil || *p.PolicyID != tc.id) {
					t.Fatalf("payload policy_id %v, want %q", p.PolicyID, tc.id)
				}
			}
		})
	}
}

// An identity that could not be shown to an operator as it is, too long,
// not UTF-8, or holding a control character, is refused by NewDriver and
// NewGate; 256 bytes is accepted.
func TestPolicyID_MalformedIsRefused(t *testing.T) {
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, ok := range map[string]bool{
		strings.Repeat("p", 256): true,
		"policy v1 – ünïcode":    true,
		strings.Repeat("p", 257): false,
		"bad \xff utf-8":         false,
		"line\nbreak":            false,
		"tab\tin":                false,
		"del\x7f":                false,
		"c1\u0085":               false,
	} {
		p := namedPolicy{agentrt.DefaultPolicy(), id}
		_, derr := agentrt.NewDriver(agentrt.Config{Store: st, Agent: &scripted.Agent{}, Policy: p})
		_, gerr := agentrt.NewGate(agentrt.GateConfig{Store: st, Policy: p})
		if (derr == nil) != ok || (gerr == nil) != ok {
			t.Fatalf("identity %q: NewDriver %v, NewGate %v, want accepted=%v", id, derr, gerr, ok)
		}
	}
}

// What each step.policy event records of the run is what the policy was
// handed for it, on a model-backed loop whose totals grow under the
// policy's view: the run rebuilt from run.created and the event equals the
// RunView's run field by field.
func TestPolicyView_IsWhatThePolicySawInTheLoop(t *testing.T) {
	h := newHarness(t, ":memory:")
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	m := &replay.Scripted{ModelName: "test-model", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), toolUse("write", `{"n":2}`), toolUse("read", `{"n":3}`), done}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: modelAgent{}, Policy: p, Now: tick(),
		Tools:    []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("write", agentrt.LocalMutation)},
		Model:    &agentrt.ModelConfig{Model: m, Prices: agentrt.PriceTable{"test-model": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}, Backoff: time.Millisecond, CallTimeout: time.Second},
		Observer: func(e agentrt.Event) { h.events = append(h.events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "read and write", modelLimits())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	requireSeen(t, h.store, h.events, p)
	last := p.views[len(p.views)-1].Run
	if last.ModelCalls == 0 || last.EstimatedCost == 0 || last.ActiveTime == 0 || last.StepCount != 3 {
		t.Fatalf("the last view carried nothing to record: %+v", last)
	}
	if last.ModelCalls == run.ModelCalls || last.ActiveTime == run.ActiveTime {
		t.Fatalf("the stored run %+v says what the policy saw %+v; the test shows nothing", run, last)
	}
}

// On resume the policy sees the WAITING run, the steps before the approved
// one, and every approval; the resume-time step.policy records that.
func TestPolicyView_IsWhatThePolicySawOnResume(t *testing.T) {
	h := newHarness(t, ":memory:")
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Policy: p, Now: tick(),
		Agent: &scripted.Agent{Decisions: []agentrt.Decision{
			scripted.ToolCall("read", `{"n":1}`, ""),
			scripted.ToolCall("publish", `{"n":2}`, "ship"),
			scripted.Complete(`{}`),
		}},
		Tools:    []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("publish", agentrt.RemoteMutation)},
		Observer: func(e agentrt.Event) { h.events = append(h.events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := d.Start(ctx, "publish", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusWaitingForApproval, "")
	approvals, _ := h.store.ListApprovals(ctx, run.ID)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "op", ""); err != nil {
		t.Fatal(err)
	}
	if run, err = d.Resume(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	requireSeen(t, h.store, h.events, p)
	if got := p.views[2].Run.Status; got != agentrt.StatusWaitingForApproval || len(p.views[2].Approvals) != 1 {
		t.Fatalf("the resume-time view is %s with %d approvals", got, len(p.views[2].Approvals))
	}
}

// A gate's caller gets the same record: each Propose's step.policy holds
// what its policy saw, and the identity of an identified policy.
func TestPolicyView_IsWhatThePolicySawThroughAGate(t *testing.T) {
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := &seeingPolicy{Policy: agentrt.DefaultPolicy()}
	var events []agentrt.Event
	g, err := agentrt.NewGate(agentrt.GateConfig{Store: st, Policy: namedPolicy{p, "gate-policy"}, Now: tick(),
		Tools:    []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("wipe", agentrt.Destructive), echoTool("publish", agentrt.RemoteMutation)},
		Observer: func(e agentrt.Event) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := g.Begin(ctx, "gated", "g", limits(10, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, dec := range []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("wipe", `{"n":1}`, ""), scripted.ToolCall("publish", `{"n":1}`, "")} {
		step, err := s.Step(ctx)
		if err != nil || step == nil {
			t.Fatalf("step: %v", err)
		}
		v, err := step.Propose(ctx, dec)
		if err != nil {
			t.Fatal(err)
		}
		if v.Outcome == agentrt.VerdictAllowed {
			if _, err := step.Execute(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if s.Run().Status != agentrt.StatusWaitingForApproval {
		t.Fatalf("run %s", s.Run().Status)
	}
	requireSeen(t, st, events, p)
	_, payloads := policyEvents(t, events)
	for _, pl := range payloads {
		if pl.PolicyID == nil || *pl.PolicyID != "gate-policy" {
			t.Fatalf("policy_id %v", pl.PolicyID)
		}
	}
}
