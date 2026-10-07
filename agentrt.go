// Package agentrt is a small runtime for executing tool-using agents under
// deterministic control. The runtime owns the step loop, argument validation,
// policy evaluation, limits, persistence, and the audit event log. The agent
// owns only the decision of what to do next.
//
// A Driver runs the loop: decide, validate, evaluate policy, execute,
// observe, persist, repeat, until the run completes, fails, or pauses for
// a hash-bound approval that Approve and Resume continue. Model calls go
// through an accounting caller that enforces the run's limits; repeated
// failures and repeated identical outcomes stop the run; a run interrupted
// mid-step is reconciled by the consumer on Resume, and a lease keeps two
// processes from executing one run. A Gate holds the same controls for a
// loop the runtime does not own: the caller decides, and the gate records,
// checks, and executes each proposed action, as the Driver, which runs on
// it, does for its agent. docs/architecture.md describes the design and
// docs/status.md what is verified.
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
	StatusRunning            RunStatus = "RUNNING"
	StatusWaitingForApproval RunStatus = "WAITING_FOR_APPROVAL"
	// StatusInterrupted is not written by the runtime: a run interrupted
	// mid-step stays RUNNING, and Resume finds it by its lease. Resume and
	// Cancel accept a run in it as they accept a RUNNING one.
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
	ReasonLimitModelCalls      TerminalReason = "limit_model_calls"
	ReasonLimitTokens          TerminalReason = "limit_tokens"
	ReasonLimitCost            TerminalReason = "limit_cost"
	ReasonLimitActiveTime      TerminalReason = "limit_active_time"
	ReasonLimitElapsedTime     TerminalReason = "limit_elapsed_time"
	ReasonApprovalExpired      TerminalReason = "approval_expired"
	ReasonModelUnavailable     TerminalReason = "model_unavailable"
	ReasonRepeatedToolFailures TerminalReason = "repeated_tool_failures"
	ReasonLoopDetected         TerminalReason = "loop_detected"
	ReasonPolicyAbort          TerminalReason = "policy_abort"
	ReasonToolAbort            TerminalReason = "tool_abort"
	ReasonApprovalRejected     TerminalReason = "approval_rejected"
	ReasonOperatorCancelled    TerminalReason = "operator_cancelled"
	ReasonReconcileConflict    TerminalReason = "reconcile_conflict"
	ReasonAgentError           TerminalReason = "agent_error"
	ReasonInternalError        TerminalReason = "internal_error"
)

// Limits are the caps the driver enforces before each step, and before each
// model call for the model limits. A zero optional limit is unlimited.
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
	// MaxModelCalls counts every attempt, including retries and ambiguous
	// ones. Zero means unlimited.
	MaxModelCalls int `json:"max_model_calls,omitempty"`
	// MaxOutputTokensPerCall caps each request's output. Zero leaves the
	// agent's value. A token or cost limit projects each call's output at
	// its cap, so under either limit a request must carry a cap, from this
	// field or its own MaxOutputTokens; one with neither is refused before
	// dispatch as limit_tokens or limit_cost, because it cannot be bounded.
	MaxOutputTokensPerCall int `json:"max_output_tokens_per_call,omitempty"`
	// MaxTotalTokens and MaxEstimatedCost are estimated limits: they are
	// checked before each call against totals plus a conservative estimate
	// for the call. Zero means unlimited.
	MaxTotalTokens   int    `json:"max_total_tokens,omitempty"`
	MaxEstimatedCost Micros `json:"max_estimated_cost_micros,omitempty"`
	// MaxActiveTime bounds the sum of step durations while RUNNING;
	// approval waits are excluded. MaxElapsedTime is an absolute deadline
	// from creation, including waits. ApprovalTTL bounds how long an
	// approval may stay pending: once it passes, the approval expires and
	// the run is cancelled when it is next touched. It does not bound an
	// approval already decided. GrantTTL bounds how long an approved grant
	// may wait for Resume, measured from its decision: Resume, or a Gate's
	// Attach or Execute, finding a grant older than that expires it and
	// cancels the run instead of executing it. Zero disables each; a run
	// stored before GrantTTL existed decodes it as zero.
	MaxActiveTime  time.Duration `json:"max_active_time,omitempty"`
	MaxElapsedTime time.Duration `json:"max_elapsed_time,omitempty"`
	ApprovalTTL    time.Duration `json:"approval_ttl,omitempty"`
	GrantTTL       time.Duration `json:"grant_ttl,omitempty"`
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
	// Model accounting totals across every attempt.
	ModelCalls    int
	Usage         Usage
	EstimatedCost Micros
	// ActiveTime is the sum of finished step durations.
	ActiveTime time.Duration
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
	// KindTruncated and KindNoToolCall are what a model agent records when
	// the model asked for nothing executable: a reply cut off by the output
	// cap, and a reply with no tool use at all. They are deliberately not
	// valid decisions, so the driver records an invalid_decision
	// observation, the step counts against the consecutive failure limit,
	// and the next render nudges the model from the recorded kind. See
	// render.Decide.
	KindTruncated  DecisionKind = "truncated"
	KindNoToolCall DecisionKind = "no_tool_call"
)

// DecisionOrigin records who proposed a step. The runtime records it and
// attaches no behaviour to it: policy does not see it.
type DecisionOrigin string

const (
	// OriginModel is a model's decision.
	OriginModel DecisionOrigin = "model"
	// OriginPlan is a step of a promoted deterministic plan.
	OriginPlan DecisionOrigin = "plan"
	// OriginOperator is a step an operator proposed.
	OriginOperator DecisionOrigin = "operator"
)

func (o DecisionOrigin) valid() bool {
	switch o {
	case "", OriginModel, OriginPlan, OriginOperator:
		return true
	}
	return false
}

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
	// Origin is who proposed the step, empty when unspecified. An empty
	// origin is not stored, so a decision without one is recorded exactly
	// as before Origin existed; a value outside the known set makes the
	// decision invalid.
	Origin DecisionOrigin `json:"origin,omitempty"`
	// InvalidArgs and InvalidResult are set only on a recorded decision:
	// arguments or a result the agent returned that were not usable JSON,
	// kept verbatim in place of Args or Result, which are then empty. When
	// either is not UTF-8, both are base64 and InvalidBase64 is set. A valid
	// decision never has them, so its stored form is unchanged by them.
	InvalidArgs   string `json:"invalid_args,omitempty"`
	InvalidResult string `json:"invalid_result,omitempty"`
	InvalidBase64 bool   `json:"invalid_base64,omitempty"`
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
	// ContentHash is the SHA-256 of the canonical JSON content. Loop
	// detection keys on it with the tool and its arguments.
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
	// SpecHash is the hash of the spec of the tool a tool_call decision
	// named, recorded with the decision when the tool was registered and
	// readable whole with Store.ToolSpec; empty for any other decision and
	// for a step written before it existed.
	SpecHash string
	// PolicyID is the identity of the policy that evaluated the step's
	// request, recorded once the policy was asked: empty when it does not
	// implement IdentifiedPolicy, when it was never asked, and when the
	// step's Policy was written by a reconciliation or an interrupted
	// side effect's pause rather than by the policy.
	PolicyID string
	// DecodeError is set when a stored column of the step could not be
	// decoded; the fields it would have filled are left empty. It lets a
	// damaged row be listed, shown, and cancelled rather than failing every
	// read of its run, and a driver refuses to continue such a run.
	DecodeError string
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
	// Kind, Capability, and Presentation apply to require_approval: the run
	// pauses on an approval that records them, hash-bound to the request,
	// and Resume executes the request once it is approved.
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
	ApprovalExpired  ApprovalStatus = "expired"
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
	ExpiresAt    time.Time       `json:"expires_at,omitzero"`
	DecidedAt    time.Time       `json:"decided_at,omitzero"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	Note         string          `json:"note,omitempty"`
	// DecodeError is set when a stored column of the approval could not be
	// decoded, as on Step. Such an approval cannot be decided or resumed.
	DecodeError string `json:"decode_error,omitempty"`
}

// Policy decides whether a validated tool request may execute. It is evaluated
// by the runtime and the agent has no influence over it.
type Policy interface {
	Evaluate(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error)
}

// IdentifiedPolicy is a Policy that names itself, so a recorded decision
// says which policy made it: a version, a digest of its rules, whatever
// lets an operator tell two policies apart. NewDriver and NewGate read the
// identity once, and refuse one longer than 256 bytes, not valid UTF-8, or
// holding a control character. It is recorded on each step the policy
// evaluated, as Step.PolicyID, and in its step.policy event. A policy
// that does not implement it is recorded with an empty identity.
type IdentifiedPolicy interface {
	Policy
	PolicyID() string
}

// StepInput is everything the agent receives when asked to decide. Steps are
// all prior steps in order; the agent is stateless between calls.
type StepInput struct {
	Run       Run
	Steps     []Step
	Approvals []Approval
	Tools     []ToolSpec
	// Model is the agent's only handle to the model. It is nil when the
	// driver was configured without one, as for scripted agents.
	Model ModelCaller
}

// Agent decides what happens next.
type Agent interface {
	Decide(ctx context.Context, in StepInput) (Decision, error)
}

// ReconcileOutcome is what a consumer's reconciliation concluded about a
// run found mid-step.
type ReconcileOutcome string

const (
	// ReconcileContinue means the loop may proceed with the next decision.
	ReconcileContinue ReconcileOutcome = "continue"
	// ReconcileCompleted means the interrupted operation had already
	// succeeded; the run completes with Result and no further decision.
	ReconcileCompleted ReconcileOutcome = "completed"
	// ReconcileWaiting means the interrupted request must wait for an
	// approval again. Reconciliation.Pause carries the require_approval
	// decision to pause on; the interrupted step returns to
	// awaiting_approval with a new hash-bound approval, and Resume executes
	// it once approved. Without Pause, or with no interrupted tool call to
	// pause, the run fails as reconcile_conflict.
	ReconcileWaiting ReconcileOutcome = "waiting"
	// ReconcileConflict means external state contradicts the record; the
	// run fails with reconcile_conflict and Detail.
	ReconcileConflict ReconcileOutcome = "conflict"
)

// Reconciliation is the structured result of a consumer's reconciliation.
// An Outcome outside the four above fails the run as internal_error.
type Reconciliation struct {
	Outcome ReconcileOutcome
	Result  json.RawMessage
	Detail  string
	// Pause applies to ReconcileWaiting: the require_approval decision the
	// interrupted request pauses on, as a policy would return it.
	Pause *PolicyDecision
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
	EventStepInterrupted   = "step.interrupted"
	// EventLeaseTakenOver records a Resume taking over a RUNNING run whose
	// lease another owner let expire. Its payload names the previous owner
	// and expiry, read from the wall clock, and the new owner.
	EventLeaseTakenOver  = "lease.taken_over"
	EventModelDispatched = "model.dispatched"
	EventModelCompleted  = "model.completed"
	EventModelFailed     = "model.failed"
)

// Observer receives every event after it has been committed.
type Observer func(Event)

// errEncode marks a value the runtime could not encode for storage or a
// hash. Consumer JSON is checked by checkJSON where it enters, so it means
// a runtime bug or a check missed; the write that needed the value fails,
// and the run fails as internal_error without executing anything more.
var errEncode = errors.New("agentrt: cannot encode")

// toJSON marshals a value for an event payload, a stored column, or a
// hash. It never substitutes: a stand-in would be stored as the record, or
// hashed as the approval, of every value that failed alike.
func toJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errEncode, err)
	}
	return b, nil
}
