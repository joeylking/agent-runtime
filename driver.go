package agentrt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"
)

// Config configures a Driver.
type Config struct {
	Store    *Store
	Agent    Agent
	Policy   Policy
	Tools    []Tool
	Observer Observer
	// Model, when set, is wrapped in a ModelCaller that agents receive in
	// StepInput. Scripted agents leave it nil.
	Model *ModelConfig
	// Reconcile, when set, runs before a run found mid-step is continued by
	// Resume. Consumers reconcile their own journaled operations and say
	// whether the run continues, completed, waits, or is in conflict.
	Reconcile func(ctx context.Context, view RunView) (Reconciliation, error)
	// Now and NewID may be overridden by tests.
	Now   func() time.Time
	NewID func() string
}

// Driver runs the step loop. It is safe to reuse for many runs but a single
// run is executed by one call at a time.
type Driver struct {
	store     *Store
	agent     Agent
	policy    Policy
	tools     map[string]Tool
	schemas   map[string]*compiledSchema
	specs     []ToolSpec
	observer  Observer
	model     *ModelConfig
	reconcile func(ctx context.Context, view RunView) (Reconciliation, error)
	now       func() time.Time
	newID     func() string
}

// NewDriver validates the configuration and compiles every tool schema.
func NewDriver(cfg Config) (*Driver, error) {
	if cfg.Store == nil {
		return nil, errors.New("agentrt: store is required")
	}
	if cfg.Agent == nil {
		return nil, errors.New("agentrt: agent is required")
	}
	if cfg.Policy == nil {
		return nil, errors.New("agentrt: policy is required")
	}
	if cfg.Model != nil && cfg.Model.Model == nil {
		return nil, errors.New("agentrt: model config without a model")
	}
	d := &Driver{
		store:     cfg.Store,
		agent:     cfg.Agent,
		policy:    cfg.Policy,
		tools:     map[string]Tool{},
		schemas:   map[string]*compiledSchema{},
		observer:  cfg.Observer,
		model:     cfg.Model,
		reconcile: cfg.Reconcile,
		now:       cfg.Now,
		newID:     cfg.NewID,
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.newID == nil {
		d.newID = newID
	}
	for _, t := range cfg.Tools {
		spec := t.Spec()
		if spec.Name == "" {
			return nil, errors.New("agentrt: tool with empty name")
		}
		if _, dup := d.tools[spec.Name]; dup {
			return nil, fmt.Errorf("agentrt: duplicate tool %q", spec.Name)
		}
		if !spec.SideEffect.valid() {
			return nil, fmt.Errorf("agentrt: tool %q: invalid side effect %q", spec.Name, spec.SideEffect)
		}
		if spec.Timeout <= 0 {
			return nil, fmt.Errorf("agentrt: tool %q: timeout is required", spec.Name)
		}
		cs, err := compileSchema(spec.Name, spec.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("agentrt: %w", err)
		}
		d.tools[spec.Name] = t
		d.schemas[spec.Name] = cs
		d.specs = append(d.specs, spec)
	}
	sort.Slice(d.specs, func(i, j int) bool { return d.specs[i].Name < d.specs[j].Name })
	return d, nil
}

// Start creates a run and executes it until it reaches a terminal status or
// pauses for approval. The returned run reflects the persisted state.
func (d *Driver) Start(ctx context.Context, goal string, limits Limits) (Run, error) {
	return d.StartWithID(ctx, d.newID(), goal, limits)
}

// StartWithID is Start with a caller-chosen run id, so a consumer can key
// its own tables by the same identifier. The id must be unique.
func (d *Driver) StartWithID(ctx context.Context, id, goal string, limits Limits) (Run, error) {
	if err := limits.validate(); err != nil {
		return Run{}, fmt.Errorf("agentrt: %w", err)
	}
	if id == "" {
		return Run{}, errors.New("agentrt: run id is required")
	}
	now := d.now()
	run := Run{
		ID:        id,
		Goal:      goal,
		Status:    StatusRunning,
		Limits:    limits,
		CreatedAt: now,
		StartedAt: now,
	}
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.insertRun(ctx, run); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunCreated, Payload: toJSON(map[string]any{"goal": goal, "limits": limits})}); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunStarted})
	})
	if err != nil {
		return Run{}, fmt.Errorf("agentrt: start run: %w", err)
	}
	return d.loop(ctx, run.ID)
}

// recordTimeout bounds the writes that record work already done after the
// caller's context was cancelled.
const recordTimeout = 10 * time.Second

// afterEffect returns the context for recording something that has already
// happened: a tool that ran, a model request that was sent. Cancelling the
// caller's context must not lose that record, so it is detached from
// cancellation and given a short deadline of its own.
func afterEffect(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

var running = []RunStatus{StatusRunning}

// loop executes steps until the run leaves RUNNING.
func (d *Driver) loop(ctx context.Context, runID string) (Run, error) {
	for {
		run, err := d.store.GetRun(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		if run.Status != StatusRunning {
			return run, nil
		}
		steps, err := d.store.ListSteps(ctx, runID)
		if err != nil {
			return Run{}, err
		}
		approvals, err := d.store.ListApprovals(ctx, runID)
		if err != nil {
			return Run{}, err
		}

		// Limits are checked before a step is started.
		if run.StepCount >= run.Limits.MaxSteps {
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonLimitSteps,
				detail: fmt.Sprintf("step limit %d reached", run.Limits.MaxSteps), limit: true})
		}
		if n := consecutiveFailures(steps); n >= run.Limits.MaxConsecutiveToolFailures {
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonRepeatedToolFailures,
				detail: fmt.Sprintf("%d consecutive failed steps", n), limit: true})
		}
		if l := run.Limits; l.MaxActiveTime > 0 && run.ActiveTime >= l.MaxActiveTime {
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonLimitActiveTime,
				detail: fmt.Sprintf("active time %s reached limit %s", run.ActiveTime.Round(time.Millisecond), l.MaxActiveTime), limit: true})
		}
		if l := run.Limits; l.MaxElapsedTime > 0 && d.now().Sub(run.CreatedAt) >= l.MaxElapsedTime {
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonLimitElapsedTime,
				detail: fmt.Sprintf("elapsed time reached limit %s", l.MaxElapsedTime), limit: true})
		}
		if n, sig := repeatedOutcome(steps); n >= run.Limits.LoopThreshold {
			at := d.now()
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonLoopDetected,
				detail: fmt.Sprintf("%s produced the same observation %d times in a row", sig.tool, n),
				pre: func(t *txn) error {
					return t.appendEvent(ctx, Event{RunID: runID, At: at, Type: EventLoopDetected, Payload: toJSON(map[string]any{"repeats": n, "tool": sig.tool, "args_hash": sig.args, "observation_hash": sig.obs})})
				}})
		}

		step, err := d.startStep(ctx, run)
		if err != nil {
			return d.lost(ctx, runID, err)
		}
		run.StepCount++

		var mc ModelCaller
		if d.model != nil {
			mc = &caller{d: d, cfg: *d.model, runID: run.ID, stepID: step.ID}
		}
		decision, err := d.agent.Decide(ctx, StepInput{Run: run, Steps: steps, Approvals: approvals, Tools: d.specs, Model: mc})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				// Cancelled while deciding: the step stays in flight and
				// Resume treats it as interrupted.
				return Run{}, cerr
			}
			reason := ReasonAgentError
			var lim ErrLimit
			var unavailable ErrModelUnavailable
			switch {
			case errors.As(err, &lim):
				reason = lim.Reason
			case errors.As(err, &unavailable):
				reason = ReasonModelUnavailable
			}
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: reason, detail: err.Error(), limit: reason != ReasonAgentError,
				step: &step, stepStatus: StepFailed, stepDetail: "agent error: " + err.Error()})
		}

		// The decision is recorded verbatim before validation so the audit log
		// shows what the agent asked for even when the request was invalid.
		// JSON that cannot be stored as JSON is kept as a string.
		recorded := recordable(decision)
		step.Decision = &recorded
		if err := d.recordDecision(ctx, &step); err != nil {
			return d.lost(ctx, runID, err)
		}

		if verr := d.validateDecision(decision); verr != nil {
			obs := observation(ObserveInvalidDecision, map[string]any{"error": verr.Error()}, "invalid decision: "+verr.Error())
			if err := d.endStep(ctx, &step, StepFailed, &obs, verr.Error()); err != nil {
				return d.lost(ctx, runID, err)
			}
			continue
		}

		switch decision.Kind {
		case DecideComplete:
			return d.settle(ctx, runID, running, ending{status: StatusCompleted, reason: ReasonGoalCompleted, result: decision.Result,
				step: &step, stepStatus: StepDone})
		case DecideFail:
			return d.settle(ctx, runID, running, ending{status: StatusFailed, reason: ReasonGoalFailed, detail: decision.Message,
				step: &step, stepStatus: StepDone})
		}

		// tool_call
		req := ToolRequest{RunID: run.ID, StepID: step.ID, Spec: d.tools[decision.Tool].Spec(), Args: orEmptyObject(decision.Args)}
		pd, perr := d.policy.Evaluate(ctx, req, RunView{Run: run, Steps: steps, Approvals: approvals})
		if fin, r, err := d.apply(ctx, run, &step, req, pd, perr, nil, time.Time{}); err != nil || fin {
			return r, err
		}
	}
}

// lost is how the loop answers a write that failed. When the run left
// RUNNING under it, as when an operator cancelled it, the loop stops and
// returns the run as it now is; any other failure is returned.
func (d *Driver) lost(ctx context.Context, runID string, err error) (Run, error) {
	if !errors.Is(err, ErrRunState) {
		return Run{}, err
	}
	rctx, cancel := afterEffect(ctx)
	defer cancel()
	fresh, gerr := d.store.GetRun(rctx, runID)
	if gerr == nil && fresh.Status.Terminal() {
		return fresh, nil
	}
	return Run{}, err
}

// recordable returns the decision as it is stored. Arguments or a result
// that are not usable JSON, which would not survive storage as JSON, are
// kept verbatim as a string under "invalid_json" (base64 under
// "invalid_json_base64" when they are not even UTF-8); validation then
// rejects the decision with the original bytes.
func recordable(dec Decision) Decision {
	if len(bytes.TrimSpace(dec.Args)) > 0 && checkJSON(dec.Args) != nil {
		dec.Args = toJSON(rawField("invalid_json", dec.Args, map[string]any{}))
	}
	if len(bytes.TrimSpace(dec.Result)) > 0 && checkJSON(dec.Result) != nil {
		dec.Result = toJSON(rawField("invalid_json", dec.Result, map[string]any{}))
	}
	return dec
}

// rawField stores bytes that are not usable JSON in m under key, as a
// string, or base64 under key+"_base64" when they are not UTF-8.
func rawField(key string, raw []byte, m map[string]any) map[string]any {
	if utf8.Valid(raw) {
		m[key] = string(raw)
	} else {
		m[key+"_base64"] = base64.StdEncoding.EncodeToString(raw)
	}
	return m
}

// consecutiveFailures counts trailing steps that ended with a failure
// observation. A step still in progress or without an observation breaks the
// sequence.
func consecutiveFailures(steps []Step) int {
	n := 0
	for i := len(steps) - 1; i >= 0; i-- {
		o := steps[i].Observation
		if o == nil || !o.Failure() {
			break
		}
		n++
	}
	return n
}

// outcomeSignature identifies a tool call by name, canonical arguments, and
// the observation it produced.
type outcomeSignature struct {
	tool, args, obs string
}

// repeatedOutcome counts trailing tool-call steps that share one signature.
// Only completed tool calls with an observation take part; a decision that
// was invalid or is still in progress breaks the sequence.
func repeatedOutcome(steps []Step) (int, outcomeSignature) {
	var sig outcomeSignature
	n := 0
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		if st.Decision == nil || st.Decision.Kind != DecideToolCall || st.Observation == nil || st.Observation.Kind == ObserveInvalidDecision {
			break
		}
		s := outcomeSignature{tool: st.Decision.Tool, args: contentHash(orEmptyObject(st.Decision.Args)), obs: st.Observation.ContentHash}
		if n == 0 {
			sig = s
		} else if s != sig {
			break
		}
		n++
	}
	return n, sig
}

func (d *Driver) validateDecision(dec Decision) error {
	switch dec.Kind {
	case DecideComplete:
		if len(bytes.TrimSpace(dec.Result)) > 0 {
			if err := checkJSON(dec.Result); err != nil {
				return fmt.Errorf("result is not usable JSON: %w", err)
			}
		}
		return nil
	case DecideFail:
		return nil
	case DecideToolCall:
		if dec.Tool == "" {
			return errors.New("tool_call without a tool name")
		}
		cs, ok := d.schemas[dec.Tool]
		if !ok {
			return fmt.Errorf("unknown tool %q", dec.Tool)
		}
		if len(bytes.TrimSpace(dec.Args)) > 0 {
			if err := checkJSON(dec.Args); err != nil {
				return fmt.Errorf("arguments are not usable JSON: %w", err)
			}
		}
		return cs.validate(dec.Args)
	case "":
		return errors.New("decision has no kind")
	case KindTruncated:
		return errors.New("the reply was cut off by the output cap before a tool call")
	case KindNoToolCall:
		return errors.New("the reply contained no tool call")
	default:
		return fmt.Errorf("unknown decision kind %q", dec.Kind)
	}
}

// checkPolicy rejects a decision whose capability or presentation is not
// usable JSON: it could be neither stored nor hashed faithfully.
func checkPolicy(pd PolicyDecision) error {
	if len(bytes.TrimSpace(pd.Capability)) > 0 {
		if err := checkJSON(pd.Capability); err != nil {
			return fmt.Errorf("capability is not usable JSON: %w", err)
		}
	}
	if len(bytes.TrimSpace(pd.Presentation)) > 0 {
		if err := checkJSON(pd.Presentation); err != nil {
			return fmt.Errorf("presentation is not usable JSON: %w", err)
		}
	}
	return nil
}

// apply acts on a policy decision for a validated request. It is the one
// dispatch for both the loop and Resume, so an outcome means the same in
// both and anything unrecognised fails the run as internal_error. granted
// is the approval being resumed, nil in the loop: the run is then WAITING
// rather than RUNNING, and a require_approval outcome whose hash matches
// the granted approval executes. The policy decision is recorded on the
// step, with a step.policy event, in the same transaction as what it
// caused. finished reports that the run stopped, returning it.
//
// Each write takes its times from the clock in the order and number the
// runtime always has, one reading per recorded state change, because a
// consumer with a deterministic clock keys its own records on that
// sequence. resumedAt is the reading Resume took for run.resumed.
func (d *Driver) apply(ctx context.Context, run Run, step *Step, req ToolRequest, pd PolicyDecision, perr error, granted *Approval, resumedAt time.Time) (finished bool, out Run, err error) {
	from, suffix := running, ""
	if granted != nil {
		from, suffix = []RunStatus{StatusWaitingForApproval}, " on resume"
	}
	// fail answers a write that failed. The first write of a resume decides
	// the race between two resumers, so its loser gets ErrRunState.
	fail := func(err error) (bool, Run, error) {
		if granted != nil {
			return true, Run{}, err
		}
		r, err := d.lost(ctx, run.ID, err)
		return true, r, err
	}
	settle := func(e ending) (bool, Run, error) {
		r, err := d.settle(ctx, run.ID, from, e)
		if err != nil && granted == nil {
			return fail(err)
		}
		return true, r, err
	}
	if perr != nil {
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: "policy: " + perr.Error(),
			step: step, stepStatus: StepFailed, stepDetail: "policy error: " + perr.Error()})
	}
	policyAt := resumedAt
	if granted == nil {
		policyAt = d.now()
	}
	policyEvent := func(t *txn) error {
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: policyAt, Type: EventStepPolicy, Payload: toJSON(step.Policy)})
	}
	if cerr := checkPolicy(pd); cerr != nil {
		step.Policy = &PolicyDecision{Outcome: pd.Outcome, Reason: pd.Reason, Kind: pd.Kind}
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: "policy: " + cerr.Error(), pre: policyEvent,
			step: step, stepStatus: StepFailed, stepDetail: "policy error: " + cerr.Error()})
	}
	step.Policy = &pd

	allowed := pd.Outcome == Allow
	if pd.Outcome == RequireApproval && granted != nil && approvalHash(pd.Kind, pd.Capability, pd.Presentation, req) == granted.Hash {
		allowed = true
	}
	switch {
	case allowed:
		startAt := policyAt
		fromStep := step.Status
		step.Status = StepExecuting
		if granted != nil {
			// The step's clock restarts here: the wait for the human is not
			// active time.
			startAt = d.now()
			step.StartedAt = startAt
		}
		if err := d.store.tx(ctx, d.observer, func(t *txn) error {
			if err := d.enter(ctx, t, run, granted, resumedAt); err != nil {
				return err
			}
			if err := t.updateStep(ctx, *step, fromStep); err != nil {
				return err
			}
			if err := policyEvent(t); err != nil {
				return err
			}
			payload := map[string]any{"tool": req.Spec.Name, "args": req.Args}
			if granted != nil {
				payload["approval_id"] = granted.ID
			}
			return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: startAt, Type: EventStepToolStarted, Payload: toJSON(payload)})
		}); err != nil {
			return fail(err)
		}
		return d.runTool(ctx, run, step, req)
	case pd.Outcome == Deny:
		obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "denied"+suffix+": "+pd.Reason)
		stepAt := d.now()
		if err := d.store.tx(ctx, d.observer, func(t *txn) error {
			if err := d.enter(ctx, t, run, granted, resumedAt); err != nil {
				return err
			}
			if err := policyEvent(t); err != nil {
				return err
			}
			return d.endStepTx(ctx, t, step, StepFailed, &obs, obs.Summary, stepAt)
		}); err != nil {
			return fail(err)
		}
		return false, Run{}, nil
	case pd.Outcome == Abort:
		obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "aborted"+suffix+": "+pd.Reason)
		return settle(ending{status: StatusFailed, reason: ReasonPolicyAbort, detail: pd.Reason, pre: policyEvent,
			step: step, stepStatus: StepFailed, obs: &obs, stepDetail: obs.Summary})
	case pd.Outcome == RequireApproval:
		// In the loop this is the first request; on resume the policy now
		// wants something other than what was granted, so the run pauses
		// again.
		r, err := d.pause(ctx, run.ID, step, req, pd, from, policyAt)
		if err != nil {
			return fail(err)
		}
		return true, r, nil
	default:
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("unknown policy outcome %q", pd.Outcome), pre: policyEvent,
			step: step, stepStatus: StepFailed, stepDetail: "unknown policy outcome"})
	}
}

// enter checks inside a transaction that the run may take the step's next
// write: in the loop it must still be RUNNING; on resume it moves from
// WAITING to RUNNING here, with run.resumed, so two resumers cannot both
// pass.
func (d *Driver) enter(ctx context.Context, t *txn, run Run, granted *Approval, now time.Time) error {
	if granted == nil {
		_, err := t.requireRun(ctx, run.ID, StatusRunning)
		return err
	}
	run.Status = StatusRunning
	if err := t.transition(ctx, run, StatusWaitingForApproval); err != nil {
		return err
	}
	return t.appendEvent(ctx, Event{RunID: run.ID, StepID: granted.StepID, At: now, Type: EventRunResumed, Payload: toJSON(map[string]any{"approval_id": granted.ID})})
}

// runTool executes a request whose step is already recorded as executing,
// and records the outcome. It returns finished=true with the final run
// when the tool ended the run, either as a terminal tool or by aborting.
// The tool has had its effect by the time it returns, so the outcome is
// recorded even when ctx was cancelled meanwhile; the cancellation is then
// returned.
func (d *Driver) runTool(ctx context.Context, run Run, step *Step, req ToolRequest) (bool, Run, error) {
	obs, abort := d.execute(ctx, req)
	rctx, cancel := afterEffect(ctx)
	defer cancel()
	var out Run
	var err error
	finished := true
	switch {
	case abort != nil:
		out, err = d.settle(rctx, run.ID, running, ending{status: StatusFailed, reason: ReasonToolAbort, detail: abort.Detail,
			step: step, stepStatus: StepFailed, obs: &obs, stepDetail: obs.Summary})
	case obs.Failure():
		finished = false
		err = d.endStep(rctx, step, StepFailed, &obs, obs.Summary)
	case req.Spec.Terminal:
		out, err = d.settle(rctx, run.ID, running, ending{status: StatusCompleted, reason: ReasonGoalCompleted, detail: "terminal tool " + req.Spec.Name, result: obs.Content,
			step: step, stepStatus: StepDone, obs: &obs})
	default:
		finished = false
		err = d.endStep(rctx, step, StepDone, &obs, "")
	}
	if err != nil {
		r, err := d.lost(ctx, run.ID, err)
		return true, r, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return true, out, cerr
	}
	return finished, out, nil
}

// execute runs the tool with its timeout and converts the outcome into an
// observation. Panics inside a tool are observed as errors. A returned
// ErrAbortRun is reported separately so the run can end. Content that is
// not usable JSON becomes a tool_error keeping the bytes as a string,
// because the side effect happened and its record must survive. A tool
// that fails because the caller's context was cancelled is observed as
// interrupted: its outcome is unknown.
func (d *Driver) execute(ctx context.Context, req ToolRequest) (obs Observation, abort *ErrAbortRun) {
	tool := d.tools[req.Spec.Name]
	tctx, cancel := context.WithTimeout(ctx, req.Spec.Timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			obs = observation(ObserveToolError, map[string]any{"tool": req.Spec.Name, "error": fmt.Sprint("panic: ", r)}, fmt.Sprintf("%s panicked", req.Spec.Name))
			abort = nil
		}
	}()
	res, err := tool.Call(tctx, ToolCall{RunID: req.RunID, StepID: req.StepID, Args: req.Args})
	if err != nil {
		var ab ErrAbortRun
		if errors.As(err, &ab) {
			return observation(ObserveToolError, map[string]any{"tool": req.Spec.Name, "error": ab.Detail, "failure": "abort"}, fmt.Sprintf("%s aborted the run: %s", req.Spec.Name, ab.Detail)), &ab
		}
		if ctx.Err() != nil {
			return observation(ObserveInterrupted, map[string]any{"tool": req.Spec.Name, "error": err.Error(), "failure": "cancelled"}, fmt.Sprintf("%s was cancelled and its outcome is unknown: %s", req.Spec.Name, err.Error())), nil
		}
		kind := "error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(tctx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		return observation(ObserveToolError, map[string]any{"tool": req.Spec.Name, "error": err.Error(), "failure": kind}, fmt.Sprintf("%s %s: %s", req.Spec.Name, kind, err.Error())), nil
	}
	content := orEmptyObject(res.Content)
	if cerr := checkJSON(content); cerr != nil {
		msg := "content is not usable JSON: " + cerr.Error()
		return observation(ObserveToolError, rawField("content", content, map[string]any{"tool": req.Spec.Name, "error": msg, "failure": "invalid_content"}), fmt.Sprintf("%s returned %s", req.Spec.Name, msg)), nil
	}
	return Observation{Kind: ObserveToolResult, Content: content, Summary: res.Summary, ContentHash: contentHash(content)}, nil
}

func observation(kind ObservationKind, content any, summary string) Observation {
	raw := toJSON(content)
	return Observation{Kind: kind, Content: raw, Summary: summary, ContentHash: contentHash(raw)}
}

// startStep claims the next step index of a RUNNING run and records it.
func (d *Driver) startStep(ctx context.Context, run Run) (Step, error) {
	now := d.now()
	step := Step{ID: d.newID(), RunID: run.ID, Index: run.StepCount, Status: StepDeciding, StartedAt: now}
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.claimStep(ctx, run.ID, step.Index); err != nil {
			return err
		}
		if err := t.insertStep(ctx, step); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: now, Type: EventStepStarted, Payload: toJSON(map[string]any{"index": step.Index})})
	})
	return step, err
}

func (d *Driver) recordDecision(ctx context.Context, step *Step) error {
	now := d.now()
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if _, err := t.requireRun(ctx, step.RunID, StatusRunning); err != nil {
			return err
		}
		if err := t.updateStep(ctx, *step, StepDeciding); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepDecided, Payload: toJSON(step.Decision)})
	})
}

// endStep finishes a step of a RUNNING run without ending the run.
func (d *Driver) endStep(ctx context.Context, step *Step, status StepStatus, obs *Observation, detail string) error {
	now := d.now()
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if _, err := t.requireRun(ctx, step.RunID, StatusRunning); err != nil {
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
		if err := t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolFinished, Payload: toJSON(payload)}); err != nil {
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
	return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepFailed, Payload: toJSON(payload)})
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
func (d *Driver) settle(ctx context.Context, runID string, from []RunStatus, e ending) (Run, error) {
	var stepAt time.Time
	if e.step != nil {
		stepAt = d.now()
	}
	now := d.now()
	var out Run
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if _, err := t.requireRun(ctx, runID, from...); err != nil {
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
			if err := t.appendEvent(ctx, Event{RunID: runID, At: now, Type: EventLimitExceeded, Payload: toJSON(map[string]any{"reason": e.reason, "detail": e.detail})}); err != nil {
				return err
			}
		}
		out = r
		return t.appendEvent(ctx, Event{RunID: runID, At: now, Type: EventRunFinished, Payload: toJSON(map[string]any{"status": e.status, "reason": e.reason, "detail": e.detail, "steps": r.StepCount})})
	})
	if err != nil {
		return Run{}, err
	}
	return out, nil
}

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
func (d *Driver) pause(ctx context.Context, runID string, step *Step, req ToolRequest, pd PolicyDecision, from []RunStatus, policyAt time.Time) (Run, error) {
	now := d.now()
	fromStep := step.Status
	step.Status, step.Policy = StepAwaitingApproval, &pd
	var out Run
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
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
		if _, err := t.requireRun(ctx, runID, StatusWaitingForApproval); err != nil {
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

var notTerminal = []RunStatus{StatusCreated, StatusRunning, StatusWaitingForApproval, StatusInterrupted}

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

// Resume continues a run. A WAITING run needs an approved approval for its
// awaiting step: policy is re-evaluated while the run is still WAITING,
// and the recorded request is executed only if policy allows it or asks
// for exactly the approval that was granted. The move to RUNNING is
// compare-and-set, so of two concurrent resumes one executes the request
// and the other returns ErrRunState. A RUNNING run with in-flight work was
// interrupted: the step is failed with an interrupted observation, the
// consumer's reconciliation runs, and the loop continues. Resume cannot
// tell a crashed process from a live one; the caller must ensure no
// process is executing a RUNNING run it resumes.
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

func (d *Driver) resumeApproved(ctx context.Context, run Run) (Run, error) {
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
	pd, perr := d.policy.Evaluate(ctx, req, RunView{Run: run, Steps: prior, Approvals: approvals})
	if fin, r, err := d.apply(ctx, run, step, req, pd, perr, granted, resumedAt); err != nil || fin {
		return r, err
	}
	return d.loop(ctx, run.ID)
}

var resumable = []RunStatus{StatusRunning, StatusInterrupted}

func (d *Driver) resumeInterrupted(ctx context.Context, run Run) (Run, error) {
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
	if len(inFlight) > 0 {
		if err := d.store.tx(ctx, d.observer, func(t *txn) error {
			if _, err := t.requireRun(ctx, run.ID, resumable...); err != nil {
				return err
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
	}
	if d.reconcile != nil {
		rec, err := d.reconcile(ctx, RunView{Run: run, Steps: steps, Approvals: approvals})
		if err != nil {
			return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()})
		}
		switch rec.Outcome {
		case ReconcileContinue:
		case ReconcileCompleted:
			if len(bytes.TrimSpace(rec.Result)) > 0 {
				if cerr := checkJSON(rec.Result); cerr != nil {
					return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: result is not usable JSON: " + cerr.Error()})
				}
			}
			return d.settle(ctx, run.ID, resumable, ending{status: StatusCompleted, reason: ReasonGoalCompleted, detail: "completed by reconciliation: " + rec.Detail, result: rec.Result})
		case ReconcileConflict:
			return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict, detail: rec.Detail})
		case ReconcileWaiting:
			return d.reconcilePause(ctx, run, inFlight, rec, now)
		default:
			return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: unknown outcome %q", rec.Outcome)})
		}
	}
	run.Status = StatusRunning
	if err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.transition(ctx, run, resumable...); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunResumed, Payload: toJSON(map[string]any{"after": "interruption"})})
	}); err != nil {
		return Run{}, err
	}
	return d.loop(ctx, run.ID)
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
		return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonReconcileConflict, detail: detail})
	}
	if rec.Pause == nil {
		return conflict("reconciliation asked to wait but gave no approval to wait for")
	}
	if rec.Pause.Outcome != RequireApproval {
		return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("reconcile: waiting needs a require_approval decision, got %q", rec.Pause.Outcome)})
	}
	if err := checkPolicy(*rec.Pause); err != nil {
		return d.settle(ctx, run.ID, resumable, ending{status: StatusFailed, reason: ReasonInternalError, detail: "reconcile: " + err.Error()})
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
	return d.pause(ctx, run.ID, step, req, *rec.Pause, resumable, now)
}
