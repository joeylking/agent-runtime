package agentrt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Resume continues a run. A WAITING run needs its awaiting step's latest
// approval to be approved: policy is re-evaluated while the run is still
// WAITING, and the recorded request is executed only if its arguments
// still match the tool's schema and policy allows it or asks for exactly
// the approval that was granted. The move to RUNNING is compare-and-set,
// so of two concurrent resumes one executes the request and the other
// returns ErrRunState. A RUNNING run is being executed by the owner of its
// lease: while that lease is live and not this driver's, or this driver
// is executing the run in another call, Resume returns ErrRunLeased and
// changes nothing. A RUNNING run whose lease has expired, or that has
// none, was interrupted: Resume takes the lease, the step in flight is
// marked interrupted with an observation, and the consumer's
// reconciliation decides what follows. Without one, a step interrupted
// while executing a tool that is not ReadOnly waits for an operator
// before it can run again; anything else continues.
func (d *Driver) Resume(ctx context.Context, runID string) (Run, error) {
	s, v := d.attach(ctx, runID)
	defer s.Close()
	if v.Outcome == VerdictAllowed {
		s.open.execute(ctx)
	}
	return d.loop(ctx, s)
}

// attach is what Resume does before its loop, and all a Gate's Attach
// does: it returns a session that holds the run, or one that has already
// ended with what Resume returns. An approved request the policy still
// allows is left open in the session for execute, with the run WAITING.
func (d *Driver) attach(ctx context.Context, runID string) (*Session, Verdict) {
	s := &Session{d: d, runID: runID, l: d.newLease(runID)}
	if !d.claim(runID) {
		s.end(Run{}, d.busy(ctx, runID))
		return s, Verdict{}
	}
	s.claimed = true
	run, err := d.store.GetRun(ctx, runID)
	if err != nil {
		s.end(Run{}, err)
		return s, Verdict{}
	}
	s.run = run
	switch run.Status {
	case StatusWaitingForApproval:
		return s, s.resumeApproved(ctx, run)
	case StatusRunning, StatusInterrupted:
		return s, s.resumeInterrupted(ctx, run)
	default:
		s.end(Run{}, fmt.Errorf("%w: run %s is %s and cannot be resumed", ErrRunState, runID, run.Status))
		return s, Verdict{}
	}
}

var waiting = []RunStatus{StatusWaitingForApproval}

func (s *Session) resumeApproved(ctx context.Context, run Run) Verdict {
	d := s.d
	steps, err := d.store.ListSteps(ctx, run.ID)
	if err != nil {
		return s.finish(Run{}, err)
	}
	approvals, err := d.store.ListApprovals(ctx, run.ID)
	if err != nil {
		return s.finish(Run{}, err)
	}
	if derr := decodeErrors(steps, approvals); derr != nil {
		return s.finish(d.failInternal(ctx, run.ID, waiting, derr))
	}
	var step *Step
	for i := range steps {
		if steps[i].Status == StepAwaitingApproval {
			step = &steps[i]
		}
	}
	if step == nil {
		return s.finish(Run{}, fmt.Errorf("%w: run %s is waiting but has no awaiting step", ErrRunState, run.ID))
	}
	// Only the step's latest approval, in insertion order, counts: a step
	// paused again has a newer approval, and an older grant is spent.
	var granted *Approval
	for i := range approvals {
		if approvals[i].StepID == step.ID {
			granted = &approvals[i]
		}
	}
	if granted == nil {
		return s.finish(Run{}, fmt.Errorf("%w: run %s has no approval for step %s", ErrRunState, run.ID, step.ID))
	}
	if granted.Status == ApprovalPending && !granted.ExpiresAt.IsZero() && d.now().After(granted.ExpiresAt) {
		if err := expireApproval(ctx, d.store, d.observer, run, *granted, d.now()); err != nil {
			return s.finish(Run{}, err)
		}
		return s.finish(d.store.GetRun(ctx, run.ID))
	}
	if granted.Status != ApprovalApproved {
		return s.finish(Run{}, fmt.Errorf("%w: run %s: the latest approval for step %s is %s, not approved", ErrRunState, run.ID, step.ID, granted.Status))
	}
	if err := verifyHash(*granted); err != nil {
		return s.finish(Run{}, err)
	}
	req := granted.Request
	if _, ok := d.tools[req.Spec.Name]; !ok {
		return s.finish(Run{}, fmt.Errorf("agentrt: approved tool %q is not registered", req.Spec.Name))
	}
	req.Spec = d.tools[req.Spec.Name].Spec()
	resumedAt := d.now()
	if ttl := run.Limits.GrantTTL; ttl > 0 && resumedAt.After(granted.DecidedAt.Add(ttl)) {
		// A grant not acted on within GrantTTL of its decision is refused
		// as stale. ApprovalTTL does not reach here: it bounds only how
		// long an approval may stay pending.
		if err := expireApproval(ctx, d.store, d.observer, run, *granted, resumedAt); err != nil {
			return s.finish(Run{}, err)
		}
		return s.finish(d.store.GetRun(ctx, run.ID))
	}
	// The arguments are checked against the schema registered now, as the
	// loop checks a fresh decision, before policy sees them.
	if verr := d.schemas[req.Spec.Name].validate(req.Args); verr != nil {
		return s.resumeInvalid(ctx, run, step, granted, resumedAt, verr)
	}
	spec, err := d.recordedSpec(req.Spec)
	if err != nil {
		return s.finish(d.failInternal(ctx, run.ID, waiting, err))
	}
	prior := steps[:step.Index]
	// Policy is evaluated while the run is still WAITING, so the move to
	// RUNNING and what the policy decided commit together: a crash between
	// them cannot drop the approved request.
	view := RunView{Run: run, Steps: cloneSteps(prior), Approvals: cloneApprovals(approvals)}
	st := &OpenStep{s: s, step: *step, run: run, proposed: true, toolSpec: req.Spec, spec: spec, view: seenBy(view)}
	pd, perr := d.policy.Evaluate(ctx, req, view)
	s.open = st
	return st.apply(ctx, req, pd, perr, granted, resumedAt)
}

// resumeInvalid moves a WAITING run whose approved request no longer
// matches its tool's schema back to RUNNING and ends the step as an
// invalid decision, as the loop would have ended it; the agent decides
// again.
func (s *Session) resumeInvalid(ctx context.Context, run Run, step *Step, granted *Approval, resumedAt time.Time, verr error) Verdict {
	d := s.d
	obs := observation(ObserveInvalidDecision, map[string]string{"error": verr.Error()}, "invalid decision on resume: "+verr.Error())
	stepAt := d.now()
	at := d.wall()
	if err := d.write(ctx, nil, func(t *txn) error {
		if err := d.enter(ctx, t, run, granted, resumedAt); err != nil {
			return err
		}
		return d.endStepTx(ctx, t, step, StepFailed, &obs, obs.Summary, stepAt)
	}); err != nil {
		return s.finish(Run{}, err)
	}
	s.l.start(at)
	return Verdict{Outcome: VerdictDenied, Observation: &obs}
}

var resumable = []RunStatus{StatusRunning, StatusInterrupted}

func (s *Session) resumeInterrupted(ctx context.Context, run Run) Verdict {
	d := s.d
	// A live lease is refused before anything is read from the clock.
	owner, until, err := d.store.lease(ctx, run.ID)
	if err != nil {
		return s.finish(Run{}, err)
	}
	if owner != "" && owner != d.terms.owner && until.After(d.wall()) {
		return s.finish(Run{}, ErrRunLeased{RunID: run.ID, Owner: owner, ExpiresAt: until})
	}
	steps, err := d.store.ListSteps(ctx, run.ID)
	if err != nil {
		return s.finish(Run{}, err)
	}
	approvals, err := d.store.ListApprovals(ctx, run.ID)
	if err != nil {
		return s.finish(Run{}, err)
	}
	now := d.now()
	// A damaged row is not rewritten: the run is taken and failed.
	damaged := decodeErrors(steps, approvals)
	var inFlight []*Step
	var executing *Step
	for i := range steps {
		if steps[i].Status == StepDeciding || steps[i].Status == StepExecuting {
			inFlight = append(inFlight, &steps[i])
		}
		if steps[i].Status == StepExecuting {
			executing = &steps[i]
		}
	}
	if executing == nil && len(steps) > 0 && interruptedExecuting(steps[len(steps)-1]) {
		// A resume that marked this step interrupted and stopped before
		// acting on it: the step is still the one whose outcome is unknown.
		executing = &steps[len(steps)-1]
	}
	// The lease is taken, and the steps in flight marked interrupted, in
	// one transaction: of two resumers one takes the run and the other
	// gets ErrRunLeased. Taking over a lease another owner let expire, or
	// this owner's own expired one, is recorded, because that is when an
	// operator will ask who ran what.
	at := d.wall()
	if err := d.write(ctx, nil, func(t *txn) error {
		prev, prevUntil, err := t.takeLease(ctx, run.ID, at, resumable...)
		if err != nil {
			return err
		}
		if prev != "" && (prev != d.terms.owner || !prevUntil.After(at)) {
			if err := t.emit(ctx, Event{RunID: run.ID, At: now, Type: EventLeaseTakenOver}, map[string]any{"previous_owner": prev, "previous_expires_at": formatTime(prevUntil), "owner": d.terms.owner}); err != nil {
				return err
			}
		}
		if damaged != nil {
			return nil
		}
		for _, st := range inFlight {
			obs := observation(ObserveInterrupted, map[string]string{"previous_status": string(st.Status)}, "step interrupted while "+string(st.Status))
			from := st.Status
			st.Status, st.Observation, st.FinishedAt = StepInterrupted, &obs, now
			if err := t.updateStep(ctx, *st, from); err != nil {
				return err
			}
			if err := t.emit(ctx, Event{RunID: run.ID, StepID: st.ID, At: now, Type: EventStepInterrupted}, map[string]any{"previous_status": obs.Content}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return s.finish(Run{}, err)
	}
	s.l.start(at)
	if damaged != nil {
		return s.finish(d.failInternal(ctx, run.ID, resumable, damaged))
	}
	if d.reconcile != nil {
		rec, err := d.reconcile(ctx, RunView{Run: run, Steps: cloneSteps(steps), Approvals: cloneApprovals(approvals)})
		if err != nil {
			return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()}))
		}
		switch rec.Outcome {
		case ReconcileContinue:
		case ReconcileCompleted:
			if len(bytes.TrimSpace(rec.Result)) > 0 {
				if cerr := checkJSON(rec.Result); cerr != nil {
					return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: result is not usable JSON: " + cerr.Error()}))
				}
			}
			return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusCompleted, reason: ReasonGoalCompleted, detail: "completed by reconciliation: " + rec.Detail, result: rec.Result}))
		case ReconcileConflict:
			return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict, detail: rec.Detail}))
		case ReconcileWaiting:
			return s.reconcilePause(ctx, run, executing, rec, now)
		default:
			return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: unknown outcome %q", rec.Outcome)}))
		}
	} else if executing != nil {
		if spec, ok := d.specOf(executing); !ok || spec.SideEffect != ReadOnly {
			// Without reconciliation nobody knows whether the side effect
			// happened, and the agent would ask for it again in a new
			// step. It is re-run only if an operator approves.
			return s.interruptedPause(ctx, run, executing, now)
		}
	}
	run.Status = StatusRunning
	if err := d.write(ctx, nil, func(t *txn) error {
		if err := t.transition(ctx, run, resumable...); err != nil {
			return err
		}
		return t.emit(ctx, Event{RunID: run.ID, At: now, Type: EventRunResumed}, map[string]any{"after": "interruption"})
	}); err != nil {
		return s.finish(Run{}, err)
	}
	s.run = run
	return Verdict{}
}

// interruptedExecuting reports a step Resume marked interrupted while it
// was executing a tool.
func interruptedExecuting(st Step) bool {
	if st.Status != StepInterrupted || st.Observation == nil || st.Observation.Kind != ObserveInterrupted {
		return false
	}
	var prev struct {
		PreviousStatus StepStatus `json:"previous_status"`
	}
	return json.Unmarshal(st.Observation.Content, &prev) == nil && prev.PreviousStatus == StepExecuting
}

// specOf is the registered spec of the tool a step called.
func (d *Driver) specOf(st *Step) (ToolSpec, bool) {
	if st == nil || st.Decision == nil || st.Decision.Kind != DecideToolCall {
		return ToolSpec{}, false
	}
	t, ok := d.tools[st.Decision.Tool]
	if !ok {
		return ToolSpec{}, false
	}
	return t.Spec(), true
}

// pausable is the request of a step interrupted while executing, checked
// as the loop checks a fresh decision: a registered tool, arguments that
// were JSON, and arguments the tool's schema accepts now. An error says
// why the step cannot wait for an approval.
func (d *Driver) pausable(run Run, st *Step) (ToolRequest, error) {
	if st == nil {
		return ToolRequest{}, fmt.Errorf("no tool call was executing")
	}
	spec, ok := d.specOf(st)
	if !ok {
		return ToolRequest{}, fmt.Errorf("the interrupted step's tool is not registered")
	}
	if st.Decision.InvalidArgs != "" {
		return ToolRequest{}, fmt.Errorf("the interrupted step's arguments were not JSON")
	}
	if err := d.schemas[spec.Name].validate(st.Decision.Args); err != nil {
		return ToolRequest{}, err
	}
	return ToolRequest{RunID: run.ID, StepID: st.ID, Spec: spec, Args: orEmptyObject(st.Decision.Args)}, nil
}

// reconcilePause parks a reconciled run on the approval its consumer asked
// for: the tool call interrupted while executing returns to
// awaiting_approval through the ordinary pause, so Approve and Resume
// treat it like any other. With no decision to pause on, or no executing
// tool call whose arguments still pass the schema, the run fails as a
// conflict rather than waiting on nothing; a step whose arguments fail the
// schema is ended as an invalid decision.
func (s *Session) reconcilePause(ctx context.Context, run Run, executing *Step, rec Reconciliation, now time.Time) Verdict {
	d := s.d
	conflict := func(detail string, e ending) Verdict {
		if rec.Detail != "" {
			detail += ": " + rec.Detail
		}
		e.status, e.reason, e.detail = StatusFailed, ReasonReconcileConflict, detail
		return s.finish(d.settle(ctx, nil, run.ID, resumable, e))
	}
	if rec.Pause == nil {
		return conflict("reconciliation asked to wait but gave no approval to wait for", ending{})
	}
	if rec.Pause.Outcome != RequireApproval {
		return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: waiting needs a require_approval decision, got %q", rec.Pause.Outcome)}))
	}
	if err := checkPolicy(*rec.Pause); err != nil {
		return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()}))
	}
	req, err := d.pausable(run, executing)
	if err != nil {
		e := ending{}
		if _, known := d.specOf(executing); known && executing.Decision.InvalidArgs == "" {
			obs := observation(ObserveInvalidDecision, map[string]string{"error": err.Error()}, "invalid decision on resume: "+err.Error())
			e.step, e.stepStatus, e.obs, e.stepDetail = executing, StepFailed, &obs, obs.Summary
		}
		return conflict("reconciliation asked to wait but no interrupted tool call can be paused ("+err.Error()+")", e)
	}
	spec, err := d.recordedSpec(req.Spec)
	if err != nil {
		return s.finish(d.failInternal(ctx, run.ID, resumable, err))
	}
	// The reconciliation's decision, not the policy's, is the step's now.
	executing.Observation, executing.FinishedAt, executing.PolicyID = nil, time.Time{}, ""
	return s.pause(d.pause(ctx, nil, run.ID, executing, req, *rec.Pause, resumable, now, spec, nil))
}

// InterruptedSideEffect is the approval kind a run pauses on when Resume,
// with no Config.Reconcile, finds a step interrupted while executing a
// tool that is not ReadOnly. Its presentation says the tool started and
// its outcome is unknown; approving runs it again, rejecting ends the run.
const InterruptedSideEffect = "interrupted_side_effect"

// interruptedPause parks a run whose interrupted side effect nobody can
// reconcile on an operator's decision, through the ordinary pause. A step
// that cannot wait, because its tool is gone or its arguments no longer
// pass the schema, ends the run as a conflict instead: it is never left
// for the agent to ask for again.
func (s *Session) interruptedPause(ctx context.Context, run Run, st *Step, now time.Time) Verdict {
	d := s.d
	req, err := d.pausable(run, st)
	if err != nil {
		return s.finish(d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict,
			detail: fmt.Sprintf("step %d was interrupted while executing a tool call whose outcome is unknown, and it cannot wait for an operator: %v", st.Index, err)}))
	}
	capability, err := toJSON(map[string]any{"tool": req.Spec.Name, "args": req.Args})
	if err != nil {
		return s.finish(d.failInternal(ctx, run.ID, resumable, err))
	}
	presentation, err := toJSON(map[string]any{
		"tool": req.Spec.Name, "args": req.Args, "side_effect": req.Spec.SideEffect,
		"notice": "This tool call started, and the process stopped before its outcome was recorded, so whether it took effect is unknown. Approving runs it again with these arguments; rejecting ends the run.",
	})
	if err != nil {
		return s.finish(d.failInternal(ctx, run.ID, resumable, err))
	}
	spec, err := d.recordedSpec(req.Spec)
	if err != nil {
		return s.finish(d.failInternal(ctx, run.ID, resumable, err))
	}
	pd := PolicyDecision{Outcome: RequireApproval, Reason: "interrupted while executing; outcome unknown", Kind: InterruptedSideEffect, Capability: capability, Presentation: presentation}
	st.Observation, st.FinishedAt, st.PolicyID = nil, time.Time{}, ""
	return s.pause(d.pause(ctx, nil, run.ID, st, req, pd, resumable, now, spec, nil))
}
