package agentrt

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// startStep claims the next step index of a RUNNING run and records it.
func (d *Driver) startStep(ctx context.Context, c *runCache, run Run) (Step, error) {
	now := d.now()
	step := Step{ID: d.newID(), RunID: run.ID, Index: run.StepCount, Status: StepDeciding, StartedAt: now}
	err := d.write(ctx, c, func(t *txn) error {
		if err := t.claimStep(ctx, run.ID, step.Index); err != nil {
			return err
		}
		if err := t.insertStep(ctx, step); err != nil {
			return err
		}
		return t.emit(ctx, Event{RunID: run.ID, StepID: step.ID, At: now, Type: EventStepStarted}, map[string]any{"index": step.Index})
	})
	return step, err
}

// recordDecision records the step's decision and, for a tool call naming
// a registered tool, the tool's spec, stored by its hash if it is not yet.
func (d *Driver) recordDecision(ctx context.Context, c *runCache, step *Step, spec *specEntry) error {
	now := d.now()
	return d.write(ctx, c, func(t *txn) error {
		if err := t.requireStatus(ctx, step.RunID, StatusRunning); err != nil {
			return err
		}
		if err := t.updateStep(ctx, *step, StepDeciding); err != nil {
			return err
		}
		if spec != nil {
			if err := t.putSpec(ctx, spec); err != nil {
				return err
			}
		}
		return t.emit(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepDecided}, step.Decision)
	})
}

// endStep finishes a step of a RUNNING run without ending the run.
func (d *Driver) endStep(ctx context.Context, c *runCache, step *Step, status StepStatus, obs *Observation, detail string) error {
	now := d.now()
	return d.write(ctx, c, func(t *txn) error {
		if err := t.requireStatus(ctx, step.RunID, StatusRunning); err != nil {
			return err
		}
		return d.endStepTx(ctx, t, step, status, obs, detail, now)
	})
}

// endStepTx moves a step to done or failed inside a transaction: the step
// row, its active time, step.tool_finished when the tool had been started,
// and step.failed for a failure.
func (d *Driver) endStepTx(ctx context.Context, t *txn, step *Step, status StepStatus, obs *Observation, detail string, now time.Time) error {
	from := step.Status
	executed := from == StepExecuting
	step.Status, step.Observation, step.FinishedAt = status, obs, now
	if err := t.updateStep(ctx, *step, from); err != nil {
		return err
	}
	if err := t.addActiveTime(ctx, step.RunID, now.Sub(step.StartedAt)); err != nil {
		return err
	}
	if executed {
		payload := map[string]any{"tool": step.Decision.Tool, "duration_ms": now.Sub(step.StartedAt).Milliseconds()}
		if status == StepDone && obs != nil {
			payload["summary"], payload["content_hash"] = obs.Summary, obs.ContentHash
		} else {
			payload["error"] = detail
		}
		if err := t.emit(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolFinished}, payload); err != nil {
			return err
		}
	}
	if status != StepFailed {
		return nil
	}
	payload := map[string]any{"detail": detail}
	if obs != nil {
		payload["observation"] = obs.Kind
		payload["content_hash"] = obs.ContentHash
	}
	return t.emit(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepFailed}, payload)
}

// ending is how a run ends: its terminal status and reason, and the step
// that ends with it, written in the same transaction so a crash cannot
// leave the step finished and the run still running.
type ending struct {
	status RunStatus
	reason TerminalReason
	detail string
	result json.RawMessage
	limit  bool // also write limit.exceeded
	// pre appends events that precede the step's end, such as step.policy
	// or loop.detected.
	pre func(t *txn) error
	// step, when set, moves to stepStatus with obs and stepDetail.
	step       *Step
	stepStatus StepStatus
	obs        *Observation
	stepDetail string
}

// settle ends a run that is in one of from, with its step, in one
// transaction. The returned run is read inside that transaction, so its
// accounting and active time are current.
//
// The step ends at one clock reading and the run at the next, as when
// they were written separately.
func (d *Driver) settle(ctx context.Context, c *runCache, runID string, from []RunStatus, e ending) (Run, error) {
	var stepAt time.Time
	if e.step != nil {
		stepAt = d.now()
	}
	now := d.now()
	var out Run
	err := d.write(ctx, c, func(t *txn) error {
		if err := t.requireStatus(ctx, runID, from...); err != nil {
			return err
		}
		if e.pre != nil {
			if err := e.pre(t); err != nil {
				return err
			}
		}
		if e.step != nil {
			if err := d.endStepTx(ctx, t, e.step, e.stepStatus, e.obs, e.stepDetail, stepAt); err != nil {
				return err
			}
		}
		r, err := t.loadRun(ctx, runID)
		if err != nil {
			return err
		}
		r.Status, r.Reason, r.ReasonDetail, r.FinishedAt, r.Result = e.status, e.reason, e.detail, now, e.result
		if err := t.transition(ctx, r, from...); err != nil {
			return err
		}
		if e.limit {
			if err := t.emit(ctx, Event{RunID: runID, At: now, Type: EventLimitExceeded}, map[string]any{"reason": e.reason, "detail": e.detail}); err != nil {
				return err
			}
		}
		out = r
		return t.emit(ctx, Event{RunID: runID, At: now, Type: EventRunFinished}, map[string]any{"status": e.status, "reason": e.reason, "detail": e.detail, "steps": r.StepCount})
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

// failInternal ends a run in one of from as internal_error when a write
// the loop needed could not be made because a value would not encode. The
// step in flight is failed with an observation saying so, and nothing more
// is executed.
func (d *Driver) failInternal(ctx context.Context, runID string, from []RunStatus, cause error) (Run, error) {
	now := d.now()
	detail := "internal: " + cause.Error()
	o := observation(ObserveInvalidDecision, map[string]string{"error": detail}, detail)
	obsJSON, err := marshalOpt(&o)
	if err != nil {
		return Run{}, err
	}
	var out Run
	if err := d.write(ctx, nil, func(t *txn) error {
		r, err := t.requireRun(ctx, runID, from...)
		if err != nil {
			return err
		}
		if _, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, observation_json=?, observation_hash=?, finished_at=? WHERE run_id=? AND status IN (?,?,?)`,
			StepFailed, obsJSON, o.ContentHash, formatTime(now), runID, StepDeciding, StepAwaitingApproval, StepExecuting); err != nil {
			return err
		}
		r.Status, r.Reason, r.ReasonDetail, r.FinishedAt = StatusFailed, ReasonInternalError, detail, now
		if err := t.transition(ctx, r, from...); err != nil {
			return err
		}
		out = r
		return t.emit(ctx, Event{RunID: runID, At: now, Type: EventRunFinished}, map[string]any{"status": r.Status, "reason": r.Reason, "detail": detail, "steps": r.StepCount})
	}); err != nil {
		return Run{}, fmt.Errorf("%w (and failing the run: %w)", cause, err)
	}
	return out, nil
}

// recordLate appends what a tool returned to a run an operator cancelled
// while the tool ran, as a step.tool_finished event marked late. The run
// stays cancelled and the step row keeps what Cancel wrote; the event is
// the record of the side effect. It reports whether the run was such a
// run.
func (d *Driver) recordLate(ctx context.Context, step *Step, obs Observation) bool {
	now := d.now()
	late := false
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		r, err := t.loadRun(ctx, step.RunID)
		if err != nil {
			return err
		}
		if r.Status != StatusCancelled || r.Reason != ReasonOperatorCancelled {
			return nil
		}
		late = true
		payload := map[string]any{"tool": step.Decision.Tool, "duration_ms": now.Sub(step.StartedAt).Milliseconds(), "late": true, "observation": obs}
		if obs.Kind == ObserveToolResult {
			payload["summary"], payload["content_hash"] = obs.Summary, obs.ContentHash
		}
		return t.emit(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolFinished}, payload)
	})
	return late && err == nil
}
