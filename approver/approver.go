// Package approver is the portable approval channel: one interface over
// what an operator surface must show and may decide, and the store-backed
// implementation of it. A chat bot, a browser front end, or a command line
// written over Approver shows an approval, decides it bound to the hash it
// showed, or cancels the run, and never touches the database directly.
// approver/webhook is the reference HTTP channel over it; cmd/agentrt is
// the terminal one, written before this package and the model for what a
// channel must show.
//
// Nothing here resumes a run: that needs the consumer's agent, tools, and
// policy, so it stays in the consumer's own process (agentrt.Driver.Resume).
// A granted approval waits there until it is resumed or, with
// Limits.GrantTTL, expires.
//
// Identity is a label. By is recorded exactly as given and never verified,
// as cmd/agentrt's -by is; the channel in front of this package is where a
// person is authenticated, and the database is the trust boundary: anyone
// who can write it can approve anything in it.
package approver

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/view"
)

// Pending is an approval as a channel presents it: the stored approval
// with its capability, presentation, and arguments bounded the way
// cmd/agentrt bounds them (view.BoundApproval), the policy's reason for
// it, and whether its expiry has passed. Hash is the hash of the stored
// approval, which a decision is bound to; the bounding changes what is
// shown, never what is hashed.
type Pending struct {
	agentrt.Approval
	// Reason is the policy's reason for asking, from the decision that
	// paused the run (agentrt.Store.ApprovalReason), bounded as an
	// argument string is. It may carry text the model chose.
	Reason string `json:"reason,omitempty"`
	// Expired reports that ExpiresAt has passed by the Local's clock at
	// the read. Nothing is written: the runtime expires the approval, and
	// cancels the run, when the approval is next decided.
	Expired bool `json:"expired,omitempty"`
}

// Decision is what a channel supplies to decide an approval. Hash is the
// hash of the approval as it was shown, and the decision is refused with
// agentrt.ErrApprovalChanged if the stored approval differs from it. An
// empty Hash differs from every stored hash, so Local refuses it with
// agentrt.ErrApprovalChanged, not ErrDecision, and nothing is decided;
// approver/webhook refuses a body without one before it reaches the
// Approver, as 400 bad_request. By is an identity label, recorded as
// given; an empty one is ErrDecision.
type Decision struct {
	RunID      string `json:"run_id"`
	ApprovalID string `json:"approval_id"`
	Hash       string `json:"hash"`
	By         string `json:"by"`
	Note       string `json:"note,omitempty"`
}

// Approver is what a channel needs. Every method addresses a run by id:
// there is no listing across runs, so a channel that holds one run's id
// learns nothing about another's. Errors are the runtime's own
// (agentrt.ErrNotFound, ErrRunState, ErrNotPending, ErrApprovalChanged,
// ErrApprovalHash), wrapped so errors.Is matches them, plus ErrExpired and
// ErrDecision from this package.
type Approver interface {
	// Pending returns at most limit of a run's pending approvals, in
	// creation order, when the run is waiting for approval; an empty list
	// for a run that is not waiting, since nothing on it can be decided;
	// and agentrt.ErrNotFound for a run that does not exist.
	Pending(ctx context.Context, runID string, limit int) ([]Pending, error)
	// Show reads one approval of a run, whatever its status, for
	// presenting; agentrt.ErrNotFound when the run has no such approval.
	Show(ctx context.Context, runID, approvalID string) (Pending, error)
	// Approve grants the approval. The run stays waiting until its owner
	// resumes it.
	Approve(ctx context.Context, d Decision) error
	// Reject refuses the approval, which ends the run as CANCELLED.
	Reject(ctx context.Context, d Decision) error
	// Cancel ends a run that is not terminal, waiting or not, without
	// deciding its approvals.
	Cancel(ctx context.Context, runID, by, note string) error
}

// RunReader reads the run a channel is about to decide or cancel, so it
// can show the run without opening the store itself. It is separate from
// Approver, which it does not change: a channel that needs it asserts it,
// or holds a *Local, which implements both.
type RunReader interface {
	// Run returns the run as a front end reads one it may not trust:
	// every text column capped at agentrt.MaxPageText
	// (agentrt.Store.GetRunCapped), with Goal, ReasonDetail, and Result
	// then bounded as an approval's fields are (view.BoundValue,
	// view.BoundRaw). agentrt.ErrNotFound for a run that does not exist.
	Run(ctx context.Context, runID string) (agentrt.Run, error)
}

// ErrExpired means the approval had passed its expiry when the decision
// arrived: the runtime expired it and cancelled the run as
// approval_expired. An error matching it also matches the runtime's
// agentrt.ErrApprovalExpired and agentrt.ErrNotPending, which an expired
// approval has always returned.
var ErrExpired = errors.New("approver: approval has expired")

// ErrDecision means a decision named no run, no approval, or no identity.
// A missing hash is the runtime's agentrt.ErrApprovalChanged instead.
var ErrDecision = errors.New("approver: decision is incomplete")

// Local is the Approver over a store this process can open: the runtime's
// own decisions without a driver (agentrt.Operator), which judge expiry by
// the wall clock unless Now is set, as cmd/agentrt uses them.
type Local struct {
	// Now, when set before the Local is used, is the clock that judges
	// expiry and stamps decisions, for a channel's tests; nil is the wall
	// clock. It is not for a consumer's run clock: a deterministic
	// Config.Now also stamps the run's own records, one reading per
	// recorded state change, so a decision stamped through the same clock
	// here takes a reading the run did not, and every later timestamp of
	// the run shifts. casework's replay recordings stopped matching when
	// it shared its scenario clock this way; a consumer leaves Now nil.
	Now   func() time.Time
	store *agentrt.Store
	obs   agentrt.Observer
	owned bool
}

// New is a Local over a store the caller opened and closes. obs, which may
// be nil, receives the runtime's events for each decision after commit.
func New(store *agentrt.Store, obs agentrt.Observer) *Local {
	return &Local{store: store, obs: obs}
}

// Open is New over the database at path, opened for writing through
// agentrt.OpenExisting, which never creates or migrates a database: the
// file must already carry this build's schema, as the consumer's own
// agentrt.OpenStore leaves it, and must not be a symbolic link. A missing
// file is an error matching fs.ErrNotExist; a database the runtime never
// opened is refused as "not an agent-runtime database"; one an older
// build wrote, or a newer build migrated, is agentrt.ErrSchemaVersion,
// and the older one opens once the consumer's own process, at this
// build, has opened it with agentrt.OpenStore. Close closes it.
func Open(path string, obs agentrt.Observer) (*Local, error) {
	store, err := agentrt.OpenExisting(path, false)
	if err != nil {
		return nil, err
	}
	return &Local{store: store, obs: obs, owned: true}, nil
}

// Close closes a store Open opened and does nothing for one New was given.
func (l *Local) Close() error {
	if !l.owned {
		return nil
	}
	return l.store.Close()
}

// Run implements RunReader.
func (l *Local) Run(ctx context.Context, runID string) (agentrt.Run, error) {
	run, err := l.store.GetRunCapped(ctx, runID)
	if err != nil {
		return agentrt.Run{}, err
	}
	run.Goal = view.BoundValue(run.Goal).(string)
	run.ReasonDetail = view.BoundValue(run.ReasonDetail).(string)
	run.Result = view.BoundRaw(run.Result)
	return run, nil
}

// Pending implements Approver through agentrt.Store.ListApprovalsPage, so
// no row is read whole: an argument object over agentrt.MaxPageText is
// read as its size, and DecodeError says so.
func (l *Local) Pending(ctx context.Context, runID string, limit int) ([]Pending, error) {
	run, err := l.store.GetRunCapped(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		return []Pending{}, nil
	}
	page, _, err := l.store.ListApprovalsPage(ctx, runID, agentrt.ApprovalPending, limit, 0)
	if err != nil {
		return nil, err
	}
	now := l.op().Now()
	out := make([]Pending, 0, len(page))
	for _, a := range page {
		p, err := l.present(ctx, a, now)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Show implements Approver. The row is read whole, as cmd/agentrt reads
// the one it decides, and bounded for showing.
func (l *Local) Show(ctx context.Context, runID, approvalID string) (Pending, error) {
	a, err := l.store.GetApproval(ctx, runID, approvalID)
	if err != nil {
		return Pending{}, err
	}
	return l.present(ctx, a, l.op().Now())
}

func (l *Local) present(ctx context.Context, a agentrt.Approval, now time.Time) (Pending, error) {
	// An id a page read capped finds no row, and is shown without one.
	reason, err := l.store.ApprovalReason(ctx, a.RunID, a.ID)
	if err != nil && !errors.Is(err, agentrt.ErrNotFound) {
		return Pending{}, err
	}
	return Pending{Approval: view.BoundApproval(a), Reason: view.BoundValue(reason).(string), Expired: !a.ExpiresAt.IsZero() && now.After(a.ExpiresAt)}, nil
}

// op is the runtime's driverless decisions on l's store and clock.
func (l *Local) op() agentrt.Operator {
	now := l.Now
	if now == nil {
		now = time.Now
	}
	return agentrt.Operator{Store: l.store, Observer: l.obs, Now: now}
}

// Approve implements Approver through agentrt.Operator.ApproveShown.
func (l *Local) Approve(ctx context.Context, d Decision) error {
	if err := d.check(); err != nil {
		return err
	}
	return expired(l.op().ApproveShown(ctx, d.RunID, d.ApprovalID, d.Hash, d.By, d.Note))
}

// Reject implements Approver through agentrt.Operator.RejectShown.
func (l *Local) Reject(ctx context.Context, d Decision) error {
	if err := d.check(); err != nil {
		return err
	}
	return expired(l.op().RejectShown(ctx, d.RunID, d.ApprovalID, d.Hash, d.By, d.Note))
}

// Cancel implements Approver through agentrt.Operator.Cancel.
func (l *Local) Cancel(ctx context.Context, runID, by, note string) error {
	if runID == "" || by == "" {
		return fmt.Errorf("%w: a run and an identity are required", ErrDecision)
	}
	return l.op().Cancel(ctx, runID, by, note)
}

func (d Decision) check() error {
	if d.RunID == "" || d.ApprovalID == "" || d.By == "" {
		return fmt.Errorf("%w: a run, an approval, and an identity are required", ErrDecision)
	}
	return nil
}

// expired names an expiry in this package's terms as well as the
// runtime's.
func expired(err error) error {
	if !errors.Is(err, agentrt.ErrApprovalExpired) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrExpired, err)
}
