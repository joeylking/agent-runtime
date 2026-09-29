package agentrt

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// Resume continues a run. A WAITING run needs an approved approval for its
// awaiting step: policy is re-evaluated while the run is still WAITING,
// and the recorded request is executed only if policy allows it or asks
// for exactly the approval that was granted. The move to RUNNING is
// compare-and-set, so of two concurrent resumes one executes the request
// and the other returns ErrRunState. A RUNNING run is being executed by
// the owner of its lease: while that lease is live and not this driver's,
// Resume returns ErrRunLeased and changes nothing. A RUNNING run whose
// lease has expired, or that has none, was interrupted: Resume takes the
// lease, the step in flight is marked interrupted with an observation, the
// consumer's reconciliation runs, and the loop continues.
func (d *Driver) Resume(ctx context.Context, runID string) (Run, error) {
	run, err := d.store.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	switch run.Status {
	case StatusWaitingForApproval:
		return d.resumeApproved(ctx, run)
	case StatusRunning, StatusInterrupted:
		return d.resumeInterrupted(ctx, run)
	default:
		return Run{}, fmt.Errorf("%w: run %s is %s and cannot be resumed", ErrRunState, runID, run.Status)
	}
}

func (d *Driver) resumeApproved(ctx context.Context, run Run) (out Run, err error) {
	l := d.newLease(run.ID)
	defer func() { l.stop(out, err) }()
	steps, err := d.store.ListSteps(ctx, run.ID)
	if err != nil {
		return Run{}, err
	}
	approvals, err := d.store.ListApprovals(ctx, run.ID)
	if err != nil {
		return Run{}, err
	}
	var step *Step
	for i := range steps {
		if steps[i].Status == StepAwaitingApproval {
			step = &steps[i]
		}
	}
	if step == nil {
		return Run{}, fmt.Errorf("%w: run %s is waiting but has no awaiting step", ErrRunState, run.ID)
	}
	// Approvals are in insertion order, so the last approved one for the
	// step is the most recent grant.
	var granted *Approval
	for i := range approvals {
		a := &approvals[i]
		if a.StepID == step.ID && a.Status == ApprovalPending && !a.ExpiresAt.IsZero() && d.now().After(a.ExpiresAt) {
			if err := expireApproval(ctx, d.store, d.observer, run, *a, d.now()); err != nil {
				return Run{}, err
			}
			return d.store.GetRun(ctx, run.ID)
		}
		if a.StepID == step.ID && a.Status == ApprovalApproved {
			granted = a
		}
	}
	if granted == nil {
		return Run{}, fmt.Errorf("%w: run %s has no approved approval for step %s", ErrRunState, run.ID, step.ID)
	}
	if approvalHash(granted.Kind, granted.Capability, granted.Presentation, granted.Request) != granted.Hash {
		return Run{}, ErrApprovalHash
	}
	req := granted.Request
	if _, ok := d.tools[req.Spec.Name]; !ok {
		return Run{}, fmt.Errorf("agentrt: approved tool %q is not registered", req.Spec.Name)
	}
	req.Spec = d.tools[req.Spec.Name].Spec()
	resumedAt := d.now()
	prior := steps[:step.Index]
	// Policy is evaluated while the run is still WAITING, so the move to
	// RUNNING and what the policy decided commit together: a crash between
	// them cannot drop the approved request.
	pd, perr := d.policy.Evaluate(ctx, req, RunView{Run: run, Steps: cloneSteps(prior), Approvals: cloneApprovals(approvals)})
	if fin, r, err := d.apply(ctx, nil, l, run, step, req, pd, perr, granted, resumedAt); err != nil || fin {
		return r, err
	}
	return d.loop(ctx, l)
}

var resumable = []RunStatus{StatusRunning, StatusInterrupted}

func (d *Driver) resumeInterrupted(ctx context.Context, run Run) (out Run, err error) {
	// A live lease is refused before anything is read from the clock.
	owner, until, err := d.store.lease(ctx, run.ID)
	if err != nil {
		return Run{}, err
	}
	if owner != "" && owner != d.terms.owner && until.After(d.wall()) {
		return Run{}, ErrRunLeased{RunID: run.ID, Owner: owner, ExpiresAt: until}
	}
	steps, err := d.store.ListSteps(ctx, run.ID)
	if err != nil {
		return Run{}, err
	}
	approvals, err := d.store.ListApprovals(ctx, run.ID)
	if err != nil {
		return Run{}, err
	}
	now := d.now()
	var inFlight []*Step
	for i := range steps {
		if steps[i].Status == StepDeciding || steps[i].Status == StepExecuting {
			inFlight = append(inFlight, &steps[i])
		}
	}
	l := d.newLease(run.ID)
	defer func() { l.stop(out, err) }()
	// The lease is taken, and the steps in flight marked interrupted, in
	// one transaction: of two resumers one takes the run and the other
	// gets ErrRunLeased. Taking over a lease another owner let expire is
	// recorded, because that is when an operator will ask who ran what.
	at := d.wall()
	if err := d.write(ctx, nil, func(t *txn) error {
		prev, prevUntil, err := t.takeLease(ctx, run.ID, at, resumable...)
		if err != nil {
			return err
		}
		if prev != "" && prev != d.terms.owner {
			if err := t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventLeaseTakenOver, Payload: toJSON(map[string]any{"previous_owner": prev, "previous_expires_at": formatTime(prevUntil), "owner": d.terms.owner})}); err != nil {
				return err
			}
		}
		for _, st := range inFlight {
			obs := observation(ObserveInterrupted, map[string]any{"previous_status": st.Status}, "step interrupted while "+string(st.Status))
			from := st.Status
			st.Status, st.Observation, st.FinishedAt = StepInterrupted, &obs, now
			if err := t.updateStep(ctx, *st, from); err != nil {
				return err
			}
			if err := t.appendEvent(ctx, Event{RunID: run.ID, StepID: st.ID, At: now, Type: EventStepInterrupted, Payload: toJSON(map[string]any{"previous_status": obs.Content})}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return Run{}, err
	}
	l.start(at)
	if d.reconcile != nil {
		rec, err := d.reconcile(ctx, RunView{Run: run, Steps: cloneSteps(steps), Approvals: cloneApprovals(approvals)})
		if err != nil {
			return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()})
		}
		switch rec.Outcome {
		case ReconcileContinue:
		case ReconcileCompleted:
			if len(bytes.TrimSpace(rec.Result)) > 0 {
				if cerr := checkJSON(rec.Result); cerr != nil {
					return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: result is not usable JSON: " + cerr.Error()})
				}
			}
			return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusCompleted, reason: ReasonGoalCompleted, detail: "completed by reconciliation: " + rec.Detail, result: rec.Result})
		case ReconcileConflict:
			return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict, detail: rec.Detail})
		case ReconcileWaiting:
			return d.reconcilePause(ctx, run, inFlight, rec, now)
		default:
			return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: unknown outcome %q", rec.Outcome)})
		}
	}
	run.Status = StatusRunning
	if err := d.write(ctx, nil, func(t *txn) error {
		if err := t.transition(ctx, run, resumable...); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunResumed, Payload: toJSON(map[string]any{"after": "interruption"})})
	}); err != nil {
		return Run{}, err
	}
	return d.loop(ctx, l)
}

// reconcilePause parks a reconciled run on the approval its consumer asked
// for: the last interrupted tool call returns to awaiting_approval through
// the ordinary pause, so Approve and Resume treat it like any other. With
// no decision to pause on, or no interrupted tool call to pause, the run
// fails as a conflict rather than waiting on nothing.
func (d *Driver) reconcilePause(ctx context.Context, run Run, interrupted []*Step, rec Reconciliation, now time.Time) (Run, error) {
	conflict := func(detail string) (Run, error) {
		if rec.Detail != "" {
			detail += ": " + rec.Detail
		}
		return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict, detail: detail})
	}
	if rec.Pause == nil {
		return conflict("reconciliation asked to wait but gave no approval to wait for")
	}
	if rec.Pause.Outcome != RequireApproval {
		return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: waiting needs a require_approval decision, got %q", rec.Pause.Outcome)})
	}
	if err := checkPolicy(*rec.Pause); err != nil {
		return d.settle(ctx, nil, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()})
	}
	var step *Step
	for _, st := range interrupted {
		if st.Decision != nil && st.Decision.Kind == DecideToolCall && d.tools[st.Decision.Tool] != nil {
			step = st
		}
	}
	if step == nil {
		return conflict("reconciliation asked to wait but no interrupted tool call can be paused")
	}
	req := ToolRequest{RunID: run.ID, StepID: step.ID, Spec: d.tools[step.Decision.Tool].Spec(), Args: orEmptyObject(step.Decision.Args)}
	step.Observation, step.FinishedAt = nil, time.Time{}
	return d.pause(ctx, nil, run.ID, step, req, *rec.Pause, resumable, now)
}
