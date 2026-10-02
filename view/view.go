// Package view builds read models over a store for operator surfaces: what
// runs exist, what a run did, and which approval is waiting. Everything here
// reads; nothing decides. The summaries are the shape cmd/agentrt prints and
// both consumers' own commands report.
package view

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// RunSummary is one run as an operator needs to see it. The model totals
// come from the run row itself, so a summary costs no extra query.
type RunSummary struct {
	ID           string                 `json:"id"`
	Goal         string                 `json:"goal"`
	Status       agentrt.RunStatus      `json:"status"`
	Reason       agentrt.TerminalReason `json:"reason,omitempty"`
	ReasonDetail string                 `json:"reason_detail,omitempty"`
	Steps        int                    `json:"steps"`
	CreatedAt    time.Time              `json:"created_at"`
	// FinishedAt uses omitzero, not omitempty: time.Time is a struct, and
	// omitempty never treats a struct as empty, so a zero time.Time would
	// otherwise be marshaled as its zero-value RFC 3339 rendering.
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// PendingApprovalID is set only for a run waiting for approval, which is
	// the only state in which an approval can still be decided.
	PendingApprovalID string         `json:"pending_approval_id,omitempty"`
	ModelCalls        int            `json:"model_calls,omitempty"`
	Usage             agentrt.Usage  `json:"usage,omitzero"`
	EstimatedCost     agentrt.Micros `json:"estimated_cost_micros,omitempty"`
}

// StepSummary is one step: what the agent asked for, what the policy said,
// and what came back.
type StepSummary struct {
	Index       int                     `json:"index"`
	ID          string                  `json:"id"`
	Tool        string                  `json:"tool,omitempty"`
	Decision    agentrt.DecisionKind    `json:"decision,omitempty"`
	Status      agentrt.StepStatus      `json:"status"`
	Policy      agentrt.PolicyOutcome   `json:"policy,omitempty"`
	Observation agentrt.ObservationKind `json:"observation,omitempty"`
	Summary     string                  `json:"summary,omitempty"`
}

// Runs summarizes every run, newest first, in two queries: the runs, and
// the pending approvals of every waiting run.
func Runs(ctx context.Context, store *agentrt.Store) ([]RunSummary, error) {
	runs, err := store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := store.PendingApprovalIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		out = append(out, summarize(r, pending[r.ID]))
	}
	return out, nil
}

// RunsPage summarizes at most limit runs, newest first, after skipping
// offset, and returns how many runs there are. Every read is bounded by
// the page (agentrt.Store.ListRunsPage, PendingApprovalIDsOf), so a
// front end can list a database it does not trust.
func RunsPage(ctx context.Context, store *agentrt.Store, limit, offset int) ([]RunSummary, int, error) {
	runs, total, err := store.ListRunsPage(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	var waiting []string
	for _, r := range runs {
		if r.Status == agentrt.StatusWaitingForApproval {
			waiting = append(waiting, r.ID)
		}
	}
	// Two ids per run are enough to tell exactly one from several.
	pending, err := store.PendingApprovalIDsOf(ctx, waiting, 2)
	if err != nil {
		return nil, 0, err
	}
	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		out = append(out, summarize(r, pending[r.ID]))
	}
	return out, total, nil
}

// Summary summarizes one run. It reads the run and at most two pending
// approval ids.
func Summary(ctx context.Context, store *agentrt.Store, runID string) (RunSummary, error) {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return RunSummary{}, err
	}
	return summarizeRun(ctx, store, run)
}

func summarizeRun(ctx context.Context, store *agentrt.Store, run agentrt.Run) (RunSummary, error) {
	if run.Status != agentrt.StatusWaitingForApproval {
		return summarize(run, nil), nil
	}
	pending, err := store.PendingApprovalIDsOf(ctx, []string{run.ID}, 2)
	if err != nil {
		return RunSummary{}, err
	}
	return summarize(run, pending[run.ID]), nil
}

// RunDetail is one run with its steps and every approval: what a command
// that shows a run prints.
type RunDetail struct {
	Run       RunSummary
	Steps     []StepSummary
	Approvals []agentrt.Approval
}

// Detail reads a run, its steps, and its approvals, each once.
func Detail(ctx context.Context, store *agentrt.Store, runID string) (RunDetail, error) {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	steps, err := Steps(ctx, store, runID)
	if err != nil {
		return RunDetail{}, err
	}
	approvals, err := store.ListApprovals(ctx, runID)
	if err != nil {
		return RunDetail{}, err
	}
	return RunDetail{Run: summarize(run, pendingIDs(approvals)), Steps: steps, Approvals: approvals}, nil
}

// RunDetailPage is one run read in pages: the run with its text capped, a page
// of its steps, a page of its pending approvals, and a page of all its
// approvals, each at most limit after skipping offset, with their totals.
// Waiting is the run's pending approval when it has exactly one, whatever
// the offset. Every read is bounded (the agentrt.Store page reads), so a
// front end can show a run of a database it does not trust.
type RunDetailPage struct {
	Run            RunSummary
	Steps          []StepSummary
	StepsTotal     int
	Pending        []agentrt.Approval
	PendingTotal   int
	Approvals      []agentrt.Approval
	ApprovalsTotal int
	Waiting        *agentrt.Approval
}

// DetailPage reads a RunDetailPage.
func DetailPage(ctx context.Context, store *agentrt.Store, runID string, limit, offset int) (RunDetailPage, error) {
	run, err := store.GetRunCapped(ctx, runID)
	if err != nil {
		return RunDetailPage{}, err
	}
	var p RunDetailPage
	if p.Run, err = summarizeRun(ctx, store, run); err != nil {
		return RunDetailPage{}, err
	}
	steps, total, err := store.ListStepsPage(ctx, runID, limit, offset)
	if err != nil {
		return RunDetailPage{}, err
	}
	p.Steps, p.StepsTotal = summarizeSteps(steps), total
	if p.Pending, p.PendingTotal, err = store.ListApprovalsPage(ctx, runID, agentrt.ApprovalPending, limit, offset); err != nil {
		return RunDetailPage{}, err
	}
	if p.Approvals, p.ApprovalsTotal, err = store.ListApprovalsPage(ctx, runID, "", limit, offset); err != nil {
		return RunDetailPage{}, err
	}
	if p.PendingTotal == 1 {
		one, _, err := store.ListApprovalsPage(ctx, runID, agentrt.ApprovalPending, 1, 0)
		if err != nil {
			return RunDetailPage{}, err
		}
		if len(one) == 1 {
			p.Waiting = &one[0]
		}
	}
	return p, nil
}

// summarize names the pending approval only for a waiting run with exactly
// one, the only case in which there is one to name.
func summarize(run agentrt.Run, pending []string) RunSummary {
	s := RunSummary{
		ID: run.ID, Goal: run.Goal, Status: run.Status, Reason: run.Reason, ReasonDetail: run.ReasonDetail,
		Steps: run.StepCount, CreatedAt: run.CreatedAt, FinishedAt: run.FinishedAt,
		ModelCalls: run.ModelCalls, Usage: run.Usage, EstimatedCost: run.EstimatedCost,
	}
	if run.Status == agentrt.StatusWaitingForApproval && len(pending) == 1 {
		s.PendingApprovalID = pending[0]
	}
	return s
}

func pendingIDs(approvals []agentrt.Approval) []string {
	var ids []string
	for _, a := range approvals {
		if a.Status == agentrt.ApprovalPending {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// Steps summarizes a run's steps in index order.
func Steps(ctx context.Context, store *agentrt.Store, runID string) ([]StepSummary, error) {
	steps, err := store.ListSteps(ctx, runID)
	if err != nil {
		return nil, err
	}
	return summarizeSteps(steps), nil
}

func summarizeSteps(steps []agentrt.Step) []StepSummary {
	out := make([]StepSummary, 0, len(steps))
	for _, st := range steps {
		s := StepSummary{Index: st.Index, ID: st.ID, Status: st.Status}
		if st.Decision != nil {
			s.Tool, s.Decision = st.Decision.Tool, st.Decision.Kind
		}
		if st.Policy != nil {
			s.Policy = st.Policy.Outcome
		}
		if st.Observation != nil {
			s.Observation, s.Summary = st.Observation.Kind, st.Observation.Summary
		}
		out = append(out, s)
	}
	return out
}

// Errors PendingApproval wraps, so a front end can match them and word its
// own message; the text carries the ids.
var (
	ErrNotPending        = errors.New("approval is not pending")
	ErrNoPendingApproval = errors.New("no pending approval")
	ErrAmbiguousApproval = errors.New("more than one pending approval")
)

// PendingApproval selects the approval an operator means. With an id it
// fetches that approval and requires it to be pending; without one the run
// must have exactly one pending approval, and the error names the candidates
// when it does not, because guessing which one to grant is not the
// runtime's decision to make.
func PendingApproval(ctx context.Context, store *agentrt.Store, runID, approvalID string) (agentrt.Approval, error) {
	if approvalID != "" {
		a, err := store.GetApproval(ctx, runID, approvalID)
		if err != nil {
			return agentrt.Approval{}, fmt.Errorf("approval %s of run %s: %w", approvalID, runID, err)
		}
		if a.Status != agentrt.ApprovalPending {
			return agentrt.Approval{}, fmt.Errorf("approval %s is %s: %w", a.ID, a.Status, ErrNotPending)
		}
		return a, nil
	}
	pending, total, err := pendingApprovals(ctx, store, runID)
	if err != nil {
		return agentrt.Approval{}, err
	}
	switch {
	case total == 1 && len(pending) == 1:
		return pending[0], nil
	case total == 0:
		return agentrt.Approval{}, fmt.Errorf("run %s: %w", runID, ErrNoPendingApproval)
	default:
		ids := make([]string, 0, len(pending))
		for _, a := range pending {
			ids = append(ids, a.ID)
		}
		if total > len(ids) {
			ids = append(ids, fmt.Sprintf("and %d more", total-len(ids)))
		}
		return agentrt.Approval{}, fmt.Errorf("run %s has %d pending approvals, name one of %s: %w", runID, total, strings.Join(ids, ", "), ErrAmbiguousApproval)
	}
}

// maxCandidates is how many pending approvals PendingApproval names when a
// run has more than one.
const maxCandidates = 20

// pendingApprovals reads a run's pending approvals: the one in full when
// there is exactly one, since that is what will be decided, and otherwise
// at most maxCandidates of them as a bounded page, with how many there are.
func pendingApprovals(ctx context.Context, store *agentrt.Store, runID string) ([]agentrt.Approval, int, error) {
	page, total, err := store.ListApprovalsPage(ctx, runID, agentrt.ApprovalPending, maxCandidates, 0)
	if err != nil || total != 1 || len(page) != 1 {
		return page, total, err
	}
	a, err := store.GetApproval(ctx, runID, page[0].ID)
	if err != nil {
		return nil, 0, err
	}
	return []agentrt.Approval{a}, 1, nil
}

// Errors decideError wraps around whatever agentrt.Approve, agentrt.Reject,
// and agentrt.Cancel report, so a caller such as cmd/agentrt can match the
// shape of a failure instead of parsing its text. The core functions return
// plain fmt.Errorf values wrapping its own exported sentinels
// (agentrt.ErrRunState, agentrt.ErrNotPending); these give the operator
// surface its own to match on.
var (
	// ErrRunNotWaiting means the run is not (or no longer) waiting for
	// approval: another decision, a resume, or an expiry already moved it
	// on.
	ErrRunNotWaiting = errors.New("run is not waiting for approval")
	// ErrApprovalDecided means the named approval was already approved,
	// rejected, or expired; it is not the caller's to decide again.
	ErrApprovalDecided = errors.New("approval is already decided")
	// ErrRunFinished means Cancel was asked to end a run that is already
	// terminal (COMPLETED, FAILED, or CANCELLED): distinct from
	// ErrRunNotWaiting, which is for a run that was never (or no longer)
	// waiting for approval, not one that finished.
	ErrRunFinished = errors.New("run is already finished")
	// ErrDatabaseLocked means another process holds the database open for
	// writing. It wraps the driver's own error, whose text carries the raw
	// SQLite code, so a front end can say something a person can act on.
	ErrDatabaseLocked = errors.New("database is locked by another consumer")
)

// Approve records an approval decision, wrapping the core's error so
// ErrRunNotWaiting, ErrApprovalDecided, and ErrDatabaseLocked can be
// matched with errors.Is.
func Approve(ctx context.Context, store *agentrt.Store, obs agentrt.Observer, runID, approvalID, by, note string) error {
	return wrapDecideErr(agentrt.Approve(ctx, store, obs, runID, approvalID, by, note))
}

// Reject records a rejection, wrapping errors the same way Approve does.
func Reject(ctx context.Context, store *agentrt.Store, obs agentrt.Observer, runID, approvalID, by, note string) error {
	return wrapDecideErr(agentrt.Reject(ctx, store, obs, runID, approvalID, by, note))
}

// ApproveShown is Approve for a front end that printed the approval before
// asking for a decision: shownHash must be the hash of the approval as
// printed (agentrt.Approval.Hash). If the stored approval no longer matches
// that hash, the decision is refused with agentrt.ErrApprovalChanged rather
// than applied to whatever the approval has since become. Wrapped the same
// way as Approve.
func ApproveShown(ctx context.Context, store *agentrt.Store, obs agentrt.Observer, runID, approvalID, shownHash, by, note string) error {
	return wrapDecideErr(agentrt.ApproveShown(ctx, store, obs, runID, approvalID, shownHash, by, note))
}

// RejectShown is ApproveShown for a rejection.
func RejectShown(ctx context.Context, store *agentrt.Store, obs agentrt.Observer, runID, approvalID, shownHash, by, note string) error {
	return wrapDecideErr(agentrt.RejectShown(ctx, store, obs, runID, approvalID, shownHash, by, note))
}

// Cancel ends a run, wrapping ErrDatabaseLocked the same way Approve does.
// Unlike Approve and Reject, the core's agentrt.ErrRunState here means the
// run is already terminal, not that it was never waiting for approval, so
// it is wrapped as ErrRunFinished instead of ErrRunNotWaiting.
func Cancel(ctx context.Context, store *agentrt.Store, obs agentrt.Observer, runID, by, note string) error {
	err := agentrt.Cancel(ctx, store, obs, runID, by, note)
	if err == nil {
		return nil
	}
	if isLocked(err) {
		return fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
	}
	if errors.Is(err, agentrt.ErrRunState) {
		return fmt.Errorf("%w: %v", ErrRunFinished, err)
	}
	return err
}

// wrapDecideErr wraps what Approve, Reject, ApproveShown, and RejectShown
// return so a front end matches ErrRunNotWaiting, ErrApprovalDecided, and
// ErrDatabaseLocked with errors.Is rather than searching the message for
// words like "already", which also appears in text that means something
// else (a run already terminal, which Cancel reports as ErrRunFinished,
// not ErrApprovalDecided).
func wrapDecideErr(err error) error {
	if err == nil {
		return nil
	}
	if isLocked(err) {
		return fmt.Errorf("%w: %v", ErrDatabaseLocked, err)
	}
	switch {
	case errors.Is(err, agentrt.ErrRunState):
		return fmt.Errorf("%w: %v", ErrRunNotWaiting, err)
	case errors.Is(err, agentrt.ErrNotPending):
		return fmt.Errorf("%w: %v", ErrApprovalDecided, err)
	}
	return err
}

// coder is what modernc.org/sqlite's *sqlite.Error implements; matching the
// interface rather than importing the driver keeps this package's
// dependency on the exact SQLite binding minimal.
type coder interface{ Code() int }

// SQLite result codes for a database another connection is writing to.
// https://www.sqlite.org/rescode.html
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

func isLocked(err error) bool {
	var c coder
	if errors.As(err, &c) {
		return c.Code() == sqliteBusy || c.Code() == sqliteLocked
	}
	return strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "SQLITE_LOCKED") || strings.Contains(err.Error(), "database is locked")
}

// MaxFieldBytes bounds a single string value embedded in an approval's
// capability, presentation, or request arguments: all three come from a
// model or a policy, not from this package, so an operator surface that
// prints or encodes one uses this same limit whether it is writing text or
// JSON. cmd/agentrt's approval prompt applies it per line; BoundApproval
// applies it to JSON output the same way, so JSON does not carry more than
// the text an operator already sees.
const MaxFieldBytes = 2000

// headTailBytes is how much of a value BoundValue truncates survives at
// each end, once it decides to truncate at all.
const headTailBytes = 200

// BoundValue returns v with every string longer than MaxFieldBytes,
// anywhere inside it, replaced by its head and tail and the number of bytes
// omitted between them. Maps and slices are walked recursively so the
// bound applies to a value nested inside an argument object, not only a
// bare string; anything else (numbers, bools, null) passes through
// unchanged.
func BoundValue(v any) any {
	switch t := v.(type) {
	case string:
		return boundString(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = BoundValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = BoundValue(vv)
		}
		return out
	default:
		return v
	}
}

func boundString(s string) string {
	if len(s) <= MaxFieldBytes {
		return s
	}
	head := headBytes(s, headTailBytes)
	tail := tailBytes(s, headTailBytes)
	omitted := len(s) - len(head) - len(tail)
	return fmt.Sprintf("%s…[%d bytes omitted]…%s", head, omitted, tail)
}

// headBytes and tailBytes cut s to at most n bytes from the start or the
// end respectively, moving inward to the nearest UTF-8 character boundary
// so truncation never splits a multi-byte rune.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// BoundRaw decodes raw as JSON, applies BoundValue, and re-encodes it. raw
// is returned unchanged if it is empty or does not round-trip through JSON,
// which only happens for a value that is already malformed for some other
// reason and is left for that error to surface elsewhere.
func BoundRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(BoundValue(v))
	if err != nil {
		return raw
	}
	return json.RawMessage(out)
}

// BoundApproval returns a with Capability, Presentation, and
// Request.Args passed through BoundRaw: an approval built from a model's
// tool call and a policy's presentation can carry a value of any size, so a
// command encoding it as JSON bounds it the same way its text output does,
// rather than a large or crafted database exhausting a consumer's memory or
// disk just because it asked for JSON instead of text.
func BoundApproval(a agentrt.Approval) agentrt.Approval {
	a.Capability = BoundRaw(a.Capability)
	a.Presentation = BoundRaw(a.Presentation)
	a.Request.Args = BoundRaw(a.Request.Args)
	return a
}
