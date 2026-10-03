// Package testkit is what a consumer uses to test its tools, policy, and
// agent against the runtime without a model: an interruption harness that
// crashes the loop at a chosen point and resumes it in a fresh Driver, a
// policy conformance table, schema fuzzing of tool arguments, and a render
// equivalence check. It is built on the public API alone and on the
// scripted package, which it re-exports nothing of.
//
// A crash is a real one as far as the runtime can tell: the hook at the
// crash point parks the goroutine executing the run and the harness closes
// its Store, so the heartbeat stops and the lease is never released, which
// is what a process that died leaves behind. Resume in a fresh Driver on a
// fresh open of the same file is refused with ErrRunLeased until that lease
// expires, as a restarted process is, so the harness sets a short
// Config.LeaseTTL and waits it out once per crash. The parked goroutine is
// released when the test ends; its writes fail against the closed Store
// and nothing of its reaches the file.
package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// Point is where a Crash stops the loop: right after the named event of a
// step has been committed and before whatever follows it, or, for
// AfterToolEffect, after the tool returned and before its outcome is
// recorded. Each is a point a dying process could stop at.
type Point string

const (
	// AfterStepStarted stops after step.started: the step exists, the
	// agent has not decided. Resume marks it interrupted while deciding
	// and continues.
	AfterStepStarted Point = agentrt.EventStepStarted
	// AfterDecided stops after step.decided: the decision is recorded,
	// policy has not seen it. Resume marks the step interrupted while
	// deciding; the request is never executed.
	AfterDecided Point = agentrt.EventStepDecided
	// AfterToolStarted stops after step.tool_started: the start is
	// recorded and the tool never ran. The record cannot tell this from
	// AfterToolEffect, so Resume treats both the same.
	AfterToolStarted Point = agentrt.EventStepToolStarted
	// AfterToolEffect stops once the tool has returned, its effect done,
	// and before the outcome is recorded: the step is left executing.
	AfterToolEffect Point = "tool.effect"
	// AfterToolFinished stops after step.tool_finished: the step is
	// complete and the next has not started.
	AfterToolFinished Point = agentrt.EventStepToolFinished
	// AfterResumed stops after run.resumed: on an approved resume that is
	// the transaction that also recorded the policy decision and the tool
	// start, so the approved request must not be executed again; on an
	// interrupted resume it is the move back to RUNNING before the loop.
	// Step is ignored.
	AfterResumed Point = agentrt.EventRunResumed
	// AfterInterrupted stops after step.interrupted: a resume took the run
	// over and died before reconciling or pausing it. The next resume must
	// still know the step was executing.
	AfterInterrupted Point = agentrt.EventStepInterrupted
)

// Crash names a point at which the harness stops the loop. Step is the
// index of the step the event belongs to; it is ignored for a run-level
// Point. Every pause is a stop as well, with no Crash needed: the loop
// returns, the Operator decides, and a fresh Driver resumes.
type Crash struct {
	Step int
	At   Point
}

func (c Crash) String() string { return fmt.Sprintf("step %d %s", c.Step, c.At) }

// Verdict is an Operator's answer to a pending approval.
type Verdict int

const (
	// Approve grants it and the harness resumes the run in a fresh Driver.
	Approve Verdict = iota
	// Reject refuses it; the runtime cancels the run.
	Reject
	// Leave decides nothing and the harness returns the waiting run.
	Leave
)

// Scenario is one run's worth of consumer code and script: what a
// consumer hands NewDriver, plus the operator's answers.
type Scenario struct {
	Tools  []agentrt.Tool
	Policy agentrt.Policy
	// Agent is usually a scripted.Agent or the consumer's own replayer. It
	// is handed to every Driver the harness builds, so it must decide from
	// the recorded steps alone, as the runtime requires anyway. A replayer
	// keyed by step index skips the decision of a step that was
	// interrupted, because the interrupted step keeps its index.
	Agent agentrt.Agent
	Goal  string
	// Limits for the run. A zero MaxSteps means agentrt.DefaultLimits.
	Limits agentrt.Limits
	// Reconcile is the consumer's Config.Reconcile; nil leaves the runtime
	// to pause an interrupted side effect for an operator.
	Reconcile func(context.Context, agentrt.RunView) (agentrt.Reconciliation, error)
	// Operator answers each pending approval. Nil approves every one.
	Operator func(agentrt.Approval) Verdict
	// Now and NewID are passed to every Driver, so a deterministic clock
	// and id sequence make two runs of one Scenario record identically.
	Now   func() time.Time
	NewID func() string
	// DB is the database file to run in, for a consumer whose own tables
	// share the runtime's file. Empty means a file of the harness's own.
	DB string
	// LeaseTTL is Config.LeaseTTL for every Driver. Zero means one second.
	// A crashed Driver never releases its lease, Resume refuses the run
	// until the lease expires, and the harness waits that long per crash,
	// as a restarted process would; a value too short for the machine
	// makes a live loop lose its own lease between heartbeats.
	LeaseTTL time.Duration
}

// Call is one invocation a consumer tool received from the runtime.
type Call struct {
	Step int
	Tool string
	Args json.RawMessage
	// Returned is false for the call a Crash at AfterToolEffect stopped
	// after: the tool did its work and the runtime never recorded it.
	Returned bool
}

// Result is what a Scenario left behind, read from a fresh open of the
// file after the last Driver returned.
type Result struct {
	RunID     string
	Run       agentrt.Run
	Steps     []agentrt.Step
	Approvals []agentrt.Approval
	Events    []agentrt.Event
	// Calls is every tool invocation across every Driver, in order.
	Calls []Call
	// Inputs is what the Agent was handed at every decision across every
	// Driver, in order, for rendering checks.
	Inputs []agentrt.StepInput
	// Crashes are the crashes that fired, in order.
	Crashes []Crash
	// Resumes counts every Driver that ran the run after the first, which
	// started it: the resume after an operator's approval as well as the
	// resume after a crash. A resume refused while a crashed Driver's
	// lease is live, and the Driver that records the operator's decision,
	// are not counted. A run that pauses for one approval and crashes once
	// after it is resumed is started (Driver 1), resumed after the
	// approval (Driver 2, which crashes), and resumed after the crash
	// (Driver 3): Resumes is 3 - 1 = 2, with one Crash.
	Resumes int
}

// Run executes sc in a fresh file-backed store, crashing at each Crash in
// turn and resuming in a fresh Driver each time, until the run is terminal
// or an Operator leaves it waiting. It then asserts what the runtime
// promises across an interruption: a step never executes twice on its own
// (every execution past the first is under an approval of that step), an
// approved request executes at most once, every recorded tool start is
// followed by its finish or by the step's interruption, the run ends
// terminal or waiting with no step in flight, no run is left leased, and
// every Crash was reached. Failures are reported with t.Errorf, so a test
// sees all of them, and the Result is returned regardless.
func Run(t testing.TB, sc Scenario, crashes ...Crash) Result {
	t.Helper()
	h := newHarness(t, sc, crashes)
	t.Cleanup(h.cleanup)
	ctx := context.Background()
	out := h.call(func(d *agentrt.Driver) (agentrt.Run, error) {
		return d.Start(ctx, sc.Goal, h.limits())
	})
	for {
		switch {
		case out.crashed:
			out = h.resume(ctx)
			continue
		case out.err != nil:
			t.Errorf("testkit: driver: %v", out.err)
		case out.run.Status == agentrt.StatusWaitingForApproval:
			if h.decide(ctx, out.run) {
				out = h.resume(ctx)
				continue
			}
		}
		break
	}
	res := h.result(ctx)
	h.check(res)
	return res
}

// outcome is what one Driver call ended with.
type outcome struct {
	run     agentrt.Run
	err     error
	crashed bool
}

// parked is a goroutine stopped at a crash point until the test ends.
type parked struct {
	release chan struct{}
	done    chan struct{}
}

type harness struct {
	t       testing.TB
	sc      Scenario
	path    string
	crashes []Crash
	runID   string
	drivers int

	mu     sync.Mutex
	index  map[string]int // step id to index, from step.started
	calls  []Call
	inputs []agentrt.StepInput
	fired  []Crash
	parked []*parked
}

func newHarness(t testing.TB, sc Scenario, crashes []Crash) *harness {
	h := &harness{t: t, sc: sc, path: sc.DB, crashes: crashes, index: map[string]int{}}
	if h.path == "" {
		h.path = filepath.Join(t.TempDir(), "testkit.db")
	}
	if h.sc.LeaseTTL <= 0 {
		h.sc.LeaseTTL = time.Second
	}
	return h
}

func (h *harness) limits() agentrt.Limits {
	if h.sc.Limits.MaxSteps == 0 {
		return agentrt.DefaultLimits()
	}
	return h.sc.Limits
}

// armed is the next crash that has not fired.
func (h *harness) armed() *Crash {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.fired) < len(h.crashes) {
		c := h.crashes[len(h.fired)]
		return &c
	}
	return nil
}

// call runs one Driver call against a fresh open of the file. It returns
// when the call does, or when the armed crash fires, having closed the
// Store underneath the parked goroutine.
func (h *harness) call(fn func(*agentrt.Driver) (agentrt.Run, error)) outcome {
	h.t.Helper()
	st, err := agentrt.OpenStore(h.path)
	if err != nil {
		h.t.Fatalf("testkit: open %s: %v", h.path, err)
	}
	h.drivers++
	crash := h.armed()
	p := &parked{release: make(chan struct{}), done: make(chan struct{})}
	fired := make(chan struct{}, 1)
	park := func() {
		select {
		case fired <- struct{}{}:
		default:
		}
		<-p.release
	}
	tools := make([]agentrt.Tool, len(h.sc.Tools))
	for i, tool := range h.sc.Tools {
		tools[i] = &crashTool{Tool: tool, h: h, crash: crash, park: park}
	}
	cfg := agentrt.Config{
		Store: st, Agent: &recordingAgent{Agent: h.sc.Agent, h: h}, Policy: h.sc.Policy, Tools: tools,
		Observer:  h.observer(crash, park),
		Reconcile: h.sc.Reconcile, Now: h.sc.Now, NewID: h.sc.NewID,
		LeaseTTL: h.sc.LeaseTTL, LeaseOwner: fmt.Sprintf("testkit-%d", h.drivers),
	}
	d, err := agentrt.NewDriver(cfg)
	if err != nil {
		st.Close()
		h.t.Fatalf("testkit: %v", err)
	}
	done := make(chan outcome, 1)
	go func() {
		defer close(p.done)
		r, err := fn(d)
		done <- outcome{run: r, err: err}
	}()
	select {
	case o := <-done:
		close(p.release)
		st.Close()
		return o
	case <-fired:
		// The process dies here: its Store closes, its heartbeat stops,
		// its lease stays. The goroutine is let go when the test ends.
		st.Close()
		h.mu.Lock()
		h.fired = append(h.fired, *crash)
		h.parked = append(h.parked, p)
		h.mu.Unlock()
		return outcome{crashed: true}
	}
}

// resume continues the run in a fresh Driver. A crashed Driver's lease is
// live until it expires, and Resume refuses it meanwhile, so the harness
// waits out the expiry the refusal names, as casework's RecoverAll and a
// restarted repo-steward do.
func (h *harness) resume(ctx context.Context) outcome {
	h.t.Helper()
	for attempt := 0; ; attempt++ {
		out := h.call(func(d *agentrt.Driver) (agentrt.Run, error) {
			return d.Resume(ctx, h.runID)
		})
		var leased agentrt.ErrRunLeased
		if !errors.As(out.err, &leased) || attempt >= 20 {
			return out
		}
		h.drivers-- // a refused resume built no loop
		time.Sleep(time.Until(leased.ExpiresAt) + 20*time.Millisecond)
	}
}

// decide asks the Operator about the run's pending approval and reports
// whether the run should be resumed.
func (h *harness) decide(ctx context.Context, run agentrt.Run) bool {
	h.t.Helper()
	st, err := agentrt.OpenStore(h.path)
	if err != nil {
		h.t.Fatalf("testkit: open %s: %v", h.path, err)
	}
	defer st.Close()
	approvals, err := st.ListApprovals(ctx, run.ID)
	if err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	var pending *agentrt.Approval
	for i := range approvals {
		if approvals[i].Status == agentrt.ApprovalPending {
			pending = &approvals[i]
		}
	}
	if pending == nil {
		h.t.Errorf("testkit: run %s is waiting with no pending approval", run.ID)
		return false
	}
	verdict := Approve
	if h.sc.Operator != nil {
		verdict = h.sc.Operator(*pending)
	}
	if verdict == Leave {
		return false
	}
	// The decision is stamped by the driver's clock, as a consumer's
	// Driver.Approve would, so a deterministic Now governs expiry too.
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Agent: h.sc.Agent, Policy: h.sc.Policy, Tools: h.sc.Tools, Now: h.sc.Now, NewID: h.sc.NewID})
	if err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	if verdict == Reject {
		if err := d.Reject(ctx, run.ID, pending.ID, "testkit", "rejected by the scenario's operator"); err != nil {
			h.t.Errorf("testkit: reject: %v", err)
		}
		return false
	}
	if err := d.Approve(ctx, run.ID, pending.ID, "testkit", "approved by the scenario's operator"); err != nil {
		h.t.Errorf("testkit: approve: %v", err)
		return false
	}
	return true
}

// observer records step indexes and fires the armed crash at its event.
func (h *harness) observer(crash *Crash, park func()) agentrt.Observer {
	return func(e agentrt.Event) {
		h.mu.Lock()
		if h.runID == "" {
			h.runID = e.RunID
		}
		h.mu.Unlock()
		if e.Type == agentrt.EventStepStarted {
			var p struct {
				Index int `json:"index"`
			}
			if json.Unmarshal(e.Payload, &p) == nil {
				h.mu.Lock()
				h.index[e.StepID] = p.Index
				h.mu.Unlock()
			}
		}
		if crash == nil || string(crash.At) != e.Type {
			return
		}
		if crash.At != AfterResumed && h.stepIndex(e.StepID) != crash.Step {
			return
		}
		park()
	}
}

func (h *harness) stepIndex(stepID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i, ok := h.index[stepID]; ok {
		return i
	}
	return -1
}

// crashTool records every call and, armed with AfterToolEffect for its
// step, parks after the tool has returned.
type crashTool struct {
	agentrt.Tool
	h     *harness
	crash *Crash
	park  func()
}

func (c *crashTool) Call(ctx context.Context, call agentrt.ToolCall) (agentrt.ToolResult, error) {
	step := c.h.stepIndex(call.StepID)
	c.h.mu.Lock()
	c.h.calls = append(c.h.calls, Call{Step: step, Tool: c.Spec().Name, Args: append(json.RawMessage(nil), call.Args...), Returned: true})
	at := len(c.h.calls) - 1
	c.h.mu.Unlock()
	res, err := c.Tool.Call(ctx, call)
	if c.crash != nil && c.crash.At == AfterToolEffect && c.crash.Step == step {
		c.h.mu.Lock()
		c.h.calls[at].Returned = false
		c.h.mu.Unlock()
		c.park()
	}
	return res, err
}

// recordingAgent keeps what the agent was handed.
type recordingAgent struct {
	agentrt.Agent
	h *harness
}

func (a *recordingAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	a.h.mu.Lock()
	a.h.inputs = append(a.h.inputs, in)
	a.h.mu.Unlock()
	return a.Agent.Decide(ctx, in)
}

// result reads the run from a fresh open of the file.
func (h *harness) result(ctx context.Context) Result {
	h.t.Helper()
	st, err := agentrt.OpenStore(h.path)
	if err != nil {
		h.t.Fatalf("testkit: open %s: %v", h.path, err)
	}
	defer st.Close()
	h.mu.Lock()
	res := Result{RunID: h.runID, Calls: append([]Call(nil), h.calls...), Inputs: append([]agentrt.StepInput(nil), h.inputs...), Crashes: append([]Crash(nil), h.fired...), Resumes: h.drivers - 1}
	h.mu.Unlock()
	if h.runID == "" {
		h.t.Errorf("testkit: no run was started")
		return res
	}
	if res.Run, err = st.GetRun(ctx, h.runID); err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	if res.Steps, err = st.ListSteps(ctx, h.runID); err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	if res.Approvals, err = st.ListApprovals(ctx, h.runID); err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	if res.Events, err = st.ListEvents(ctx, h.runID); err != nil {
		h.t.Fatalf("testkit: %v", err)
	}
	owner, _, err := st.Lease(ctx, h.runID)
	if err != nil {
		h.t.Fatalf("testkit: lease: %v", err)
	}
	if owner != "" {
		h.t.Errorf("testkit: run %s is left leased by %q", h.runID, owner)
	}
	return res
}

// check asserts the interruption contract over the record.
func (h *harness) check(res Result) {
	h.t.Helper()
	t := h.t
	if res.RunID == "" {
		return
	}
	if len(res.Crashes) < len(h.crashes) {
		t.Errorf("testkit: crash %s was never reached; the run ended %s/%s after %d step(s)", h.crashes[len(res.Crashes)], res.Run.Status, res.Run.Reason, res.Run.StepCount)
	}
	if res.Run.Status == agentrt.StatusRunning || res.Run.Status == agentrt.StatusInterrupted {
		t.Errorf("testkit: run %s is left %s", res.RunID, res.Run.Status)
	}
	for _, st := range res.Steps {
		switch st.Status {
		case agentrt.StepDeciding, agentrt.StepExecuting:
			t.Errorf("testkit: step %d is left %s", st.Index, st.Status)
		case agentrt.StepAwaitingApproval:
			if res.Run.Status != agentrt.StatusWaitingForApproval {
				t.Errorf("testkit: step %d is awaiting approval under a %s run", st.Index, res.Run.Status)
			}
		}
	}
	approved := map[string]int{}
	for _, a := range res.Approvals {
		if a.Status == agentrt.ApprovalApproved {
			approved[a.StepID]++
		}
	}
	index := map[string]int{}
	for _, st := range res.Steps {
		index[st.ID] = st.Index
	}
	starts := map[string]int{}
	byApproval := map[string]int{}
	open := map[string]bool{} // step id with a tool start not yet closed
	for _, e := range res.Events {
		switch e.Type {
		case agentrt.EventStepToolStarted:
			starts[e.StepID]++
			var p struct {
				Approval string `json:"approval_id"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.Approval != "" {
				byApproval[p.Approval]++
			}
			if open[e.StepID] {
				t.Errorf("testkit: step %d started a tool again before the previous start was finished or interrupted (event %d)", index[e.StepID], e.Seq)
			}
			open[e.StepID] = true
		case agentrt.EventStepToolFinished, agentrt.EventStepInterrupted:
			delete(open, e.StepID)
		}
	}
	for id := range open {
		t.Errorf("testkit: step %d has a tool start with no finish and no interruption", index[id])
	}
	for id, n := range starts {
		if n > 1+approved[id] {
			t.Errorf("testkit: step %d executed %d times with %d approval(s): the runtime re-executed a step on its own", index[id], n, approved[id])
		}
	}
	for id, n := range byApproval {
		if n > 1 {
			t.Errorf("testkit: approval %s executed %d times", id, n)
		}
	}
	calls := map[int]int{}
	for _, c := range res.Calls {
		calls[c.Step]++
	}
	startsByIndex := map[int]int{}
	for id, n := range starts {
		startsByIndex[index[id]] = n
	}
	for step, n := range calls {
		if n > startsByIndex[step] {
			t.Errorf("testkit: step %d reached its tool %d time(s) but recorded %d start(s)", step, n, startsByIndex[step])
		}
	}
}

// cleanup lets every parked goroutine go and waits for it. Its writes fail
// against the closed Store it was given.
func (h *harness) cleanup() {
	h.mu.Lock()
	parked := h.parked
	h.parked = nil
	h.mu.Unlock()
	for _, p := range parked {
		close(p.release)
	}
	for _, p := range parked {
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			h.t.Errorf("testkit: a crashed driver did not return after release")
		}
	}
}

// String summarizes the run and its steps as "index tool:status", for
// messages.
func (r Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s %s", r.Run.Status, r.Run.Reason)
	for _, st := range r.Steps {
		tool := "-"
		if st.Decision != nil {
			tool = string(st.Decision.Kind)
			if st.Decision.Tool != "" {
				tool = st.Decision.Tool
			}
		}
		fmt.Fprintf(&b, " [%d %s:%s]", st.Index, tool, st.Status)
	}
	return b.String()
}
