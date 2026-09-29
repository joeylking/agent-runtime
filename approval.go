package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// approvalHash binds an approval to exactly what was asked and shown.
func approvalHash(kind string, capability, presentation json.RawMessage, req ToolRequest) string {
	return contentHash(toJSON(map[string]any{
		"kind": kind, "capability": orEmptyObject(capability), "presentation": orEmptyObject(presentation), "request": req,
	}))
}

// pause records a hash-bound approval for the step's request and parks
// the run, which must be in one of from. The step moves to
// awaiting_approval from whatever status it holds, and step.policy
// precedes approval.requested.
func (d *Driver) pause(ctx context.Context, c *runCache, runID string, step *Step, req ToolRequest, pd PolicyDecision, from []RunStatus, policyAt time.Time) (Run, error) {
	now := d.now()
	fromStep := step.Status
	step.Status, step.Policy = StepAwaitingApproval, &pd
	var out Run
	err := d.write(ctx, c, func(t *txn) error {
		r, err := t.requireRun(ctx, runID, from...)
		if err != nil {
			return err
		}
		a := Approval{ID: d.newID(), RunID: runID, StepID: step.ID, Kind: pd.Kind, Capability: orEmptyObject(pd.Capability), Presentation: orEmptyObject(pd.Presentation), Request: req, Status: ApprovalPending, CreatedAt: now}
		if r.Limits.ApprovalTTL > 0 {
			a.ExpiresAt = now.Add(r.Limits.ApprovalTTL)
		}
		a.Hash = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request)
		if err := t.updateStep(ctx, *step, fromStep); err != nil {
			return err
		}
		r.Status = StatusWaitingForApproval
		if err := t.transition(ctx, r, from...); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: runID, StepID: step.ID, At: policyAt, Type: EventStepPolicy, Payload: toJSON(step.Policy)}); err != nil {
			return err
		}
		if err := t.insertApproval(ctx, a); err != nil {
			return err
		}
		out = r
		return t.appendEvent(ctx, Event{RunID: runID, StepID: step.ID, At: now, Type: EventApprovalRequested, Payload: toJSON(map[string]any{
			"approval_id": a.ID, "kind": pd.Kind, "reason": pd.Reason, "capability": a.Capability, "presentation": a.Presentation, "hash": a.Hash,
		})})
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

// ErrApprovalHash means a stored approval no longer matches its own fields.
var ErrApprovalHash = errors.New("agentrt: approval hash does not match its recorded request")

// Approve marks a pending approval approved after recomputing its hash
// from the stored fields. The run stays WAITING until Resume. Expiry is
// judged by the driver's clock.
func (d *Driver) Approve(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, d.store, d.observer, d.now, runID, approvalID, by, note, ApprovalApproved)
}

// Reject marks a pending approval rejected and ends the run as CANCELLED.
func (d *Driver) Reject(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, d.store, d.observer, d.now, runID, approvalID, by, note, ApprovalRejected)
}

// Approve records an approval decision without a driver, for command-line
// front ends that decide approvals in a process that will not resume the
// run. The hash is recomputed from the stored fields first. Expiry is
// judged by the wall clock.
func Approve(ctx context.Context, store *Store, obs Observer, runID, approvalID, by, note string) error {
	return decide(ctx, store, obs, time.Now, runID, approvalID, by, note, ApprovalApproved)
}

// Reject records a rejection without a driver and ends the run as CANCELLED.
func Reject(ctx context.Context, store *Store, obs Observer, runID, approvalID, by, note string) error {
	return decide(ctx, store, obs, time.Now, runID, approvalID, by, note, ApprovalRejected)
}

// decide records an approval decision. A run that is not waiting fails
// with ErrRunState; an approval that is not pending, or has expired, with
// ErrNotPending.
func decide(ctx context.Context, store *Store, obs Observer, clock func() time.Time, runID, approvalID, by, note string, status ApprovalStatus) error {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusWaitingForApproval {
		return fmt.Errorf("%w: run %s is %s, not waiting for approval", ErrRunState, runID, run.Status)
	}
	a, err := store.GetApproval(ctx, runID, approvalID)
	if err != nil {
		return err
	}
	if a.Status != ApprovalPending {
		return fmt.Errorf("%w: approval %s is already %s", ErrNotPending, approvalID, a.Status)
	}
	if approvalHash(a.Kind, a.Capability, a.Presentation, a.Request) != a.Hash {
		return ErrApprovalHash
	}
	now := clock()
	if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
		if err := expireApproval(ctx, store, obs, run, a, now); err != nil {
			return err
		}
		return fmt.Errorf("%w: approval %s expired at %s", ErrNotPending, approvalID, a.ExpiresAt.Format(time.RFC3339))
	}
	a.Status, a.DecidedAt, a.DecidedBy, a.Note = status, now, by, note
	// Loaded before the transaction: the store has a single connection.
	steps, err := store.ListSteps(ctx, runID)
	if err != nil {
		return err
	}
	return store.tx(ctx, obs, func(t *txn) error {
		if err := t.requireStatus(ctx, runID, StatusWaitingForApproval); err != nil {
			return err
		}
		if err := t.decideApproval(ctx, a); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: runID, StepID: a.StepID, At: now, Type: EventApprovalDecided, Payload: toJSON(map[string]any{"approval_id": a.ID, "status": status, "by": by, "note": note})}); err != nil {
			return err
		}
		if status != ApprovalRejected {
			return nil
		}
		for _, st := range steps {
			if st.ID != a.StepID {
				continue
			}
			o := observation(ObservePolicyDenied, map[string]any{"approval_id": a.ID, "note": note}, "approval rejected: "+note)
			from := st.Status
			st.Status, st.Observation, st.FinishedAt = StepFailed, &o, now
			if err := t.updateStep(ctx, st, from); err != nil {
				return err
			}
		}
		run.Status, run.Reason, run.ReasonDetail, run.FinishedAt = StatusCancelled, ReasonApprovalRejected, note, now
		if err := t.transition(ctx, run, StatusWaitingForApproval); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: runID, At: now, Type: EventRunFinished, Payload: toJSON(map[string]any{"status": run.Status, "reason": run.Reason, "detail": note, "steps": run.StepCount})})
	})
}

// Cancel ends a run that is not terminal as CANCELLED with reason
// operator_cancelled. It is for a run the operator no longer wants: one
// waiting for an approval, one already approved but not yet resumed, or
// one left interrupted. A step still in flight is failed with an
// observation naming the cancellation, and pending approvals are left as
// they are. A loop still executing the run finds it cancelled at its next
// write and stops there; the runtime does not interrupt a tool already
// running.
func (d *Driver) Cancel(ctx context.Context, runID, by, note string) error {
	return cancelRun(ctx, d.store, d.observer, d.now, runID, by, note)
}

// Cancel is Driver.Cancel without a driver, for command-line tools that
// decide on runs they did not start. It stamps the wall clock.
func Cancel(ctx context.Context, store *Store, obs Observer, runID, by, note string) error {
	return cancelRun(ctx, store, obs, time.Now, runID, by, note)
}

var notTerminal = []RunStatus{StatusRunning, StatusWaitingForApproval, StatusInterrupted}

func cancelRun(ctx context.Context, store *Store, obs Observer, clock func() time.Time, runID, by, note string) error {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("%w: run %s is already %s", ErrRunState, runID, run.Status)
	}
	now := clock()
	o := observation(ObserveInterrupted, map[string]any{"by": by, "note": note}, "cancelled by operator: "+note)
	return store.tx(ctx, obs, func(t *txn) error {
		r, err := t.requireRun(ctx, runID, notTerminal...)
		if err != nil {
			return err
		}
		if _, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, observation_json=?, observation_hash=?, finished_at=? WHERE run_id=? AND status IN (?,?,?)`,
			StepFailed, marshalOpt(&o), o.ContentHash, formatTime(now), runID, StepDeciding, StepAwaitingApproval, StepExecuting); err != nil {
			return err
		}
		r.Status, r.Reason, r.ReasonDetail, r.FinishedAt = StatusCancelled, ReasonOperatorCancelled, note, now
		if err := t.transition(ctx, r, notTerminal...); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: runID, At: now, Type: EventRunFinished, Payload: toJSON(map[string]any{"status": r.Status, "reason": r.Reason, "detail": note, "by": by, "steps": r.StepCount})})
	})
}

// expireApproval marks a pending approval expired and cancels the run.
func expireApproval(ctx context.Context, store *Store, obs Observer, run Run, a Approval, now time.Time) error {
	a.Status, a.DecidedAt, a.Note = ApprovalExpired, now, "expired"
	run.Status, run.Reason, run.ReasonDetail, run.FinishedAt = StatusCancelled, ReasonApprovalExpired, "approval "+a.ID+" expired", now
	return store.tx(ctx, obs, func(t *txn) error {
		if err := t.decideApproval(ctx, a); err != nil {
			return err
		}
		if err := t.transition(ctx, run, StatusWaitingForApproval); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: run.ID, StepID: a.StepID, At: now, Type: EventApprovalDecided, Payload: toJSON(map[string]any{"approval_id": a.ID, "status": ApprovalExpired})}); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunFinished, Payload: toJSON(map[string]any{"status": run.Status, "reason": run.Reason, "detail": run.ReasonDetail, "steps": run.StepCount})})
	})
}
