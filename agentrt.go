// Package agentrt is a small runtime for executing tool-using agents under
// deterministic control. The runtime owns the step loop, argument validation,
// policy evaluation, limits, persistence, and the audit event log. The agent
// owns only the decision of what to do next.
//
// Milestone 0A implements one scripted run: decide, validate, evaluate policy,
// execute, observe, persist, repeat, and stop on a step limit. Approvals,
// resumption, model accounting, loop detection, and recovery are documented in
// docs/status.md and are not part of this package yet.
package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RunStatus is the lifecycle state of a run.
type RunStatus string

const (
	StatusCreated            RunStatus = "CREATED"
	StatusRunning            RunStatus = "RUNNING"
	StatusWaitingForApproval RunStatus = "WAITING_FOR_APPROVAL"
	// StatusInterrupted marks a run found RUNNING with in-flight work by a
	// process that did not start it; Resume reconciles and continues it.
	StatusInterrupted RunStatus = "INTERRUPTED"
	StatusCompleted   RunStatus = "COMPLETED"
	StatusFailed      RunStatus = "FAILED"
	StatusCancelled   RunStatus = "CANCELLED"
)

// Terminal reports whether no further steps can occur for a run in this status.
func (s RunStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// TerminalReason explains why a run reached a terminal status. Reasons are
// generic to the runtime; consumers record their own outcome codes separately.
type TerminalReason string

const (
	ReasonGoalCompleted        TerminalReason = "goal_completed"
	ReasonGoalFailed           TerminalReason = "goal_failed"
	ReasonLimitSteps           TerminalReason = "limit_steps"
	ReasonRepeatedToolFailures TerminalReason = "repeated_tool_failures"
	ReasonLoopDetected         TerminalReason = "loop_detected"
	ReasonPolicyAbort          TerminalReason = "policy_abort"
	ReasonToolAbort            TerminalReason = "tool_abort"
	ReasonApprovalRejected     TerminalReason = "approval_rejected"
	ReasonAgentError           TerminalReason = "agent_error"
	ReasonInternalError        TerminalReason = "internal_error"
)

// Limits are the caps the driver enforces before each step. Only the limits
// implemented in this milestone are present.
type Limits struct {
	// MaxSteps is the maximum number of steps a run may start.
	MaxSteps int `json:"max_steps"`
	// MaxConsecutiveToolFailures ends the run when this many consecutive steps
	// produced a failure observation (tool error, policy denial, or an invalid
	// decision).
	MaxConsecutiveToolFailures int `json:"max_consecutive_tool_failures"`
	// LoopThreshold ends the run when the same tool call with the same
	// arguments has produced the same observation this many times in a row.
	// Repeating a call whose result changed is progress and is allowed.
	LoopThreshold int `json:"loop_threshold"`
}

// DefaultLimits returns conservative defaults.
func DefaultLimits() Limits {
	return Limits{MaxSteps: 50, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}
}

func (l Limits) validate() error {
	if l.MaxSteps <= 0 {
		return errors.New("limits: max_steps must be positive")
	}
	if l.MaxConsecutiveToolFailures <= 0 {
		return errors.New("limits: max_consecutive_tool_failures must be positive")
	}
	if l.LoopThreshold <= 0 {
		return errors.New("limits: loop_threshold must be positive")
	}
	return nil
}

// Run is the persisted state of one execution.
type Run struct {
	ID           string
	Goal         string
	Status       RunStatus
	Reason       TerminalReason
	ReasonDetail string
	Limits       Limits
	// StepCount is the number of steps started, including the current one.
	StepCount  int
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	// Result is the payload of a Complete decision, if any.
	Result json.RawMessage
}

// StepStatus is the state of one step.
type StepStatus string

const (
	StepDeciding         StepStatus = "deciding"
	StepAwaitingApproval StepStatus = "awaiting_approval"
	StepExecuting        StepStatus = "executing"
	StepDone             StepStatus = "done"
	StepFailed           StepStatus = "failed"
	StepInterrupted      StepStatus = "interrupted"
)

// DecisionKind classifies what the agent asked for.
type DecisionKind string

const (
	DecideToolCall DecisionKind = "tool_call"
	DecideComplete DecisionKind = "complete"
	DecideFail     DecisionKind = "fail"
)

// Decision is the agent's output for one step. It is recorded verbatim before
// it is validated, so the audit log shows what the agent asked for even when
// the request was invalid.
type Decision struct {
	Kind DecisionKind `json:"kind"`
	// Tool and Args apply to tool_call.
	Tool string          `json:"tool,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
	// Reason is the agent's stated rationale, recorded as data.
	Reason string `json:"reason,omitempty"`
	// Result applies to complete.
	Result json.RawMessage `json:"result,omitempty"`
	// Message applies to fail.
	Message string `json:"message,omitempty"`
}

// ObservationKind classifies what the agent sees after a step.
type ObservationKind string

const (
	ObserveToolResult      ObservationKind = "tool_result"
	ObserveToolError       ObservationKind = "tool_error"
	ObservePolicyDenied    ObservationKind = "policy_denied"
	ObserveInvalidDecision ObservationKind = "invalid_decision"
	ObserveInterrupted     ObservationKind = "interrupted"
)

// Observation is what a step produced for the agent to consider next.
type Observation struct {
	Kind    ObservationKind `json:"kind"`
	Content json.RawMessage `json:"content,omitempty"`
	Summary string          `json:"summary,omitempty"`
	// ContentHash is the SHA-256 of the canonical JSON content. It is recorded
	// now so that later no-progress detection can key on it.
	ContentHash string `json:"content_hash,omitempty"`
}

// Failure reports whether the observation counts toward the consecutive
// failure limit.
func (o Observation) Failure() bool {
	return o.Kind != ObserveToolResult
}

// Step is one iteration of the loop: a decision and what it caused.
type Step struct {
	ID          string
	RunID       string
	Index       int
	Status      StepStatus
	Decision    *Decision
	Policy      *PolicyDecision
	Observation *Observation
	StartedAt   time.Time
	FinishedAt  time.Time
}

// SideEffect classifies what a tool may do. Policy is evaluated on it.
type SideEffect string

const (
	ReadOnly       SideEffect = "read_only"
	LocalMutation  SideEffect = "local_mutation"
	RemoteMutation SideEffect = "remote_mutation"
	Destructive    SideEffect = "destructive"
)

func (s SideEffect) valid() bool {
	switch s {
	case ReadOnly, LocalMutation, RemoteMutation, Destructive:
		return true
	}
	return false
}

// ToolSpec describes a tool to the runtime and to the agent.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	SideEffect  SideEffect      `json:"side_effect"`
	// Timeout bounds a single call. It is required.
	Timeout time.Duration `json:"timeout"`
	// Terminal tools complete the run when they succeed; their result
	// becomes the run result and no further decision is requested.
	Terminal bool `json:"terminal,omitempty"`
}

// ErrAbortRun may be returned by a tool to end the run as FAILED with
// reason tool_abort. It is for conditions that make continuing pointless,
// such as a frozen proposal found invalid at publication.
type ErrAbortRun struct {
	Detail string
}

func (e ErrAbortRun) Error() string { return "abort run: " + e.Detail }

// ToolCall carries the driver-supplied execution identity into a tool.
type ToolCall struct {
	RunID  string
	StepID string
	Args   json.RawMessage
}

// ToolResult is what a tool returns on success.
type ToolResult struct {
	Content json.RawMessage
	Summary string
}

// Tool is a capability the agent may request. The runtime validates arguments
// against Spec().InputSchema and evaluates policy before Call is invoked.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, call ToolCall) (ToolResult, error)
}

// PolicyOutcome is what the policy decided about a tool request.
type PolicyOutcome string

const (
	Allow           PolicyOutcome = "allow"
	RequireApproval PolicyOutcome = "require_approval"
	Deny            PolicyOutcome = "deny"
	Abort           PolicyOutcome = "abort"
)

// PolicyDecision is the policy's answer for one request.
type PolicyDecision struct {
	Outcome PolicyOutcome `json:"outcome"`
	Reason  string        `json:"reason,omitempty"`
	// Kind, Capability, and Presentation apply to require_approval. In this
	// milestone the run pauses and records them; the approval record and
	// resumption arrive with durable approvals.
	Kind         string          `json:"kind,omitempty"`
	Capability   json.RawMessage `json:"capability,omitempty"`
	Presentation json.RawMessage `json:"presentation,omitempty"`
}

// ToolRequest is a schema-validated request handed to the policy.
type ToolRequest struct {
	RunID  string          `json:"run_id"`
	StepID string          `json:"step_id"`
	Spec   ToolSpec        `json:"spec"`
	Args   json.RawMessage `json:"args"`
}

// RunView is the read-only view a policy may consult.
type RunView struct {
	Run       Run
	Steps     []Step
	Approvals []Approval
}

// ApprovalStatus is the state of an approval.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

// Approval is a durable, hash-bound request for a human decision. Its hash
// covers the kind, capability, presentation, and the exact tool request, so
// a changed request cannot inherit an old approval. On resume the runtime
// executes the recorded request and nothing else.
type Approval struct {
	ID           string          `json:"id"`
	RunID        string          `json:"run_id"`
	StepID       string          `json:"step_id"`
	Kind         string          `json:"kind"`
	Capability   json.RawMessage `json:"capability"`
	Presentation json.RawMessage `json:"presentation"`
	Request      ToolRequest     `json:"request"`
	Hash         string          `json:"hash"`
	Status       ApprovalStatus  `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	DecidedAt    time.Time       `json:"decided_at,omitempty"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	Note         string          `json:"note,omitempty"`
}

// Policy decides whether a validated tool request may execute. It is evaluated
// by the runtime and the agent has no influence over it.
type Policy interface {
	Evaluate(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error)
}

// StepInput is everything the agent receives when asked to decide. Steps are
// all prior steps in order; the agent is stateless between calls.
type StepInput struct {
	Run       Run
	Steps     []Step
	Approvals []Approval
	Tools     []ToolSpec
}

// Agent decides what happens next.
type Agent interface {
	Decide(ctx context.Context, in StepInput) (Decision, error)
}

// Event is one row of the append-only audit log.
type Event struct {
	Seq     int64
	RunID   string
	StepID  string
	At      time.Time
	Type    string
	Payload json.RawMessage
}

// Event types written by the driver.
const (
	EventRunCreated        = "run.created"
	EventRunStarted        = "run.started"
	EventRunFinished       = "run.finished"
	EventStepStarted       = "step.started"
	EventStepDecided       = "step.decided"
	EventStepPolicy        = "step.policy"
	EventStepToolStarted   = "step.tool_started"
	EventStepToolFinished  = "step.tool_finished"
	EventStepFailed        = "step.failed"
	EventApprovalRequested = "approval.requested"
	EventLimitExceeded     = "limit.exceeded"
	EventLoopDetected      = "loop.detected"
	EventApprovalDecided   = "approval.decided"
	EventRunResumed        = "run.resumed"
	EventRunInterrupted    = "run.interrupted"
	EventStepInterrupted   = "step.interrupted"
)

// Observer receives every event after it has been committed.
type Observer func(Event)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("agentrt: marshal: %v", err))
	}
	return b
}
