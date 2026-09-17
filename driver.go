package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Config configures a Driver.
type Config struct {
	Store    *Store
	Agent    Agent
	Policy   Policy
	Tools    []Tool
	Observer Observer
	// Reconcile, when set, runs before a run found mid-step is continued by
	// Resume. Consumers use it to reconcile their own journaled operations.
	Reconcile func(ctx context.Context, view RunView) error
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
	reconcile func(ctx context.Context, view RunView) error
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
	d := &Driver{
		store:     cfg.Store,
		agent:     cfg.Agent,
		policy:    cfg.Policy,
		tools:     map[string]Tool{},
		schemas:   map[string]*compiledSchema{},
		observer:  cfg.Observer,
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
	if err := limits.validate(); err != nil {
		return Run{}, fmt.Errorf("agentrt: %w", err)
	}
	now := d.now()
	run := Run{
		ID:        d.newID(),
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
		if err := t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunCreated, Payload: mustJSON(map[string]any{"goal": goal, "limits": limits})}); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunStarted})
	})
	if err != nil {
		return Run{}, fmt.Errorf("agentrt: start run: %w", err)
	}
	return d.loop(ctx, run.ID)
}

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
			return d.finish(ctx, run, StatusFailed, ReasonLimitSteps,
				fmt.Sprintf("step limit %d reached", run.Limits.MaxSteps), nil, true)
		}
		if n := consecutiveFailures(steps); n >= run.Limits.MaxConsecutiveToolFailures {
			return d.finish(ctx, run, StatusFailed, ReasonRepeatedToolFailures,
				fmt.Sprintf("%d consecutive failed steps", n), nil, true)
		}
		if n, sig := repeatedOutcome(steps); n >= run.Limits.LoopThreshold {
			now := d.now()
			err := d.store.tx(ctx, d.observer, func(t *txn) error {
				return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventLoopDetected, Payload: mustJSON(map[string]any{"repeats": n, "tool": sig.tool, "args_hash": sig.args, "observation_hash": sig.obs})})
			})
			if err != nil {
				return Run{}, err
			}
			return d.finish(ctx, run, StatusFailed, ReasonLoopDetected,
				fmt.Sprintf("%s produced the same observation %d times in a row", sig.tool, n), nil, false)
		}

		step, err := d.startStep(ctx, &run)
		if err != nil {
			return Run{}, err
		}

		decision, err := d.agent.Decide(ctx, StepInput{Run: run, Steps: steps, Approvals: approvals, Tools: d.specs})
		if err != nil {
			if ferr := d.failStep(ctx, &step, nil, "agent error: "+err.Error()); ferr != nil {
				return Run{}, ferr
			}
			return d.finish(ctx, run, StatusFailed, ReasonAgentError, err.Error(), nil, false)
		}
		step.Decision = &decision

		// The decision is recorded verbatim before validation so the audit log
		// shows what the agent asked for even when the request was invalid.
		if err := d.recordDecision(ctx, step); err != nil {
			return Run{}, err
		}

		if verr := d.validateDecision(decision); verr != nil {
			obs := observation(ObserveInvalidDecision, map[string]any{"error": verr.Error()}, "invalid decision: "+verr.Error())
			if err := d.failStep(ctx, &step, &obs, verr.Error()); err != nil {
				return Run{}, err
			}
			continue
		}

		switch decision.Kind {
		case DecideComplete:
			if err := d.doneStep(ctx, &step, nil); err != nil {
				return Run{}, err
			}
			return d.finish(ctx, run, StatusCompleted, ReasonGoalCompleted, "", decision.Result, false)
		case DecideFail:
			if err := d.doneStep(ctx, &step, nil); err != nil {
				return Run{}, err
			}
			return d.finish(ctx, run, StatusFailed, ReasonGoalFailed, decision.Message, nil, false)
		}

		// tool_call
		req := ToolRequest{RunID: run.ID, StepID: step.ID, Spec: d.tools[decision.Tool].Spec(), Args: orEmptyObject(decision.Args)}
		pd, err := d.policy.Evaluate(ctx, req, RunView{Run: run, Steps: steps, Approvals: approvals})
		if err != nil {
			if ferr := d.failStep(ctx, &step, nil, "policy error: "+err.Error()); ferr != nil {
				return Run{}, ferr
			}
			return d.finish(ctx, run, StatusFailed, ReasonInternalError, "policy: "+err.Error(), nil, false)
		}
		step.Policy = &pd
		if err := d.recordPolicy(ctx, step); err != nil {
			return Run{}, err
		}

		switch pd.Outcome {
		case Allow:
			if fin, r, err := d.runTool(ctx, run, &step, req); err != nil || fin {
				return r, err
			}
		case Deny:
			obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "denied: "+pd.Reason)
			if err := d.failStep(ctx, &step, &obs, obs.Summary); err != nil {
				return Run{}, err
			}
		case Abort:
			obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "aborted: "+pd.Reason)
			if err := d.failStep(ctx, &step, &obs, obs.Summary); err != nil {
				return Run{}, err
			}
			return d.finish(ctx, run, StatusFailed, ReasonPolicyAbort, pd.Reason, nil, false)
		case RequireApproval:
			return d.pause(ctx, run, step, req, pd)
		default:
			if ferr := d.failStep(ctx, &step, nil, "unknown policy outcome"); ferr != nil {
				return Run{}, ferr
			}
			return d.finish(ctx, run, StatusFailed, ReasonInternalError, fmt.Sprintf("unknown policy outcome %q", pd.Outcome), nil, false)
		}
	}
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
	case DecideComplete, DecideFail:
		return nil
	case DecideToolCall:
		if dec.Tool == "" {
			return errors.New("tool_call without a tool name")
		}
		cs, ok := d.schemas[dec.Tool]
		if !ok {
			return fmt.Errorf("unknown tool %q", dec.Tool)
		}
		return cs.validate(dec.Args)
	case "":
		return errors.New("decision has no kind")
	default:
		return fmt.Errorf("unknown decision kind %q", dec.Kind)
	}
}

// runTool executes an allowed request and records the outcome. It returns
// finished=true with the final run when the tool ended the run, either as
// a terminal tool or by aborting.
func (d *Driver) runTool(ctx context.Context, run Run, step *Step, req ToolRequest) (bool, Run, error) {
	obs, abort := d.execute(ctx, req)
	if abort != nil {
		if err := d.failStep(ctx, step, &obs, obs.Summary); err != nil {
			return true, Run{}, err
		}
		r, err := d.finish(ctx, run, StatusFailed, ReasonToolAbort, abort.Detail, nil, false)
		return true, r, err
	}
	if obs.Failure() {
		return false, Run{}, d.failStep(ctx, step, &obs, obs.Summary)
	}
	if err := d.doneStep(ctx, step, &obs); err != nil {
		return true, Run{}, err
	}
	if req.Spec.Terminal {
		r, err := d.finish(ctx, run, StatusCompleted, ReasonGoalCompleted, "terminal tool "+req.Spec.Name, obs.Content, false)
		return true, r, err
	}
	return false, Run{}, nil
}

// execute runs the tool with its timeout and converts the outcome into an
// observation. Panics inside a tool are observed as errors. A returned
// ErrAbortRun is reported separately so the run can end.
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
		kind := "error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(tctx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		return observation(ObserveToolError, map[string]any{"tool": req.Spec.Name, "error": err.Error(), "failure": kind}, fmt.Sprintf("%s %s: %s", req.Spec.Name, kind, err.Error())), nil
	}
	content := orEmptyObject(res.Content)
	return Observation{Kind: ObserveToolResult, Content: content, Summary: res.Summary, ContentHash: contentHash(content)}, nil
}

func observation(kind ObservationKind, content any, summary string) Observation {
	raw := mustJSON(content)
	return Observation{Kind: kind, Content: raw, Summary: summary, ContentHash: contentHash(raw)}
}

func (d *Driver) startStep(ctx context.Context, run *Run) (Step, error) {
	now := d.now()
	step := Step{ID: d.newID(), RunID: run.ID, Index: run.StepCount, Status: StepDeciding, StartedAt: now}
	run.StepCount++
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.insertStep(ctx, step); err != nil {
			return err
		}
		if err := t.updateRun(ctx, *run); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: now, Type: EventStepStarted, Payload: mustJSON(map[string]any{"index": step.Index})})
	})
	return step, err
}

func (d *Driver) recordDecision(ctx context.Context, step Step) error {
	now := d.now()
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, step); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepDecided, Payload: mustJSON(step.Decision)})
	})
}

func (d *Driver) recordPolicy(ctx context.Context, step Step) error {
	now := d.now()
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, step); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepPolicy, Payload: mustJSON(step.Policy)}); err != nil {
			return err
		}
		if step.Policy.Outcome == Allow {
			step.Status = StepExecuting
			if err := t.updateStep(ctx, step); err != nil {
				return err
			}
			return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolStarted, Payload: mustJSON(map[string]any{"tool": step.Decision.Tool, "args": orEmptyObject(step.Decision.Args)})})
		}
		return nil
	})
}

func (d *Driver) doneStep(ctx context.Context, step *Step, obs *Observation) error {
	now := d.now()
	step.Status = StepDone
	step.Observation = obs
	step.FinishedAt = now
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, *step); err != nil {
			return err
		}
		if obs != nil {
			return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolFinished, Payload: mustJSON(map[string]any{
				"tool": step.Decision.Tool, "summary": obs.Summary, "content_hash": obs.ContentHash, "duration_ms": now.Sub(step.StartedAt).Milliseconds(),
			})})
		}
		return nil
	})
}

func (d *Driver) failStep(ctx context.Context, step *Step, obs *Observation, detail string) error {
	now := d.now()
	step.Status = StepFailed
	step.Observation = obs
	step.FinishedAt = now
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, *step); err != nil {
			return err
		}
		payload := map[string]any{"detail": detail}
		if obs != nil {
			payload["observation"] = obs.Kind
			payload["content_hash"] = obs.ContentHash
		}
		if step.Decision != nil && step.Decision.Kind == DecideToolCall && step.Policy != nil && step.Policy.Outcome == Allow {
			if err := t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepToolFinished, Payload: mustJSON(map[string]any{
				"tool": step.Decision.Tool, "error": detail, "duration_ms": now.Sub(step.StartedAt).Milliseconds(),
			})}); err != nil {
				return err
			}
		}
		return t.appendEvent(ctx, Event{RunID: step.RunID, StepID: step.ID, At: now, Type: EventStepFailed, Payload: mustJSON(payload)})
	})
}

// approvalHash binds an approval to exactly what was asked and shown.
func approvalHash(kind string, capability, presentation json.RawMessage, req ToolRequest) string {
	return contentHash(mustJSON(map[string]any{
		"kind": kind, "capability": orEmptyObject(capability), "presentation": orEmptyObject(presentation), "request": req,
	}))
}

// pause records a hash-bound approval and parks the run.
func (d *Driver) pause(ctx context.Context, run Run, step Step, req ToolRequest, pd PolicyDecision) (Run, error) {
	now := d.now()
	step.Status = StepAwaitingApproval
	run.Status = StatusWaitingForApproval
	a := Approval{ID: d.newID(), RunID: run.ID, StepID: step.ID, Kind: pd.Kind, Capability: orEmptyObject(pd.Capability), Presentation: orEmptyObject(pd.Presentation), Request: req, Status: ApprovalPending, CreatedAt: now}
	a.Hash = approvalHash(a.Kind, a.Capability, a.Presentation, a.Request)
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, step); err != nil {
			return err
		}
		if err := t.updateRun(ctx, run); err != nil {
			return err
		}
		if err := t.insertApproval(ctx, a); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: now, Type: EventApprovalRequested, Payload: mustJSON(map[string]any{
			"approval_id": a.ID, "kind": pd.Kind, "reason": pd.Reason, "capability": a.Capability, "presentation": a.Presentation, "hash": a.Hash,
		})})
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

// ErrApprovalHash means a stored approval no longer matches its own fields.
var ErrApprovalHash = errors.New("agentrt: approval hash does not match its recorded request")

// Approve marks a pending approval approved after recomputing its hash
// from the stored fields. The run stays WAITING until Resume.
func (d *Driver) Approve(ctx context.Context, runID, approvalID, by, note string) error {
	return d.decide(ctx, runID, approvalID, by, note, ApprovalApproved)
}

// Reject marks a pending approval rejected and ends the run as CANCELLED.
func (d *Driver) Reject(ctx context.Context, runID, approvalID, by, note string) error {
	return d.decide(ctx, runID, approvalID, by, note, ApprovalRejected)
}

func (d *Driver) decide(ctx context.Context, runID, approvalID, by, note string, status ApprovalStatus) error {
	run, err := d.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status != StatusWaitingForApproval {
		return fmt.Errorf("agentrt: run %s is %s, not waiting for approval", runID, run.Status)
	}
	a, err := d.store.GetApproval(ctx, runID, approvalID)
	if err != nil {
		return err
	}
	if a.Status != ApprovalPending {
		return fmt.Errorf("agentrt: approval %s is already %s", approvalID, a.Status)
	}
	if approvalHash(a.Kind, a.Capability, a.Presentation, a.Request) != a.Hash {
		return ErrApprovalHash
	}
	now := d.now()
	a.Status, a.DecidedAt, a.DecidedBy, a.Note = status, now, by, note
	// Loaded before the transaction: the store has a single connection.
	steps, err := d.store.ListSteps(ctx, runID)
	if err != nil {
		return err
	}
	return d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.decideApproval(ctx, a); err != nil {
			return err
		}
		if err := t.appendEvent(ctx, Event{RunID: runID, StepID: a.StepID, At: now, Type: EventApprovalDecided, Payload: mustJSON(map[string]any{"approval_id": a.ID, "status": status, "by": by, "note": note})}); err != nil {
			return err
		}
		if status == ApprovalRejected {
			for _, st := range steps {
				if st.ID == a.StepID {
					st.Status = StepFailed
					obs := observation(ObservePolicyDenied, map[string]any{"approval_id": a.ID, "note": note}, "approval rejected: "+note)
					st.Observation = &obs
					st.FinishedAt = now
					if err := t.updateStep(ctx, st); err != nil {
						return err
					}
				}
			}
			run.Status, run.Reason, run.ReasonDetail, run.FinishedAt = StatusCancelled, ReasonApprovalRejected, note, now
			if err := t.updateRun(ctx, run); err != nil {
				return err
			}
			return t.appendEvent(ctx, Event{RunID: runID, At: now, Type: EventRunFinished, Payload: mustJSON(map[string]any{"status": run.Status, "reason": run.Reason, "detail": note, "steps": run.StepCount})})
		}
		return nil
	})
}

// Resume continues a run. A WAITING run needs an approved approval for its
// awaiting step: policy is re-evaluated, and the recorded request is
// executed only if policy allows it or asks for exactly the approval that
// was granted. A RUNNING run with in-flight work was interrupted: the step
// is failed with an interrupted observation, the consumer's reconciliation
// runs, and the loop continues.
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
		return Run{}, fmt.Errorf("agentrt: run %s is %s and cannot be resumed", runID, run.Status)
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
		return Run{}, fmt.Errorf("agentrt: run %s is waiting but has no awaiting step", run.ID)
	}
	var granted *Approval
	for i := range approvals {
		a := &approvals[i]
		if a.StepID == step.ID && a.Status == ApprovalApproved {
			granted = a
		}
	}
	if granted == nil {
		return Run{}, fmt.Errorf("agentrt: run %s has no approved approval for step %s", run.ID, step.ID)
	}
	if approvalHash(granted.Kind, granted.Capability, granted.Presentation, granted.Request) != granted.Hash {
		return Run{}, ErrApprovalHash
	}
	req := granted.Request
	if _, ok := d.tools[req.Spec.Name]; !ok {
		return Run{}, fmt.Errorf("agentrt: approved tool %q is not registered", req.Spec.Name)
	}
	req.Spec = d.tools[req.Spec.Name].Spec()
	now := d.now()
	run.Status = StatusRunning
	prior := steps[:step.Index]
	if err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateRun(ctx, run); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: now, Type: EventRunResumed, Payload: mustJSON(map[string]any{"approval_id": granted.ID})})
	}); err != nil {
		return Run{}, err
	}
	pd, err := d.policy.Evaluate(ctx, req, RunView{Run: run, Steps: prior, Approvals: approvals})
	if err != nil {
		if ferr := d.failStep(ctx, step, nil, "policy error: "+err.Error()); ferr != nil {
			return Run{}, ferr
		}
		return d.finish(ctx, run, StatusFailed, ReasonInternalError, "policy: "+err.Error(), nil, false)
	}
	switch pd.Outcome {
	case Allow:
	case RequireApproval:
		if approvalHash(pd.Kind, pd.Capability, pd.Presentation, req) != granted.Hash {
			// The policy now wants something different: pause again.
			return d.pause(ctx, run, *step, req, pd)
		}
	case Deny:
		obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "denied on resume: "+pd.Reason)
		if err := d.failStep(ctx, step, &obs, obs.Summary); err != nil {
			return Run{}, err
		}
		return d.loop(ctx, run.ID)
	case Abort:
		obs := observation(ObservePolicyDenied, map[string]any{"tool": req.Spec.Name, "reason": pd.Reason}, "aborted on resume: "+pd.Reason)
		if err := d.failStep(ctx, step, &obs, obs.Summary); err != nil {
			return Run{}, err
		}
		return d.finish(ctx, run, StatusFailed, ReasonPolicyAbort, pd.Reason, nil, false)
	}
	step.Status = StepExecuting
	if err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateStep(ctx, *step); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, StepID: step.ID, At: d.now(), Type: EventStepToolStarted, Payload: mustJSON(map[string]any{"tool": req.Spec.Name, "args": req.Args, "approval_id": granted.ID})})
	}); err != nil {
		return Run{}, err
	}
	if fin, r, err := d.runTool(ctx, run, step, req); err != nil || fin {
		return r, err
	}
	return d.loop(ctx, run.ID)
}

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
	for i := range steps {
		st := &steps[i]
		if st.Status == StepDeciding || st.Status == StepExecuting {
			obs := observation(ObserveInterrupted, map[string]any{"previous_status": st.Status}, "step interrupted while "+string(st.Status))
			st.Status, st.Observation, st.FinishedAt = StepInterrupted, &obs, now
			if err := d.store.tx(ctx, d.observer, func(t *txn) error {
				if err := t.updateStep(ctx, *st); err != nil {
					return err
				}
				return t.appendEvent(ctx, Event{RunID: run.ID, StepID: st.ID, At: now, Type: EventStepInterrupted, Payload: mustJSON(map[string]any{"previous_status": obs.Content})})
			}); err != nil {
				return Run{}, err
			}
		}
	}
	if d.reconcile != nil {
		if err := d.reconcile(ctx, RunView{Run: run, Steps: steps, Approvals: approvals}); err != nil {
			return d.finish(ctx, run, StatusFailed, ReasonInternalError, "reconcile: "+err.Error(), nil, false)
		}
	}
	run.Status = StatusRunning
	if err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateRun(ctx, run); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunResumed, Payload: mustJSON(map[string]any{"after": "interruption"})})
	}); err != nil {
		return Run{}, err
	}
	return d.loop(ctx, run.ID)
}

func (d *Driver) finish(ctx context.Context, run Run, status RunStatus, reason TerminalReason, detail string, result json.RawMessage, limit bool) (Run, error) {
	now := d.now()
	run.Status = status
	run.Reason = reason
	run.ReasonDetail = detail
	run.FinishedAt = now
	run.Result = result
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		if err := t.updateRun(ctx, run); err != nil {
			return err
		}
		if limit {
			if err := t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventLimitExceeded, Payload: mustJSON(map[string]any{"reason": reason, "detail": detail})}); err != nil {
				return err
			}
		}
		return t.appendEvent(ctx, Event{RunID: run.ID, At: now, Type: EventRunFinished, Payload: mustJSON(map[string]any{"status": status, "reason": reason, "detail": detail, "steps": run.StepCount})})
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}
