package agentrt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// GateConfig configures a Gate. It is Config without an Agent: the loop
// that decides belongs to the caller.
type GateConfig struct {
	Store    *Store
	Policy   Policy
	Tools    []Tool
	Observer Observer
	// Model, when set, is wrapped in the ModelCaller each step hands out
	// from OpenStep.Model, charged to that step and held to the run's
	// limits.
	Model *ModelConfig
	// Reconcile, when set, runs before Attach continues a run found
	// mid-step, as Config.Reconcile does for Resume.
	Reconcile func(ctx context.Context, view RunView) (Reconciliation, error)
	// Now and NewID may be overridden by tests.
	Now   func() time.Time
	NewID func() string
	// LeaseTTL and LeaseOwner are Config's: the lease a session holds on
	// its run, renewed by a heartbeat at a third of the TTL, and the name
	// its owner id starts with. Lease times are read from the wall clock,
	// never from Now.
	LeaseTTL   time.Duration
	LeaseOwner string
}

// Gate holds the runtime's controls for a loop the runtime does not own,
// such as another agent framework's or a hand-written one. The caller
// decides; the gate records each decision, validates it, evaluates
// policy, pauses for approvals, enforces the limits, and runs the tools,
// with the same records, events, transactions, and clock readings as a
// Driver, which is a gate with an agent. A Gate is safe to reuse for many
// runs, and refuses a second session on a run it holds.
type Gate struct {
	d *Driver
}

// NewGate validates the configuration and compiles every tool schema.
func NewGate(cfg GateConfig) (*Gate, error) {
	d, err := newDriver(cfg)
	if err != nil {
		return nil, err
	}
	return &Gate{d: d}, nil
}

// Begin creates a run with a caller-chosen id, which must be unique, and
// opens a session on it, as Driver.StartWithID does before its first step:
// the run is RUNNING under this gate's lease, which the session holds
// until it ends or is closed.
func (g *Gate) Begin(ctx context.Context, runID, goal string, limits Limits) (*Session, error) {
	return g.d.begin(ctx, runID, goal, limits)
}

// Attach opens a session on an existing run, doing what Driver.Resume
// does before its loop. A WAITING run must have its awaiting step's latest
// approval approved; policy is re-evaluated, and the verdict on the
// recorded request is returned: Allowed leaves that step open, Current,
// for Execute, with the run still WAITING until Execute moves it to
// RUNNING, taking the lease, as it starts the tool. A RUNNING run whose
// lease is live and not this gate's, or that this gate holds in another
// session, is refused with ErrRunLeased and nothing changes. One whose
// lease has expired, or that has none, is taken over: the step in flight
// is marked interrupted, and GateConfig.Reconcile decides what follows;
// without it, a step interrupted while executing a tool that is not
// ReadOnly pauses as interrupted_side_effect. A verdict of Pending or
// Ended comes with a session that is already over; a zero verdict means
// the session is open with no step, ready for Step.
//
// An Allowed verdict is the policy's answer at the time it was given, and
// GrantTTL is judged here against the grant's decision. The approved step
// may wait for Execute, which judges GrantTTL again, so a grant is never
// executed after it expired however long the caller waits.
func (g *Gate) Attach(ctx context.Context, runID string) (*Session, Verdict, error) {
	s, v := g.d.attach(ctx, runID)
	if s.ended && s.err != nil {
		return nil, Verdict{}, s.err
	}
	return s, v, nil
}

// begin is what Start does before its loop, and all a Gate's Begin does.
func (d *Driver) begin(ctx context.Context, id, goal string, limits Limits) (*Session, error) {
	if err := limits.validate(); err != nil {
		return nil, fmt.Errorf("agentrt: %w", err)
	}
	if id == "" {
		return nil, errors.New("agentrt: run id is required")
	}
	if !d.claim(id) {
		return nil, d.busy(ctx, id)
	}
	now := d.now()
	run := Run{
		ID:        id,
		Goal:      goal,
		Status:    StatusRunning,
		Limits:    limits,
		CreatedAt: now,
		StartedAt: now,
	}
	at := d.wall()
	err := d.write(ctx, nil, func(t *txn) error {
		if err := t.insertRun(ctx, run); err != nil {
			return err
		}
		if err := t.emit(ctx, Event{RunID: run.ID, At: now, Type: EventRunCreated}, map[string]any{"goal": goal, "limits": limits}); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunStarted})
	})
	if err != nil {
		d.unclaim(id)
		return nil, fmt.Errorf("agentrt: start run: %w", err)
	}
	s := &Session{d: d, runID: id, l: d.newLease(id), claimed: true, run: run}
	s.l.start(at)
	return s, nil
}

// ErrSessionClosed is returned by a call on a session that was closed, or
// that an earlier call ended with an error, which it wraps.
var ErrSessionClosed = errors.New("agentrt: session is closed")

// Session is one hold on a run, from Begin or Attach until the run pauses
// or ends, the lease is lost, a call fails, or Close. It holds the run's
// lease throughout, renewed by a heartbeat, and has at most one open
// step. The call that pauses or ends the run reports it, and every later
// call fails: with ErrRunState once the run is paused or ended, with
// ErrLeaseLost once the lease is lost, and with ErrSessionClosed after
// Close or a failed call. A call that misuses the session, a second open
// step or an Execute without an Allowed verdict, fails with ErrRunState
// and changes nothing. Calls are serialised; the ModelCaller of the open
// step may be used concurrently, as an agent's may.
type Session struct {
	d     *Driver
	runID string
	l     *lease
	// c is the run's steps and approvals, loaded at the first step.
	c       *runCache
	claimed bool

	mu   sync.Mutex
	open *OpenStep
	// run is the run as the session last read or wrote it.
	run Run
	// ended is set by the call that ended the session, with the run and
	// error a Driver returns for it, and over is what later calls return.
	ended    bool
	out      Run
	err      error
	over     error
	approval *Approval
}

// Step checks the run's limits and starts its next step, recording
// step.started, as the Driver does before asking its agent. The step,
// limit, consecutive failure, active and elapsed time, and loop limits
// fire here: the run ends, and Step returns a nil step and no error, as
// it does when it finds the run no longer RUNNING because an operator
// cancelled it. Run then reports the run.
func (s *Session) Step(ctx context.Context) (*OpenStep, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil, s.over
	}
	if s.open != nil {
		return nil, fmt.Errorf("%w: run %s: step %d is still open", ErrRunState, s.runID, s.open.step.Index)
	}
	st := s.step(ctx)
	if st == nil {
		return nil, s.err
	}
	return st, nil
}

// Current is the session's open step, or nil: the step Step started, or
// the approved step Attach left for Execute, until it is over.
func (s *Session) Current() *OpenStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// Input is what an agent deciding at this point of the session would be
// handed in StepInput, for a loop that decides from the run's record as
// an agent does: copies, which the caller may change without changing the
// run's record or any later view but its own. While a step is open it is
// that step's input, as Decide receives it: the run as the step started,
// the steps before it, every approval, the tools, and the step's Model;
// for the approved step Attach left open, the run is the WAITING run and
// the steps those before it. With no step open it is the run as the
// session last read or wrote it and every step it has recorded, before
// the first Step none. It is served from the copy of the run's steps and
// approvals the session keeps as it writes: the first call of a session,
// or the first after a write the copy could not apply, loads them, which
// the next Step would otherwise have done and then does not, and a later
// call copies only the steps that changed since the previous one, so a
// call costs one shallow copy of each step and reads nothing from the
// store. A run whose stored rows cannot be decoded returns an error, and
// the next Step fails the run as it would have.
func (s *Session) Input(ctx context.Context) (StepInput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c == nil || s.c.stale {
		c, err := s.d.load(ctx, s.runID)
		if err != nil {
			return StepInput{}, err
		}
		if derr := decodeErrors(c.steps, c.approvals); derr != nil {
			// Left for Step, which fails the run on it.
			return StepInput{}, fmt.Errorf("agentrt: run %s: %w", s.runID, derr)
		}
		s.c = c
	}
	in := StepInput{Run: s.run, Tools: cloneSpecs(s.d.specs)}
	n := len(s.c.steps)
	if st := s.open; st != nil {
		in.Run, n, in.Model = st.run, st.n, st.mc
		if st.granted != nil {
			n = st.step.Index
		}
	}
	in.Steps, in.Approvals = s.c.agent.view(s.c, n)
	return in, nil
}

// cloneSpecs copies tool specs and their schemas.
func cloneSpecs(specs []ToolSpec) []ToolSpec {
	out := slices.Clone(specs)
	for i := range out {
		out[i].InputSchema = cloneBytes(out[i].InputSchema)
	}
	return out
}

// Run is the run as the session last read or wrote it: after the call
// that paused or ended it, the run as that call left it.
func (s *Session) Run() Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run
}

// Done reports that the session is over and every further call fails.
func (s *Session) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// Close ends a session that is not over, releasing its lease so another
// process need not wait for it to expire. An open step is left as the
// store has it, as a crash would leave it: one still deciding, or allowed
// and not executed, is never executed, and Attach marks it interrupted;
// an approved step Attach left open stays approved and WAITING. A
// session that is not over holds a heartbeat until it is closed, so Close
// it when it is no longer used; closing one that is over does nothing.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended, s.open = true, nil
	s.l.stop(Run{}, nil)
	s.release()
	s.over = ErrSessionClosed
}

// end ends the session with what the call ending it returns. The lease is
// released unless the run was paused or finished, which released it in
// the same transaction.
func (s *Session) end(out Run, err error) {
	s.ended, s.out, s.err, s.open = true, out, err, nil
	if out.ID != "" {
		s.run = out
	}
	s.l.stop(out, err)
	s.release()
	switch {
	case errors.Is(err, ErrLeaseLost):
		s.over = err
	case err != nil:
		s.over = fmt.Errorf("%w: %w", ErrSessionClosed, err)
	default:
		s.over = fmt.Errorf("%w: run %s is %s", ErrRunState, s.runID, out.Status)
	}
}

func (s *Session) release() {
	if s.claimed {
		s.claimed = false
		s.d.unclaim(s.runID)
	}
}

// ending is the verdict of a call that ended the session without an
// error: Pending on the approval it paused on, or Ended.
func (s *Session) ending() Verdict {
	if s.err != nil {
		return Verdict{}
	}
	if s.approval != nil {
		a := cloneApprovals([]Approval{*s.approval})[0]
		return Verdict{Outcome: VerdictPending, Approval: &a, Run: s.out}
	}
	return Verdict{Outcome: VerdictEnded, Run: s.out}
}

func (s *Session) finish(out Run, err error) Verdict {
	s.end(out, err)
	return s.ending()
}

// lose ends the session on a write that failed or a lease it no longer
// holds, as lost answers them.
func (s *Session) lose(ctx context.Context, err error) Verdict {
	return s.finish(s.d.lost(ctx, s.runID, err))
}

func (s *Session) settle(ctx context.Context, from []RunStatus, e ending) Verdict {
	return s.finish(s.d.settle(ctx, s.c, s.runID, from, e))
}

func (s *Session) pause(r Run, a Approval, err error) Verdict {
	if err != nil {
		return s.finish(Run{}, err)
	}
	s.approval = &a
	return s.finish(r, nil)
}

// OpenStep is a session's step in flight. Its decision is proposed once,
// and an Allowed one executed once; Fail ends it without a decision.
type OpenStep struct {
	s    *Session
	step Step
	// run is the run as the step started and n the steps before it, which
	// the policy sees.
	run Run
	n   int
	mc  ModelCaller
	// An Allowed verdict leaves what execute starts: the request, the
	// policy's clock reading, and on resume the grant and its reading.
	allowed   bool
	req       ToolRequest
	policyAt  time.Time
	granted   *Approval
	resumedAt time.Time
	proposed  bool
	executed  bool
}

// ID is the step's id, which its tool call carries as ToolCall.StepID.
func (st *OpenStep) ID() string { return st.step.ID }

// Index is the step's position in the run, from zero.
func (st *OpenStep) Index() int { return st.step.Index }

// Model is the step's only handle to the model: every call is recorded,
// charged to this step, and held to the run's call, token, and cost
// limits before it is sent, and none is sent once the lease is lost. A
// limit refuses the call with ErrLimit and ends nothing by itself; pass
// the error to Fail to end the run with its reason. Model is nil when the
// gate has no GateConfig.Model, and for the approved step Attach returns,
// whose decision was made before.
func (st *OpenStep) Model() ModelCaller { return st.mc }

// Propose records a decision verbatim, as the Driver records its agent's,
// validates it, and acts on it up to, and not including, running a tool.
// A complete or fail decision ends the run (Ended). An invalid decision,
// or a tool call the policy denies, ends the step with a failure
// observation, which counts toward the consecutive failure limit, and the
// session continues (Denied). A tool call the policy aborts ends the run
// (Ended), and one that requires approval pauses it on a hash-bound
// approval (Pending). A tool call the policy allows leaves the step open
// for Execute (Allowed); nothing is written for it until Execute, so a
// step abandoned there is still deciding, and Attach marks it interrupted
// without running it.
func (st *OpenStep) Propose(ctx context.Context, decision Decision) (Verdict, error) {
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := st.usable(false); err != nil {
		return Verdict{}, err
	}
	var v Verdict
	if !s.l.ok() {
		// The Driver's loop does not ask: its agent decided inside the
		// lease it just checked. A caller may have taken any time.
		v = s.lose(ctx, s.l.err())
	} else {
		v = st.propose(ctx, decision)
	}
	if s.ended && s.err != nil {
		return Verdict{}, s.err
	}
	return v, nil
}

// Execute runs the request an Allowed verdict let through, with the tool's
// timeout, a panic observed as an error, and its observation recorded
// even when ctx is cancelled after the tool ran, in which case the
// cancellation is returned with it. The tool is not started once the
// lease is lost, and the write that starts it requires the lease
// unexpired. A terminal tool that succeeds completes the run, and one
// that returns ErrAbortRun fails it; Done then reports the session over
// and Run the run.
//
// The Allowed verdict is the policy's answer when it was given. For a
// step Propose allowed, the gap until Execute is covered by the session's
// lease and by no other limit, exactly as an agent's slow Decide is: the
// time limits are checked at Step. For the approved step Attach allowed,
// which holds no lease, Execute judges Limits.GrantTTL again on a fresh
// reading of GateConfig.Now: a grant that expired since is expired as
// Attach would have expired it, the run cancelled as approval_expired,
// nothing executed, and the session ended with an error that matches
// ErrApprovalExpired.
func (st *OpenStep) Execute(ctx context.Context) (Observation, error) {
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := st.usable(true); err != nil {
		return Observation{}, err
	}
	if err := st.grantExpired(ctx); err != nil {
		return Observation{}, err
	}
	obs := st.execute(ctx)
	if s.ended && s.err != nil {
		return obs, s.err
	}
	return obs, nil
}

// grantExpired ends the session when the grant of an approved step has
// passed its GrantTTL since Attach allowed it, expiring the grant and
// cancelling the run as Attach does, and returns why. The Driver executes
// the grant in the call that judged it, so only a gate's caller can wait,
// and only it reads the clock again here.
func (st *OpenStep) grantExpired(ctx context.Context) error {
	s, d, granted := st.s, st.s.d, st.granted
	ttl := st.run.Limits.GrantTTL
	if granted == nil || ttl <= 0 {
		return nil
	}
	now := d.now()
	if !now.After(granted.DecidedAt.Add(ttl)) {
		return nil
	}
	if err := expireApproval(ctx, d.store, d.observer, st.run, *granted, now); err != nil {
		// Another process executed or cancelled the run meanwhile.
		s.end(Run{}, err)
		return err
	}
	err := fmt.Errorf("%w: run %s: approval %s was granted at %s and its grant TTL of %s passed before Execute", ErrApprovalExpired,
		s.runID, granted.ID, granted.DecidedAt.Format(time.RFC3339), ttl)
	out, _ := d.store.GetRun(ctx, s.runID)
	s.end(out, err)
	return err
}

// Fail ends a step whose decision could not be made, because the model
// calls behind it, or whatever else decides, returned err: the run fails
// with the limit's reason for an ErrLimit from Model, as model_unavailable
// for ErrModelUnavailable, and as agent_error otherwise, exactly as the
// Driver ends a run whose agent returned err. A cancelled ctx instead
// leaves the step in flight, for Attach to mark interrupted, and returns
// the cancellation.
func (st *OpenStep) Fail(ctx context.Context, err error) (Verdict, error) {
	s := st.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if uerr := st.usable(false); uerr != nil {
		return Verdict{}, uerr
	}
	if err == nil {
		return Verdict{}, errors.New("agentrt: Fail needs the error the decision failed with")
	}
	st.fail(ctx, err)
	if s.err != nil {
		return Verdict{}, s.err
	}
	return s.ending(), nil
}

// usable reports why the step cannot take a call now: its session is
// over, the step is, or it is not at the phase the call needs.
func (st *OpenStep) usable(execute bool) error {
	s := st.s
	switch {
	case s.ended:
		return s.over
	case s.open != st:
		return fmt.Errorf("%w: run %s: step %d is over", ErrRunState, s.runID, st.step.Index)
	case execute && !st.allowed:
		return fmt.Errorf("%w: run %s: step %d has no allowed verdict to execute", ErrRunState, s.runID, st.step.Index)
	case !execute && st.proposed:
		return fmt.Errorf("%w: run %s: step %d already has a decision", ErrRunState, s.runID, st.step.Index)
	}
	return nil
}

// VerdictOutcome is what the gate made of a proposed step.
type VerdictOutcome string

const (
	// VerdictAllowed means the request may run: call Execute.
	VerdictAllowed VerdictOutcome = "allowed"
	// VerdictDenied means the step ended without running anything, with a
	// failure observation: the policy denied the request, or the decision
	// was invalid. The session continues with the next Step.
	VerdictDenied VerdictOutcome = "denied"
	// VerdictPending means the run is WAITING on an approval and the
	// session is over. Approve it, then Attach.
	VerdictPending VerdictOutcome = "pending"
	// VerdictEnded means the run reached a terminal status and the session
	// is over.
	VerdictEnded VerdictOutcome = "ended"
)

// Verdict is the gate's answer to a proposed step.
type Verdict struct {
	Outcome VerdictOutcome
	// Request applies to Allowed: exactly what Execute runs. It is a copy,
	// so changing it cannot change what runs.
	Request *ToolRequest
	// Observation applies to Denied.
	Observation *Observation
	// Approval applies to Pending: the approval the run waits on.
	Approval *Approval
	// Run applies to Pending and Ended.
	Run Run
}
