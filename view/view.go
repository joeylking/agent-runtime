// Package view builds read models over a store for operator surfaces: what
// runs exist, what a run did, and which approval is waiting. Everything here
// reads; nothing decides. The summaries are the shape cmd/agentrt prints and
// both consumers' own commands report.
package view

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	FinishedAt   time.Time              `json:"finished_at,omitempty"`
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

// Runs summarizes every run, newest first.
func Runs(ctx context.Context, store *agentrt.Store) ([]RunSummary, error) {
	runs, err := store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		s, err := summarize(ctx, store, r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Summary summarizes one run.
func Summary(ctx context.Context, store *agentrt.Store, runID string) (RunSummary, error) {
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return RunSummary{}, err
	}
	return summarize(ctx, store, run)
}

func summarize(ctx context.Context, store *agentrt.Store, run agentrt.Run) (RunSummary, error) {
	s := RunSummary{
		ID: run.ID, Goal: run.Goal, Status: run.Status, Reason: run.Reason, ReasonDetail: run.ReasonDetail,
		Steps: run.StepCount, CreatedAt: run.CreatedAt, FinishedAt: run.FinishedAt,
		ModelCalls: run.ModelCalls, Usage: run.Usage, EstimatedCost: run.EstimatedCost,
	}
	if run.Status != agentrt.StatusWaitingForApproval {
		return s, nil
	}
	pending, err := pendingApprovals(ctx, store, run.ID)
	if err != nil {
		return RunSummary{}, err
	}
	if len(pending) == 1 {
		s.PendingApprovalID = pending[0].ID
	}
	return s, nil
}

// Steps summarizes a run's steps in index order.
func Steps(ctx context.Context, store *agentrt.Store, runID string) ([]StepSummary, error) {
	steps, err := store.ListSteps(ctx, runID)
	if err != nil {
		return nil, err
	}
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
	return out, nil
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
	pending, err := pendingApprovals(ctx, store, runID)
	if err != nil {
		return agentrt.Approval{}, err
	}
	switch len(pending) {
	case 1:
		return pending[0], nil
	case 0:
		return agentrt.Approval{}, fmt.Errorf("run %s: %w", runID, ErrNoPendingApproval)
	default:
		ids := make([]string, 0, len(pending))
		for _, a := range pending {
			ids = append(ids, a.ID)
		}
		return agentrt.Approval{}, fmt.Errorf("run %s has %d pending approvals, name one of %s: %w", runID, len(pending), strings.Join(ids, ", "), ErrAmbiguousApproval)
	}
}

func pendingApprovals(ctx context.Context, store *agentrt.Store, runID string) ([]agentrt.Approval, error) {
	all, err := store.ListApprovals(ctx, runID)
	if err != nil {
		return nil, err
	}
	var pending []agentrt.Approval
	for _, a := range all {
		if a.Status == agentrt.ApprovalPending {
			pending = append(pending, a)
		}
	}
	return pending, nil
}
