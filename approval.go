package agentrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// approvalHash binds an approval to exactly what was asked and shown. A
// value the encoder or the canonical decoder refuses is an error, never a
// stand-in hash; for everything else it is contentHash of the same bytes
// it always hashed.
func approvalHash(kind string, capability, presentation json.RawMessage, req ToolRequest) (string, error) {
	b, err := toJSON(map[string]any{
		"kind": kind, "capability": orEmptyObject(capability), "presentation": orEmptyObject(presentation), "request": req,
	})
	if err != nil {
		return "", err
	}
	c, err := canonicalJSON(b)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errEncode, err)
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// verifyHash reports whether a stored approval still matches its hash. An
// approval whose fields no longer hash at all, as one stored with the hash
// of an encoding error, does not.
func verifyHash(a Approval) error {
	if a.DecodeError != "" {
		return fmt.Errorf("%w: %s", ErrApprovalHash, a.DecodeError)
	}
	if h, err := approvalHash(a.Kind, a.Capability, a.Presentation, a.Request); err != nil || h != a.Hash {
		return ErrApprovalHash
	}
	return nil
}

// pause records a hash-bound approval for the step's request and parks
// the run, which must be in one of from, returning the run and the
// approval. The step moves to awaiting_approval from whatever status it
// holds, and step.policy precedes approval.requested.
func (d *Driver) pause(ctx context.Context, c *runCache, runID string, step *Step, req ToolRequest, pd PolicyDecision, from []RunStatus, policyAt time.Time) (Run, Approval, error) {
	now := d.now()
	fromStep := step.Status
	step.Status, step.Policy = StepAwaitingApproval, &pd
	var out Run
	var paused Approval
	err := d.write(ctx, c, func(t *txn) error {
		r, err := t.requireRun(ctx, runID, from...)
		if err != nil {
			return err
		}
		a := Approval{ID: d.newID(), RunID: runID, StepID: step.ID, Kind: pd.Kind, Capability: orEmptyObject(pd.Capability), Presentation: orEmptyObject(pd.Presentation), Request: req, Status: ApprovalPending, CreatedAt: now}
		if r.Limits.ApprovalTTL > 0 {
			a.ExpiresAt = now.Add(r.Limits.ApprovalTTL)
		}
		if a.Hash, err = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request); err != nil {
			return err
		}
		if err := t.updateStep(ctx, *step, fromStep); err != nil {
			return err
		}
		r.Status = StatusWaitingForApproval
		if err := t.transition(ctx, r, from...); err != nil {
			return err
		}
		if err := t.emit(ctx, Event{RunID: runID, StepID: step.ID, At: policyAt, Type: EventStepPolicy}, step.Policy); err != nil {
			return err
		}
		if err := t.insertApproval(ctx, a); err != nil {
			return err
		}
		out, paused = r, a
		return t.emit(ctx, Event{RunID: runID, StepID: step.ID, At: now, Type: EventApprovalRequested}, map[string]any{
			"approval_id": a.ID, "kind": pd.Kind, "reason": pd.Reason, "capability": a.Capability, "presentation": a.Presentation, "hash": a.Hash,
		})
	})
	if err != nil {
		return Run{}, Approval{}, err
	}
	return out, paused, nil
}

// ErrApprovalHash means a stored approval no longer matches its own fields.
var ErrApprovalHash = errors.New("agentrt: approval hash does not match its recorded request")

// Approve marks a pending approval approved after recomputing its hash
// from the stored fields. The run stays WAITING until Resume. Expiry is
// judged by the driver's clock.
func (d *Driver) Approve(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, d.store, d.observer, d.now, runID, approvalID, "", by, note, ApprovalApproved)
}

// Reject marks a pending approval rejected and ends the run as CANCELLED.
func (d *Driver) Reject(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, d.store, d.observer, d.now, runID, approvalID, "", by, note, ApprovalRejected)
}

// Approve records an approval decision without a driver, for command-line
// front ends that decide approvals in a process that will not resume the
// run. The hash is recomputed from the stored fields first. Expiry is
// judged by the wall clock.
func Approve(ctx context.Context, store *Store, obs Observer, runID, approvalID, by, note string) error {
	return decide(ctx, store, obs, time.Now, runID, approvalID, "", by, note, ApprovalApproved)
}

// Reject records a rejection without a driver and ends the run as CANCELLED.
func Reject(ctx context.Context, store *Store, obs Observer, runID, approvalID, by, note string) error {
	return decide(ctx, store, obs, time.Now, runID, approvalID, "", by, note, ApprovalRejected)
}

// ErrApprovalChanged means the stored approval is no longer the one the
// operator was shown.
var ErrApprovalChanged = errors.New("agentrt: approval is not the one that was shown")

// ApproveShown is Approve for a front end that displayed the approval
// first: shownHash is the hash of what the operator read, and the decision
// is refused with ErrApprovalChanged if the stored approval differs. The
// comparison is made in the transaction that records the decision, and the
// update is conditional on the hash, so a writer cannot change the row
// between the check and the decision.
func ApproveShown(ctx context.Context, store *Store, obs Observer, runID, approvalID, shownHash, by, note string) error {
	if shownHash == "" {
		return fmt.Errorf("%w: approval %s: no hash was shown", ErrApprovalChanged, approvalID)
	}
	return decide(ctx, store, obs, time.Now, runID, approvalID, shownHash, by, note, ApprovalApproved)
}

// RejectShown is Reject with the same binding as ApproveShown.
func RejectShown(ctx context.Context, store *Store, obs Observer, runID, approvalID, shownHash, by, note string) error {
	if shownHash == "" {
		return fmt.Errorf("%w: approval %s: no hash was shown", ErrApprovalChanged, approvalID)
	}
	return decide(ctx, store, obs, time.Now, runID, approvalID, shownHash, by, note, ApprovalRejected)
}

// ErrApprovalExpired means a decision found the approval expired: its
// ApprovalTTL had passed by the deciding clock, so the decision expired it
// and cancelled the run as approval_expired, or it had been expired
// before. It matches ErrNotPending with errors.Is, which an expired
// approval has always returned, and the message is unchanged: it names
// the approval and, for one the decision expired, the expiry time.
var ErrApprovalExpired error = notPending("agentrt: approval has expired")

// notPending is a sentinel that matches ErrNotPending.
type notPending string

func (e notPending) Error() string { return string(e) }
func (e notPending) Unwrap() error { return ErrNotPending }

// expiredError is the error of a decision on an expired approval: its
// message is the one ErrNotPending always carried, and it matches
// ErrApprovalExpired and, through it, ErrNotPending.
type expiredError string

func (e expiredError) Error() string { return string(e) }
func (e expiredError) Unwrap() error { return ErrApprovalExpired }

// Operator decides approvals and cancels runs without a driver, as the
// package-level Approve, Reject, ApproveShown, RejectShown, and Cancel do,
// stamping and judging expiry by Now. A channel's tests set Now to put an
// approval before or past its expiry; the zero Now is the wall clock, so
// Operator{Store: s, Observer: o}.Approve is Approve(ctx, s, o, ...).
type Operator struct {
	Store    *Store
	Observer Observer
	Now      func() time.Time
}

func (o Operator) clock() func() time.Time {
	if o.Now == nil {
		return time.Now
	}
	return o.Now
}

// Approve is the package-level Approve on o's clock.
func (o Operator) Approve(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, o.Store, o.Observer, o.clock(), runID, approvalID, "", by, note, ApprovalApproved)
}

// Reject is the package-level Reject on o's clock.
func (o Operator) Reject(ctx context.Context, runID, approvalID, by, note string) error {
	return decide(ctx, o.Store, o.Observer, o.clock(), runID, approvalID, "", by, note, ApprovalRejected)
}

// ApproveShown is the package-level ApproveShown on o's clock.
func (o Operator) ApproveShown(ctx context.Context, runID, approvalID, shownHash, by, note string) error {
	if shownHash == "" {
		return fmt.Errorf("%w: approval %s: no hash was shown", ErrApprovalChanged, approvalID)
	}
	return decide(ctx, o.Store, o.Observer, o.clock(), runID, approvalID, shownHash, by, note, ApprovalApproved)
}

// RejectShown is the package-level RejectShown on o's clock.
func (o Operator) RejectShown(ctx context.Context, runID, approvalID, shownHash, by, note string) error {
	if shownHash == "" {
		return fmt.Errorf("%w: approval %s: no hash was shown", ErrApprovalChanged, approvalID)
	}
	return decide(ctx, o.Store, o.Observer, o.clock(), runID, approvalID, shownHash, by, note, ApprovalRejected)
}

// Cancel is the package-level Cancel on o's clock.
func (o Operator) Cancel(ctx context.Context, runID, by, note string) error {
	return cancelRun(ctx, o.Store, o.Observer, o.clock(), runID, by, note)
}

// decide records an approval decision. A run that is not waiting fails
// with ErrRunState; an approval that is not pending with ErrNotPending, or
// with ErrApprovalExpired, which matches it, when it has expired; one whose
// fields no longer match its hash with ErrApprovalHash; and, when shown is
// set, one whose hash is not shown with ErrApprovalChanged. The checks are repeated on the row read inside the
// deciding transaction, which holds the write lock, and the update is
// conditional on the hash checked.
func decide(ctx context.Context, store *Store, obs Observer, clock func() time.Time, runID, approvalID, shown, by, note string, status ApprovalStatus) error {
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
	check := func(a Approval) error {
		if a.Status == ApprovalExpired {
			return expiredError(fmt.Sprintf("%v: approval %s is already %s", ErrNotPending, approvalID, a.Status))
		}
		if a.Status != ApprovalPending {
			return fmt.Errorf("%w: approval %s is already %s", ErrNotPending, approvalID, a.Status)
		}
		if shown != "" && a.Hash != shown {
			return fmt.Errorf("%w: approval %s", ErrApprovalChanged, approvalID)
		}
		return verifyHash(a)
	}
	if err := check(a); err != nil {
		return err
	}
	now := clock()
	if !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt) {
		if err := expireApproval(ctx, store, obs, run, a, now); err != nil {
			return err
		}
		return expiredError(fmt.Sprintf("%v: approval %s expired at %s", ErrNotPending, approvalID, a.ExpiresAt.Format(time.RFC3339)))
	}
	return store.tx(ctx, obs, func(t *txn) error {
		if err := t.requireStatus(ctx, runID, StatusWaitingForApproval); err != nil {
			return err
		}
		cur, err := t.getApproval(ctx, runID, approvalID)
		if err != nil {
			return err
		}
		if err := check(cur); err != nil {
			return err
		}
		cur.Status, cur.DecidedAt, cur.DecidedBy, cur.Note = status, now, by, note
		if err := t.decideApproval(ctx, cur, ApprovalPending, cur.Hash); err != nil {
			return err
		}
		if err := t.emit(ctx, Event{RunID: runID, StepID: cur.StepID, At: now, Type: EventApprovalDecided}, map[string]any{"approval_id": cur.ID, "status": status, "by": by, "note": note}); err != nil {
			return err
		}
		if status != ApprovalRejected {
			return nil
		}
		// Only the awaiting step's outcome columns are written: its
		// decision and policy stay exactly as stored.
		o := observation(ObservePolicyDenied, map[string]string{"approval_id": cur.ID, "note": note}, "approval rejected: "+note)
		obsJSON, err := marshalOpt(&o)
		if err != nil {
			return err
		}
		if _, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, observation_json=?, observation_hash=?, finished_at=? WHERE id=? AND run_id=?`,
			StepFailed, obsJSON, o.ContentHash, formatTime(now), cur.StepID, runID); err != nil {
			return err
		}
		r, err := t.loadRun(ctx, runID)
		if err != nil {
			return err
		}
		r.Status, r.Reason, r.ReasonDetail, r.FinishedAt = StatusCancelled, ReasonApprovalRejected, note, now
		if err := t.transition(ctx, r, StatusWaitingForApproval); err != nil {
			return err
		}
		return t.emit(ctx, Event{RunID: runID, At: now, Type: EventRunFinished}, map[string]any{"status": r.Status, "reason": r.Reason, "detail": note, "steps": r.StepCount})
	})
}

// Cancel ends a run that is not terminal as CANCELLED with reason
// operator_cancelled. It is for a run the operator no longer wants: one
// waiting for an approval, one already approved but not yet resumed, or
// one left interrupted. A step still in flight is failed with an
// observation naming the cancellation, and pending approvals are left as
// they are. A loop still executing the run finds it cancelled at its next
// write and stops there; the runtime does not interrupt a tool already
// running, and what such a tool returns is appended to the cancelled run
// as a late step.tool_finished event.
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
	o := observation(ObserveInterrupted, map[string]string{"by": by, "note": note}, "cancelled by operator: "+note)
	obsJSON, err := marshalOpt(&o)
	if err != nil {
		return err
	}
	return store.tx(ctx, obs, func(t *txn) error {
		r, err := t.requireRun(ctx, runID, notTerminal...)
		if err != nil {
			return err
		}
		if _, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, observation_json=?, observation_hash=?, finished_at=? WHERE run_id=? AND status IN (?,?,?)`,
			StepFailed, obsJSON, o.ContentHash, formatTime(now), runID, StepDeciding, StepAwaitingApproval, StepExecuting); err != nil {
			return err
		}
		r.Status, r.Reason, r.ReasonDetail, r.FinishedAt = StatusCancelled, ReasonOperatorCancelled, note, now
		if err := t.transition(ctx, r, notTerminal...); err != nil {
			return err
		}
		return t.emit(ctx, Event{RunID: runID, At: now, Type: EventRunFinished}, map[string]any{"status": r.Status, "reason": r.Reason, "detail": note, "by": by, "steps": r.StepCount})
	})
}

// expireApproval marks an approval expired, pending or a grant never
// resumed, and cancels the run.
func expireApproval(ctx context.Context, store *Store, obs Observer, run Run, a Approval, now time.Time) error {
	from := a.Status
	if from == ApprovalPending {
		a.DecidedAt, a.Note = now, "expired"
	}
	// A grant keeps who decided it and when; its expiry is the event.
	a.Status = ApprovalExpired
	run.Status, run.Reason, run.ReasonDetail, run.FinishedAt = StatusCancelled, ReasonApprovalExpired, "approval "+a.ID+" expired", now
	return store.tx(ctx, obs, func(t *txn) error {
		if err := t.decideApproval(ctx, a, from, a.Hash); err != nil {
			return err
		}
		if err := t.transition(ctx, run, StatusWaitingForApproval); err != nil {
			return err
		}
		if err := t.emit(ctx, Event{RunID: run.ID, StepID: a.StepID, At: now, Type: EventApprovalDecided}, map[string]any{"approval_id": a.ID, "status": ApprovalExpired}); err != nil {
			return err
		}
		return t.emit(ctx, Event{RunID: run.ID, At: now, Type: EventRunFinished}, map[string]any{"status": run.Status, "reason": run.Reason, "detail": run.ReasonDetail, "steps": run.StepCount})
	})
}
