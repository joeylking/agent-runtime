package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// scenarioAgent decides by step index and records what it was given. At
// step cancelAt it cancels the context it was handed, which leaves the step
// in flight for Resume to find interrupted.
type scenarioAgent struct {
	decisions []Decision
	cancelAt  int
	cancel    context.CancelFunc
	inputs    []StepInput
	hook      func(in StepInput)
}

func (a *scenarioAgent) Decide(_ context.Context, in StepInput) (Decision, error) {
	a.inputs = append(a.inputs, in)
	if a.hook != nil {
		a.hook(in)
	}
	i := len(in.Steps)
	if i == a.cancelAt && a.cancel != nil {
		a.cancel()
		return Decision{}, errors.New("cancelled")
	}
	return a.decisions[i], nil
}

type policyCall struct {
	Req  ToolRequest
	View RunView
}

type scenarioPolicy struct {
	calls []policyCall
	hook  func(view RunView)
}

func (p *scenarioPolicy) Evaluate(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error) {
	p.calls = append(p.calls, policyCall{Req: req, View: view})
	if p.hook != nil {
		p.hook(view)
	}
	return DefaultPolicy().Evaluate(ctx, req, view)
}

type funcTool struct {
	spec ToolSpec
	call func(ToolCall) (ToolResult, error)
}

func (f funcTool) Spec() ToolSpec                                         { return f.spec }
func (f funcTool) Call(_ context.Context, c ToolCall) (ToolResult, error) { return f.call(c) }

// scenarioStore is the store of the scenario running, for checks that run
// inside the driver's hooks.
var scenarioStore *Store

type recording struct {
	agent  []StepInput
	policy []policyCall
	events []Event
	reads  int
	stored string // every stored column of steps and approvals
}

// runScenario drives one run through an allowed step, a denial, an invalid
// decision, a tool error, an approval pause, a resume, a step interrupted
// while deciding, a second resume, and completion. Clock and ids are
// deterministic, so two scenarios differ only in how the loop knows its
// steps. Stored JSON differs from what the agent returned (whitespace,
// HTML characters), so the cache must hold the stored form.
func runScenario(t *testing.T, reload bool, agentHook func(StepInput), policyHook func(RunView), written func(*runCache)) recording {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scenarioStore = store
	schema := []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`)
	spec := func(name string, se SideEffect) ToolSpec {
		return ToolSpec{Name: name, Description: name, InputSchema: schema, SideEffect: se, Timeout: time.Second}
	}
	tools := []Tool{
		funcTool{spec("read", ReadOnly), func(c ToolCall) (ToolResult, error) {
			return ToolResult{Content: []byte(fmt.Sprintf(`{ "args" : %s, "html": "<b>&amp;</b>", "pad": [1, 2,  3] }`, c.Args)), Summary: "read <ok>"}, nil
		}},
		funcTool{spec("flaky", LocalMutation), func(ToolCall) (ToolResult, error) { return ToolResult{}, errors.New("flaked") }},
		funcTool{spec("push", RemoteMutation), func(c ToolCall) (ToolResult, error) { return ToolResult{Content: c.Args, Summary: "pushed"}, nil }},
		funcTool{spec("wipe", Destructive), func(ToolCall) (ToolResult, error) { return ToolResult{}, nil }},
	}
	call := func(tool, args, reason string) Decision {
		return Decision{Kind: DecideToolCall, Tool: tool, Args: []byte(args), Reason: reason}
	}
	agent := &scenarioAgent{hook: agentHook, cancelAt: 6, decisions: []Decision{
		call("read", `{ "n" : 1 }`, "look <first>"),
		call("wipe", `{}`, "denied"),
		call("nope", `{}`, "invalid"),
		call("flaky", `{"n":2}`, "tool error"),
		call("read", `{"n":2}`, ""),
		call("push", `{"n": 3}`, "needs approval"),
		{},
		call("read", `{"n":4}`, "after the interruption"),
		{Kind: DecideComplete, Result: []byte(`{ "ok" : true }`)},
	}}
	policy := &scenarioPolicy{hook: policyHook}
	clock := time.Unix(1700000000, 0)
	var rec recording
	ids := 0
	d, err := NewDriver(Config{Store: store, Agent: agent, Policy: policy, Tools: tools,
		Observer: func(e Event) { rec.events = append(rec.events, e) },
		Now:      func() time.Time { rec.reads++; clock = clock.Add(1500 * time.Microsecond); return clock },
		NewID:    func() string { ids++; return fmt.Sprintf("id-%03d", ids) }})
	if err != nil {
		t.Fatal(err)
	}
	d.reload, d.written = reload, written
	ctx := context.Background()
	run, err := d.Start(ctx, "g", Limits{MaxSteps: 20, MaxConsecutiveToolFailures: 5, LoopThreshold: 5})
	if err != nil || run.Status != StatusWaitingForApproval {
		t.Fatalf("start: %v, %s %s", err, run.Status, run.ReasonDetail)
	}
	approvals, _ := store.ListApprovals(ctx, run.ID)
	if err := d.Approve(ctx, run.ID, approvals[0].ID, "joey", ""); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	agent.cancel = cancel
	if _, err := d.Resume(cctx, run.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("resume: %v", err)
	}
	agent.cancel = nil
	run, err = d.Resume(ctx, run.ID)
	if err != nil || run.Status != StatusCompleted {
		t.Fatalf("second resume: %v, %s %s", err, run.Status, run.ReasonDetail)
	}
	rec.agent, rec.policy = agent.inputs, policy.calls
	for _, q := range []string{`SELECT * FROM steps ORDER BY rowid`, `SELECT * FROM approvals ORDER BY rowid`, `SELECT * FROM runs`} {
		rows, err := store.DB().Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			rec.stored += fmt.Sprintln(vals...)
		}
		rows.Close()
	}
	return rec
}

// What the agent and the policy receive from the kept steps is, field for
// field, what they received when the loop reread the store at every step,
// through a pause, a resume, and an interruption; the events, the clock
// readings, and every stored column are the same too, so text lent from
// an earlier write of a step is what encoding it again would give.
func TestDriver_CachedViewEqualsReloadedView(t *testing.T) {
	old := runScenario(t, true, nil, nil, nil)
	cached := runScenario(t, false, nil, nil, nil)
	if len(old.agent) != 9 || len(old.policy) != 7 {
		t.Fatalf("scenario made %d decisions and %d policy calls", len(old.agent), len(old.policy))
	}
	for i := range old.agent {
		if !reflect.DeepEqual(old.agent[i], cached.agent[i]) {
			t.Errorf("agent input %d differs:\n reload %+v\n cached %+v", i, old.agent[i], cached.agent[i])
		}
	}
	for i := range old.policy {
		if !reflect.DeepEqual(old.policy[i], cached.policy[i]) {
			t.Errorf("policy call %d differs:\n reload %+v\n cached %+v", i, old.policy[i], cached.policy[i])
		}
	}
	if len(old.agent) != len(cached.agent) || len(old.policy) != len(cached.policy) {
		t.Errorf("calls: reload %d/%d, cached %d/%d", len(old.agent), len(old.policy), len(cached.agent), len(cached.policy))
	}
	if !reflect.DeepEqual(old.events, cached.events) {
		t.Error("events differ")
	}
	if old.stored != cached.stored {
		t.Errorf("stored rows differ:\n reload %s\n cached %s", old.stored, cached.stored)
	}
	if old.reads != cached.reads {
		t.Errorf("clock readings: reload %d, cached %d", old.reads, cached.reads)
	}
}

// After every write the loop commits, its kept steps and approvals, and a
// handout of them, equal a fresh load from the store.
func TestDriver_CacheEqualsStoreAfterEveryWrite(t *testing.T) {
	seen := map[StepStatus]bool{}
	writes := 0
	check := func(c *runCache) {
		writes++
		if len(c.steps) == 0 || c.stale {
			t.Fatalf("write %d: cache has %d steps, stale %v", writes, len(c.steps), c.stale)
		}
		ctx := context.Background()
		store := scenarioStore
		steps, err := store.ListSteps(ctx, c.steps[0].RunID)
		if err != nil {
			t.Fatal(err)
		}
		approvals, err := store.ListApprovals(ctx, c.steps[0].RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.steps, steps) {
			t.Errorf("write %d: steps differ from the store:\n cache %+v\n store %+v", writes, c.steps, steps)
		}
		if !reflect.DeepEqual(c.approvals, approvals) {
			t.Errorf("write %d: approvals differ from the store:\n cache %+v\n store %+v", writes, c.approvals, approvals)
		}
		var m mirror
		if hs, ha := m.view(c, len(c.steps)); !reflect.DeepEqual(hs, steps) || !reflect.DeepEqual(ha, approvals) {
			t.Errorf("write %d: handout differs from the store", writes)
		}
		for _, st := range c.steps {
			seen[st.Status] = true
			if st.Observation != nil {
				seen[StepStatus(st.Observation.Kind)] = true
			}
		}
	}
	runScenario(t, false, nil, nil, check)
	for _, s := range []StepStatus{StepDone, StepFailed, StepAwaitingApproval, StepInterrupted, StepStatus(ObservePolicyDenied), StepStatus(ObserveInvalidDecision), StepStatus(ObserveToolError)} {
		if !seen[s] {
			t.Errorf("no write checked with a step %s", s)
		}
	}
	if writes < 20 {
		t.Errorf("only %d writes checked", writes)
	}
}

// A write that fails rolls back and leaves the kept steps and approvals as
// they were.
func TestDriver_FailedWriteLeavesCacheUnchanged(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	d, err := NewDriver(Config{Store: store, Agent: &listAgent{}, Policy: DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	run := Run{ID: "r1", Goal: "g", Status: StatusRunning, Limits: Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, StepCount: 1, CreatedAt: now, StartedAt: now}
	step := Step{ID: "s0", RunID: "r1", Index: 0, Status: StepDeciding, Decision: &Decision{Kind: DecideToolCall, Tool: "t", Args: []byte(`{}`)}, StartedAt: now}
	if err := store.tx(ctx, nil, func(t *txn) error {
		if err := t.insertRun(ctx, run); err != nil {
			return err
		}
		return t.insertStep(ctx, step)
	}); err != nil {
		t.Fatal(err)
	}
	c, err := d.load(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	before := *c
	beforeSteps, beforeApprovals := cloneSteps(c.steps), cloneApprovals(c.approvals)
	boom := errors.New("boom")
	err = d.write(ctx, c, func(t *txn) error {
		next := step
		next.Status, next.Observation = StepFailed, &Observation{Kind: ObserveToolError, Content: []byte(`{}`)}
		if err := t.updateStep(ctx, next, StepDeciding); err != nil {
			return err
		}
		if err := t.insertStep(ctx, Step{ID: "s1", RunID: "r1", Index: 1, Status: StepDeciding, StartedAt: now}); err != nil {
			return err
		}
		if err := t.insertApproval(ctx, Approval{ID: "a1", RunID: "r1", StepID: "s0", Status: ApprovalPending, CreatedAt: now}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("write: %v", err)
	}
	if !reflect.DeepEqual(c.steps, beforeSteps) || !reflect.DeepEqual(c.approvals, beforeApprovals) || !reflect.DeepEqual(c.version, before.version) || c.apprVersion != before.apprVersion || c.stale {
		t.Fatalf("cache advanced on a failed write: %+v", c)
	}
	fresh, _ := store.ListSteps(ctx, "r1")
	if !reflect.DeepEqual(fresh, c.steps) {
		t.Fatalf("store changed: %+v", fresh)
	}
}

// corrupt writes through everything a consumer was handed: fields, the
// structs behind pointers, slice elements, and, when bytes is set, the
// bytes of every RawMessage.
func corrupt(steps []Step, approvals []Approval, bytes bool) {
	scribble := func(b json.RawMessage) {
		if bytes {
			for i := range b {
				b[i] = 'X'
			}
		}
	}
	for i := range steps {
		st := &steps[i]
		if st.Decision != nil {
			st.Decision.Tool, st.Decision.Kind = "corrupt", "corrupt"
			scribble(st.Decision.Args)
			scribble(st.Decision.Result)
		}
		if st.Policy != nil {
			st.Policy.Outcome = Deny
			scribble(st.Policy.Capability)
		}
		if st.Observation != nil {
			st.Observation.Kind, st.Observation.ContentHash = ObserveToolResult, "corrupt"
			scribble(st.Observation.Content)
		}
		st.Status, st.Decision = "corrupt", nil
	}
	for i := range approvals {
		a := &approvals[i]
		a.Hash, a.Status, a.Request.Spec.Name = "corrupt", ApprovalApproved, "corrupt"
		scribble(a.Capability)
		scribble(a.Request.Args)
	}
}

// Neither consumer can reach the driver's steps or the other's view. One
// writes through everything it is handed, bytes included; the other checks
// that each view equals a fresh load and then writes through its fields.
func TestDriver_ConsumersCannotCorruptEachOther(t *testing.T) {
	for _, byteWriter := range []string{"agent", "policy"} {
		t.Run(byteWriter, func(t *testing.T) {
			ctx := context.Background()
			fresh := func(runID string) ([]Step, []Approval) {
				steps, _ := scenarioStore.ListSteps(ctx, runID)
				approvals, _ := scenarioStore.ListApprovals(ctx, runID)
				return steps, approvals
			}
			checked := 0
			checkThenCorrupt := func(run Run, steps []Step, approvals []Approval) {
				want, wantA := fresh(run.ID)
				// The step being decided is in the store but not in the view.
				if len(want) > 0 && want[len(want)-1].Status == StepDeciding {
					want = want[:len(want)-1]
				}
				if len(steps) > 0 && !reflect.DeepEqual(steps, want[:len(steps)]) || !reflect.DeepEqual(approvals, wantA) {
					t.Errorf("view differs from the store:\n view  %+v\n store %+v", steps, want)
				}
				checked++
				corrupt(steps, approvals, false)
			}
			agentHook := func(in StepInput) {
				if byteWriter == "agent" {
					corrupt(in.Steps, in.Approvals, true)
					return
				}
				checkThenCorrupt(in.Run, in.Steps, in.Approvals)
			}
			policyHook := func(v RunView) {
				if byteWriter == "policy" {
					corrupt(v.Steps, v.Approvals, true)
					return
				}
				checkThenCorrupt(v.Run, v.Steps, v.Approvals)
			}
			written := func(c *runCache) {
				steps, approvals := fresh(c.steps[0].RunID)
				if !reflect.DeepEqual(c.steps, steps) || !reflect.DeepEqual(c.approvals, approvals) {
					t.Errorf("the driver's steps differ from the store")
				}
			}
			clean := runScenario(t, false, nil, nil, nil)
			got := runScenario(t, false, agentHook, policyHook, written)
			if checked == 0 {
				t.Fatal("nothing checked")
			}
			if !reflect.DeepEqual(clean.events, got.events) {
				t.Error("the run's events changed")
			}
		})
	}
}

// A field written twice with different values is decoded again, not lent
// from the first write, and an unchanged one is lent.
func TestDriver_CacheDecodesRewrittenFields(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	d, err := NewDriver(Config{Store: store, Agent: &listAgent{}, Policy: DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	run := Run{ID: "r1", Goal: "g", Status: StatusRunning, Limits: Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}, CreatedAt: now, StartedAt: now}
	if err := store.tx(ctx, nil, func(t *txn) error { return t.insertRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	c, err := d.load(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	step := Step{ID: "s0", RunID: "r1", Index: 0, Status: StepDeciding, Decision: &Decision{Kind: DecideToolCall, Tool: "t", Args: []byte(`{ "a" : 1 }`)}, StartedAt: now}
	write := func(from StepStatus, insert bool) {
		t.Helper()
		if err := d.write(ctx, c, func(t *txn) error {
			if insert {
				return t.insertStep(ctx, step)
			}
			return t.updateStep(ctx, step, from)
		}); err != nil {
			t.Fatal(err)
		}
		fresh, _ := store.ListSteps(ctx, "r1")
		if !reflect.DeepEqual(c.steps, fresh) {
			t.Fatalf("cache %+v, store %+v", c.steps[0], fresh[0])
		}
	}
	write("", true)
	decoded := c.steps[0].Decision
	step.Status, step.Observation = StepExecuting, &Observation{Kind: ObserveToolResult, Content: []byte(`{"n":1}`)}
	write(StepDeciding, false)
	step.Status, step.Observation = StepDone, &Observation{Kind: ObserveToolError, Content: []byte(`{"n":2}`)}
	write(StepExecuting, false)
	if o := c.steps[0].Observation; o.Kind != ObserveToolError || string(o.Content) != `{"n":2}` {
		t.Errorf("observation = %+v", o)
	}
	if c.steps[0].Decision != decoded {
		t.Error("an unchanged decision was decoded again")
	}
}
