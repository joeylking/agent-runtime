package agentrt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

var running = []RunStatus{StatusRunning}

// loop executes steps until the session ends: the run leaves RUNNING,
// pauses, or the lease is lost. Each step is the agent's decision passed
// through the session, which a Gate hands to a loop the runtime does not
// own, so the two run one sequence.
func (d *Driver) loop(ctx context.Context, s *Session) (Run, error) {
	for !s.ended {
		st := s.step(ctx)
		if st == nil {
			break
		}
		in := StepInput{Run: st.run, Tools: slices.Clone(d.specs), Model: st.mc}
		in.Steps, in.Approvals = s.c.agent.view(s.c, st.n)
		decision, err := d.agent.Decide(ctx, in)
		if err != nil {
			st.fail(ctx, err)
			break
		}
		if v := st.propose(ctx, decision); v.Outcome == VerdictAllowed {
			st.execute(ctx)
		}
	}
	return s.out, s.err
}

// step checks the limits and starts the next step. It reads the run at
// every step, because model calls and an operator change it, and loads
// the run's steps and approvals once, keeping them as it writes. It
// returns nil when the session has ended: the run left RUNNING, a limit
// ended it, or the lease is lost.
func (s *Session) step(ctx context.Context) *OpenStep {
	d, runID := s.d, s.runID
	run, err := d.store.GetRun(ctx, runID)
	if err != nil {
		s.end(Run{}, err)
		return nil
	}
	if run.Status != StatusRunning {
		s.end(run, nil)
		return nil
	}
	if !s.l.ok() {
		s.end(d.lost(ctx, runID, s.l.err()))
		return nil
	}
	if s.c == nil || s.c.stale || d.reload {
		if s.c, err = d.load(ctx, runID); err != nil {
			s.end(Run{}, err)
			return nil
		}
		if derr := decodeErrors(s.c.steps, s.c.approvals); derr != nil {
			// A record that cannot be read is not continued from.
			s.end(d.failInternal(ctx, runID, running, derr))
			return nil
		}
	}
	c := s.c
	n := len(c.steps)
	steps := c.steps[:n:n]
	settle := func(e ending) *OpenStep {
		s.end(d.settle(ctx, c, runID, running, e))
		return nil
	}

	// Limits are checked before a step is started.
	if run.StepCount >= run.Limits.MaxSteps {
		return settle(ending{status: StatusFailed, reason: ReasonLimitSteps,
			detail: fmt.Sprintf("step limit %d reached", run.Limits.MaxSteps), limit: true})
	}
	if n := consecutiveFailures(steps); n >= run.Limits.MaxConsecutiveToolFailures {
		return settle(ending{status: StatusFailed, reason: ReasonRepeatedToolFailures,
			detail: fmt.Sprintf("%d consecutive failed steps", n), limit: true})
	}
	if l := run.Limits; l.MaxActiveTime > 0 && run.ActiveTime >= l.MaxActiveTime {
		return settle(ending{status: StatusFailed, reason: ReasonLimitActiveTime,
			detail: fmt.Sprintf("active time %s reached limit %s", run.ActiveTime.Round(time.Millisecond), l.MaxActiveTime), limit: true})
	}
	if l := run.Limits; l.MaxElapsedTime > 0 && d.now().Sub(run.CreatedAt) >= l.MaxElapsedTime {
		return settle(ending{status: StatusFailed, reason: ReasonLimitElapsedTime,
			detail: fmt.Sprintf("elapsed time reached limit %s", l.MaxElapsedTime), limit: true})
	}
	if n, sig := repeatedOutcome(steps); n >= run.Limits.LoopThreshold {
		at := d.now()
		return settle(ending{status: StatusFailed, reason: ReasonLoopDetected,
			detail: fmt.Sprintf("%s produced the same observation %d times in a row", sig.tool, n),
			pre: func(t *txn) error {
				return t.emit(ctx, Event{RunID: runID, At: at, Type: EventLoopDetected}, map[string]any{"repeats": n, "tool": sig.tool, "args_hash": sig.args, "observation_hash": sig.obs})
			}})
	}

	step, err := d.startStep(ctx, c, run)
	if err != nil {
		s.end(d.lost(ctx, runID, err))
		return nil
	}
	run.StepCount++
	s.run = run

	st := &OpenStep{s: s, step: step, run: run, n: n}
	if d.model != nil {
		st.mc = &caller{d: d, cfg: *d.model, runID: run.ID, stepID: step.ID, started: step.StartedAt, lease: s.l}
	}
	s.open = st
	return st
}

// fail ends a step whose decision could not be made: an agent's Decide,
// or the model calls behind a Gate caller's decision, returned err. A
// cancelled ctx leaves the step in flight, and Resume treats it as
// interrupted; a lost lease leaves the run to its next owner. A limit the
// step's ModelCaller enforced ends the run with its reason, an unavailable
// model as model_unavailable, and anything else as agent_error.
func (st *OpenStep) fail(ctx context.Context, err error) {
	s, d := st.s, st.s.d
	if cerr := ctx.Err(); cerr != nil {
		// Cancelled while deciding: the step stays in flight and
		// Resume treats it as interrupted.
		s.end(Run{}, cerr)
		return
	}
	if errors.Is(err, ErrLeaseLost) || !s.l.ok() {
		// The run is no longer this loop's to fail.
		s.end(d.lost(ctx, s.runID, s.l.err()))
		return
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
	s.end(d.settle(ctx, s.c, s.runID, running, ending{status: StatusFailed, reason: reason, detail: err.Error(), limit: reason != ReasonAgentError,
		step: &st.step, stepStatus: StepFailed, stepDetail: "agent error: " + err.Error()}))
}

// propose records a decision, validates it, and evaluates policy on a tool
// call, acting on every outcome up to, and not including, running the
// tool: an Allowed verdict leaves the step for execute.
func (st *OpenStep) propose(ctx context.Context, decision Decision) Verdict {
	s, d := st.s, st.s.d
	st.proposed = true
	// The decision is recorded verbatim before validation so the audit log
	// shows what the agent asked for even when the request was invalid.
	// JSON that cannot be stored as JSON is kept as a string beside it.
	decision = blankAsAbsent(decision)
	recorded := recordable(decision)
	st.step.Decision = &recorded
	if t, ok := d.tools[decision.Tool]; ok && decision.Kind == DecideToolCall {
		// The spec is read once, recorded by its hash with the decision,
		// and is the one the policy is handed.
		st.toolSpec = t.Spec()
		spec, err := d.recordedSpec(st.toolSpec)
		if err != nil {
			return s.lose(ctx, err)
		}
		st.spec, st.step.SpecHash = spec, spec.hash
	}
	if err := d.recordDecision(ctx, s.c, &st.step, st.spec); err != nil {
		return s.lose(ctx, err)
	}

	if verr := d.validateDecision(decision); verr != nil {
		obs := observation(ObserveInvalidDecision, map[string]string{"error": verr.Error()}, "invalid decision: "+verr.Error())
		if err := d.endStep(ctx, s.c, &st.step, StepFailed, &obs, verr.Error()); err != nil {
			return s.lose(ctx, err)
		}
		s.open = nil
		return Verdict{Outcome: VerdictDenied, Observation: &obs}
	}

	switch decision.Kind {
	case DecideComplete:
		return s.settle(ctx, running, ending{status: StatusCompleted, reason: ReasonGoalCompleted, result: decision.Result,
			step: &st.step, stepStatus: StepDone})
	case DecideFail:
		return s.settle(ctx, running, ending{status: StatusFailed, reason: ReasonGoalFailed, detail: decision.Message,
			step: &st.step, stepStatus: StepDone})
	}

	// tool_call
	req := ToolRequest{RunID: st.run.ID, StepID: st.step.ID, Spec: st.toolSpec, Args: orEmptyObject(decision.Args)}
	view := RunView{Run: st.run}
	view.Steps, view.Approvals = s.c.policy.view(s.c, st.n)
	st.view = seenBy(view)
	pd, perr := d.policy.Evaluate(ctx, req, view)
	return st.apply(ctx, req, pd, perr, nil, time.Time{})
}

// lost is how the loop answers a write that failed or a lease it no longer
// holds. When an operator cancelled the run under it, the loop stops and
// returns the run as it now is. Otherwise only another owner can have
// moved the run, so the loop has lost its lease and says so. A value that
// would not encode fails the run as internal_error; any other failure is
// returned as it is.
func (d *Driver) lost(ctx context.Context, runID string, err error) (Run, error) {
	if errors.Is(err, errEncode) {
		return d.failInternal(ctx, runID, running, err)
	}
	if !errors.Is(err, ErrRunState) && !errors.Is(err, ErrLeaseLost) {
		return Run{}, err
	}
	rctx, cancel := afterEffect(ctx)
	defer cancel()
	fresh, gerr := d.store.GetRun(rctx, runID)
	if gerr != nil {
		return Run{}, err
	}
	if fresh.Status.Terminal() && fresh.Reason == ReasonOperatorCancelled {
		return fresh, nil
	}
	if errors.Is(err, ErrLeaseLost) {
		return Run{}, err
	}
	return Run{}, fmt.Errorf("%w: %w", ErrLeaseLost, err)
}

// blankAsAbsent treats arguments or a result that are only whitespace as
// absent, which is what validation already took them for.
func blankAsAbsent(dec Decision) Decision {
	if len(dec.Args) > 0 && len(bytes.TrimSpace(dec.Args)) == 0 {
		dec.Args = nil
	}
	if len(dec.Result) > 0 && len(bytes.TrimSpace(dec.Result)) == 0 {
		dec.Result = nil
	}
	return dec
}

// recordable returns the decision as it is stored. Arguments or a result
// that are not usable JSON, which would not survive storage as JSON, are
// moved verbatim to InvalidArgs or InvalidResult, base64 when they are not
// UTF-8, and Args or Result left empty, so the record of a decision that
// could not be parsed never looks like one that could; validation then
// rejects the decision with the original bytes. The Invalid fields are
// the runtime's, so whatever an agent put there is dropped.
func recordable(dec Decision) Decision {
	dec.InvalidArgs, dec.InvalidResult, dec.InvalidBase64 = "", "", false
	badArgs := len(dec.Args) > 0 && checkJSON(dec.Args) != nil
	badResult := len(dec.Result) > 0 && checkJSON(dec.Result) != nil
	if !badArgs && !badResult {
		return dec
	}
	dec.InvalidBase64 = badArgs && !utf8.Valid(dec.Args) || badResult && !utf8.Valid(dec.Result)
	keep := func(raw []byte) string {
		if dec.InvalidBase64 {
			return base64.StdEncoding.EncodeToString(raw)
		}
		return string(raw)
	}
	if badArgs {
		dec.InvalidArgs, dec.Args = keep(dec.Args), nil
	}
	if badResult {
		dec.InvalidResult, dec.Result = keep(dec.Result), nil
	}
	return dec
}

// rawField stores bytes that are not usable JSON in m under key, as a
// string, or base64 under key+"_base64" when they are not UTF-8.
func rawField(key string, raw []byte, m map[string]string) map[string]string {
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
	if !dec.Origin.valid() {
		return fmt.Errorf("unknown decision origin %q", dec.Origin)
	}
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
		return checkArgs(cs, dec.Args)
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
// the granted approval is allowed. The policy decision is recorded on the
// step, with a step.policy event, in the same transaction as what it
// caused; for an allowed request that is the tool start, which execute
// writes, so apply writes nothing for it and the move to RUNNING, the
// policy decision, and the tool start still commit together on resume.
//
// Each write takes its times from the clock in the order and number the
// runtime always has, one reading per recorded state change, because a
// consumer with a deterministic clock keys its own records on that
// sequence. resumedAt is the reading Resume took for run.resumed.
//
// The session's lease is the one the caller holds or, on resume, takes
// with the move to RUNNING.
func (st *OpenStep) apply(ctx context.Context, req ToolRequest, pd PolicyDecision, perr error, granted *Approval, resumedAt time.Time) Verdict {
	s, d := st.s, st.s.d
	c, run, step := s.c, st.run, &st.step
	st.granted, st.resumedAt = granted, resumedAt
	step.PolicyID = d.policyID
	from, suffix := running, ""
	if granted != nil {
		from, suffix = waiting, " on resume"
	}
	settle := func(e ending) Verdict {
		r, err := d.settle(ctx, c, run.ID, from, e)
		if err != nil && granted == nil {
			return st.lose(ctx, err)
		}
		s.end(r, err)
		return s.ending()
	}
	if perr != nil {
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: "policy: " + perr.Error(),
			step: step, stepStatus: StepFailed, stepDetail: "policy error: " + perr.Error()})
	}
	st.policyAt = resumedAt
	if granted == nil {
		st.policyAt = d.now()
	}
	policyEvent := st.policyEvent(ctx)
	if cerr := checkPolicy(pd); cerr != nil {
		step.Policy = &PolicyDecision{Outcome: pd.Outcome, Reason: pd.Reason, Kind: pd.Kind}
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: "policy: " + cerr.Error(), pre: policyEvent,
			step: step, stepStatus: StepFailed, stepDetail: "policy error: " + cerr.Error()})
	}
	step.Policy = &pd

	allowed := pd.Outcome == Allow
	if pd.Outcome == RequireApproval && granted != nil {
		h, err := approvalHash(pd.Kind, pd.Capability, pd.Presentation, req)
		if err != nil {
			return st.lose(ctx, err)
		}
		allowed = h == granted.Hash
	}
	switch {
	case allowed:
		// Nothing is written until execute starts the tool. The verdict
		// carries a copy: what the caller does with it cannot change the
		// request that runs.
		st.allowed, st.req = true, req
		shown := req
		shown.Args, shown.Spec.InputSchema = cloneBytes(req.Args), cloneBytes(req.Spec.InputSchema)
		return Verdict{Outcome: VerdictAllowed, Request: &shown}
	case pd.Outcome == Deny:
		obs := observation(ObservePolicyDenied, map[string]string{"tool": req.Spec.Name, "reason": pd.Reason}, "denied"+suffix+": "+pd.Reason)
		stepAt := d.now()
		at := d.wall()
		if err := d.write(ctx, c, func(t *txn) error {
			if err := d.enter(ctx, t, run, granted, resumedAt); err != nil {
				return err
			}
			if err := policyEvent(t); err != nil {
				return err
			}
			return d.endStepTx(ctx, t, step, StepFailed, &obs, obs.Summary, stepAt)
		}); err != nil {
			return st.lose(ctx, err)
		}
		s.l.start(at)
		s.open = nil
		return Verdict{Outcome: VerdictDenied, Observation: &obs}
	case pd.Outcome == Abort:
		obs := observation(ObservePolicyDenied, map[string]string{"tool": req.Spec.Name, "reason": pd.Reason}, "aborted"+suffix+": "+pd.Reason)
		return settle(ending{status: StatusFailed, reason: ReasonPolicyAbort, detail: pd.Reason, pre: policyEvent,
			step: step, stepStatus: StepFailed, obs: &obs, stepDetail: obs.Summary})
	case pd.Outcome == RequireApproval:
		// In the loop this is the first request; on resume the policy now
		// wants something other than what was granted, so the run pauses
		// again.
		r, a, err := d.pause(ctx, c, run.ID, step, req, pd, from, st.policyAt, st.spec, st.view)
		if err != nil {
			return st.lose(ctx, err)
		}
		s.approval = &a
		s.end(r, nil)
		return s.ending()
	default:
		return settle(ending{status: StatusFailed, reason: ReasonInternalError, detail: fmt.Sprintf("unknown policy outcome %q", pd.Outcome), pre: policyEvent,
			step: step, stepStatus: StepFailed, stepDetail: "unknown policy outcome"})
	}
}

// policyEvent appends the step's policy decision as step.policy, at the
// reading apply took for it, with what the policy was handed.
func (st *OpenStep) policyEvent(ctx context.Context) func(t *txn) error {
	return func(t *txn) error {
		return t.policyEvent(ctx, &st.step, st.policyAt, st.spec, st.view)
	}
}

// lose answers a write of the step that failed. The first write of a
// resume decides the race between two resumers, so its loser gets
// ErrRunState; in the loop it is the session's lose.
func (st *OpenStep) lose(ctx context.Context, err error) Verdict {
	s := st.s
	if st.granted == nil {
		return s.lose(ctx, err)
	}
	if errors.Is(err, errEncode) {
		s.end(s.d.failInternal(ctx, s.runID, waiting, err))
	} else {
		s.end(Run{}, err)
	}
	return s.ending()
}

// execute starts the request an Allowed verdict let through and records
// what it did. The policy decision and step.tool_started commit with the
// step's move to executing and, on resume, with the run's move to
// RUNNING, which takes the lease. In the loop a tool is not started once
// the lease is lost, and the write that starts it requires the lease
// unexpired.
func (st *OpenStep) execute(ctx context.Context) Observation {
	s, d := st.s, st.s.d
	c, l, run, step, req, granted := s.c, s.l, st.run, &st.step, st.req, st.granted
	st.executed = true
	startAt := st.policyAt
	if granted == nil && !l.ok() {
		// The tool is not started: the step stays in flight for the
		// next owner, who finds it was never executed.
		st.lose(ctx, l.err())
		return Observation{}
	}
	fromStep := step.Status
	step.Status = StepExecuting
	if granted != nil {
		// The step's clock restarts here: the wait for the human is not
		// active time.
		startAt = d.now()
		step.StartedAt = startAt
	}
	policyEvent := st.policyEvent(ctx)
	at := d.wall()
	if err := d.write(ctx, c, func(t *txn) error {
		// In the loop the lease must be unexpired as well as held: a
		// side effect starts only inside a live lease.
		if granted == nil {
			if err := t.requireLease(ctx, run.ID); err != nil {
				return err
			}
		} else if err := d.enter(ctx, t, run, granted, st.resumedAt); err != nil {
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
		return t.emit(ctx, Event{RunID: run.ID, StepID: step.ID, At: startAt, Type: EventStepToolStarted}, payload)
	}); err != nil {
		st.lose(ctx, err)
		return Observation{}
	}
	l.start(at)
	s.open = nil
	obs, finished, out, err := d.runTool(ctx, c, run, step, req)
	if err != nil || finished {
		s.end(out, err)
	}
	return obs
}

// enter checks inside a transaction that the run may take the step's next
// write: in the loop it must still be RUNNING under this driver's lease; on
// resume it moves from WAITING to RUNNING here, taking the lease, with
// run.resumed, so two resumers cannot both pass.
func (d *Driver) enter(ctx context.Context, t *txn, run Run, granted *Approval, now time.Time) error {
	if granted == nil {
		return t.requireStatus(ctx, run.ID, StatusRunning)
	}
	run.Status = StatusRunning
	if err := t.transition(ctx, run, StatusWaitingForApproval); err != nil {
		return err
	}
	return t.emit(ctx, Event{RunID: run.ID, StepID: granted.StepID, At: now, Type: EventRunResumed}, map[string]any{"approval_id": granted.ID})
}

// runTool executes a request whose step is already recorded as executing,
// and records the outcome, which it returns. finished=true comes with the
// final run when the tool ended the run, either as a terminal tool or by
// aborting.
// The tool has had its effect by the time it returns, so the outcome is
// recorded even when ctx was cancelled meanwhile; the cancellation is then
// returned.
func (d *Driver) runTool(ctx context.Context, c *runCache, run Run, step *Step, req ToolRequest) (Observation, bool, Run, error) {
	obs, abort := d.execute(ctx, req)
	rctx, cancel := afterEffect(ctx)
	defer cancel()
	var out Run
	var err error
	finished := true
	switch {
	case abort != nil:
		out, err = d.settle(rctx, c, run.ID, running, ending{status: StatusFailed, reason: ReasonToolAbort, detail: abort.Detail,
			step: step, stepStatus: StepFailed, obs: &obs, stepDetail: obs.Summary})
	case obs.Failure():
		finished = false
		err = d.endStep(rctx, c, step, StepFailed, &obs, obs.Summary)
	case req.Spec.Terminal:
		out, err = d.settle(rctx, c, run.ID, running, ending{status: StatusCompleted, reason: ReasonGoalCompleted, detail: "terminal tool " + req.Spec.Name, result: obs.Content,
			step: step, stepStatus: StepDone, obs: &obs})
	default:
		finished = false
		err = d.endStep(rctx, c, step, StepDone, &obs, "")
	}
	if err != nil {
		if errors.Is(err, ErrRunState) {
			// An operator's Cancel while the tool ran: its outcome still
			// goes on the record, as a late event.
			d.recordLate(rctx, step, obs)
		}
		r, err := d.lost(ctx, run.ID, err)
		return obs, true, r, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return obs, true, out, cerr
	}
	return obs, finished, out, nil
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
			obs = observation(ObserveToolError, map[string]string{"tool": req.Spec.Name, "error": fmt.Sprint("panic: ", r)}, fmt.Sprintf("%s panicked", req.Spec.Name))
			abort = nil
		}
	}()
	res, err := tool.Call(tctx, ToolCall{RunID: req.RunID, StepID: req.StepID, Args: req.Args})
	if err != nil {
		var ab ErrAbortRun
		if errors.As(err, &ab) {
			return observation(ObserveToolError, map[string]string{"tool": req.Spec.Name, "error": ab.Detail, "failure": "abort"}, fmt.Sprintf("%s aborted the run: %s", req.Spec.Name, ab.Detail)), &ab
		}
		if ctx.Err() != nil {
			return observation(ObserveInterrupted, map[string]string{"tool": req.Spec.Name, "error": err.Error(), "failure": "cancelled"}, fmt.Sprintf("%s was cancelled and its outcome is unknown: %s", req.Spec.Name, err.Error())), nil
		}
		kind := "error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(tctx.Err(), context.DeadlineExceeded) {
			kind = "timeout"
		}
		return observation(ObserveToolError, map[string]string{"tool": req.Spec.Name, "error": err.Error(), "failure": kind}, fmt.Sprintf("%s %s: %s", req.Spec.Name, kind, err.Error())), nil
	}
	content := orEmptyObject(res.Content)
	if cerr := checkJSON(content); cerr != nil {
		msg := "content is not usable JSON: " + cerr.Error()
		return observation(ObserveToolError, rawField("content", content, map[string]string{"tool": req.Spec.Name, "error": msg, "failure": "invalid_content"}), fmt.Sprintf("%s returned %s", req.Spec.Name, msg)), nil
	}
	return Observation{Kind: ObserveToolResult, Content: content, Summary: res.Summary, ContentHash: contentHash(content)}, nil
}

// observation builds a runtime observation. Its content is string fields
// only, whose encoding cannot fail, so there is no error to substitute for.
func observation(kind ObservationKind, content map[string]string, summary string) Observation {
	raw, _ := json.Marshal(content)
	return Observation{Kind: kind, Content: raw, Summary: summary, ContentHash: contentHash(raw)}
}
