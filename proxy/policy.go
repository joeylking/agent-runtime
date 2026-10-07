package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// decider is the proxy's policy as configured: an outcome and a reason for
// one tool. It is the seam where another evaluator could stand later; the
// gate's Policy around it builds the approvals, which stay the proxy's.
type decider interface {
	decide(t *tool) (agentrt.PolicyOutcome, string)
}

// classPolicy decides by the operator's rule for the tool when it names an
// outcome, and by the tool's side-effect class otherwise. A class with no
// outcome is denied.
type classPolicy map[agentrt.SideEffect]agentrt.PolicyOutcome

func (c classPolicy) decide(t *tool) (agentrt.PolicyOutcome, string) {
	if t.outcome != "" {
		return t.outcome, fmt.Sprintf("tool %s is %s by the operator's rule", t.name, t.outcome)
	}
	o, ok := c[t.class]
	if !ok {
		return agentrt.Deny, fmt.Sprintf("no policy for side effect %q", t.class)
	}
	return o, fmt.Sprintf("side effect %s is %s by policy", t.class, o)
}

// gatePolicy is the agentrt.Policy the gate evaluates, at a call and again
// when an approved call is collected. It is an agentrt.IdentifiedPolicy:
// its identity is the configuration's PolicyID, recorded with each
// decision.
type gatePolicy struct{ p *Proxy }

// PolicyID implements agentrt.IdentifiedPolicy.
func (g gatePolicy) PolicyID() string { return g.p.policyID }

// Evaluate implements agentrt.Policy. A denial is a denial. A request that
// re-runs an attempt whose outcome is unknown is asked for as an
// interruption whatever the policy's outcome short of a denial, so an
// operator decides it even for a tool the policy allows: one whose step
// paused on such an approval, which is asked for again as the same
// interruption so its grant matches, or one the proxy proposes after an
// unknown outcome. Any other request is allowed or asked for as itself.
func (g gatePolicy) Evaluate(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
	t, ok := g.p.tools[req.Spec.Name]
	if !ok {
		return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: fmt.Sprintf("tool %q is not offered", req.Spec.Name)}, nil
	}
	outcome, reason := g.p.decider.decide(t)
	if outcome == agentrt.Deny {
		return agentrt.PolicyDecision{Outcome: outcome, Reason: reason}, nil
	}
	at, err := g.p.rerunOf(ctx, req, view)
	if err != nil {
		return agentrt.PolicyDecision{}, err
	}
	if at != nil {
		return interruptedDecision(t, req, *at)
	}
	switch outcome {
	case agentrt.Allow:
		return agentrt.PolicyDecision{Outcome: outcome, Reason: reason}, nil
	case agentrt.RequireApproval:
		return approvalDecision(t, req, reason)
	}
	return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: fmt.Sprintf("unknown outcome %q", outcome)}, nil
}

// rerunOf is the earlier attempt a request re-runs, or nil. A step whose
// latest approval is an interruption re-runs the attempt that approval
// names: it is a fact the proxy recorded when it asked, and the rest of
// the capability is computed again, so a changed server, tool, class,
// argument, or fixed value still asks anew. Otherwise it is the attempt
// the proxy named for the run as it proposed the request (Proxy.rerun),
// or, in a re-check, the one the record names (recordedRerun).
func (p *Proxy) rerunOf(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (*attempt, error) {
	if a := latestFor(view.Approvals, req.StepID); a != nil && a.Kind == agentrt.InterruptedSideEffect {
		var c capability
		if json.Unmarshal(a.Capability, &c) == nil && c.Interrupted != nil {
			return c.Interrupted, nil
		}
		// Not one the proxy asked for: the step itself was cut off.
		started, err := p.stepStarted(ctx, req.RunID, req.StepID)
		if err != nil {
			return nil, err
		}
		return &attempt{RunID: req.RunID, StepID: req.StepID, StartedAt: stamp3339(started), Ended: endedCutOff}, nil
	}
	p.reruns.Lock()
	at := p.reruns.m[req.RunID]
	p.reruns.Unlock()
	if at == nil && p.recorded {
		return p.recordedRerun(ctx, req, view)
	}
	return at, nil
}

// recordedRerun is, when a recorded run is re-checked, the attempt a
// request the proxy proposed re-ran. The proxy named it only in memory as
// it proposed the request (setRerun), and recorded it in the interruption
// approval the policy then asked for: that approval is the run's next
// after those the policy is shown, of the request's step, and names an
// attempt other than the step itself, which a reconciliation's pause of
// the step would name. Nil when there is none, as for a request the
// policy denied, whose re-check cannot tell it was a re-run.
func (p *Proxy) recordedRerun(ctx context.Context, req agentrt.ToolRequest, view agentrt.RunView) (*attempt, error) {
	next, _, err := p.store.ListApprovalsPage(ctx, req.RunID, "", 1, len(view.Approvals))
	if err != nil || len(next) == 0 {
		return nil, err
	}
	a := next[0]
	if a.StepID != req.StepID || a.Kind != agentrt.InterruptedSideEffect {
		return nil, nil
	}
	var c capability
	if json.Unmarshal(a.Capability, &c) != nil || c.Interrupted == nil || c.Interrupted.RunID == req.RunID && c.Interrupted.StepID == req.StepID {
		return nil, nil
	}
	return c.Interrupted, nil
}

// capability is what an operator grants: the upstream call, with the
// operator's fixed values, which the request does not carry, so a change
// to them makes the policy ask for a different approval at collection
// rather than execute under values nobody approved. Interrupted names the
// earlier attempt an interruption's approval re-runs.
type capability struct {
	Server      string                     `json:"server"`
	Tool        string                     `json:"tool"`
	Name        string                     `json:"name"`
	SideEffect  agentrt.SideEffect         `json:"side_effect"`
	Args        json.RawMessage            `json:"args"`
	Fixed       map[string]json.RawMessage `json:"fixed,omitempty"`
	Interrupted *attempt                   `json:"interrupted,omitempty"`
}

// attempt is an earlier attempt of a request whose outcome is unknown: its
// run and step, when the step started, and how it ended.
type attempt struct {
	RunID     string `json:"run_id"`
	StepID    string `json:"step_id"`
	StartedAt string `json:"started_at"`
	Ended     string `json:"ended"`
}

// How an attempt's outcome came to be unknown: cut off by a crash before
// it was recorded, timed out, or failed with no answer from the server.
const (
	endedCutOff   = "cut_off"
	endedTimeout  = "timed_out"
	endedNoAnswer = "no_answer"
)

// how is an attempt's ending as a clause, for the operator and the model.
func (a attempt) how() string {
	switch a.Ended {
	case endedTimeout:
		return "timed out before the server answered"
	case endedNoAnswer:
		return "failed without an answer from the server"
	}
	return "was cut off before its outcome was recorded"
}

func stamp3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// presentation is what the operator is shown: the capability, the
// question, the policy's reason, and for an interruption what is unknown.
type presentation struct {
	Question string `json:"question"`
	capability
	Reason string `json:"reason"`
	Notice string `json:"notice,omitempty"`
}

func capabilityOf(t *tool, req agentrt.ToolRequest) capability {
	return capability{Server: t.server, Tool: t.remote, Name: t.name, SideEffect: t.class, Args: req.Args, Fixed: t.fixed}
}

// approvalDecision asks for an operator's approval of one call, kind the
// tool's side-effect class.
func approvalDecision(t *tool, req agentrt.ToolRequest, reason string) (agentrt.PolicyDecision, error) {
	c := capabilityOf(t, req)
	return agentrt.NeedApproval(string(t.class), reason, c, presentation{
		Question: fmt.Sprintf("May the model call %s on server %s with these arguments?", t.remote, t.server), capability: c, Reason: reason,
	})
}

// interruptedReason is an interruption approval's reason, in its
// presentation as well, so the decision made when the run paused and the
// one made when it is collected hash alike.
const interruptedReason = "interrupted while executing; outcome unknown"

// interruptedDecision asks an operator whether to run a call whose earlier
// attempt's outcome is unknown again, as agentrt's own
// interrupted_side_effect approval does, with the proxy's capability and
// the earlier attempt in it.
func interruptedDecision(t *tool, req agentrt.ToolRequest, at attempt) (agentrt.PolicyDecision, error) {
	c := capabilityOf(t, req)
	c.Interrupted = &at
	return agentrt.NeedApproval(agentrt.InterruptedSideEffect, interruptedReason, c, presentation{
		Question:   fmt.Sprintf("Run %s on server %s again with these arguments?", t.remote, t.server),
		capability: c,
		Reason:     interruptedReason,
		Notice: fmt.Sprintf("An earlier attempt of this call, started at %s in run %s, %s, so whether it took effect is unknown: it may have. "+
			"Approving runs it again with these arguments once the model calls it again. Rejecting ends this request; "+
			"an identical call later asks an operator again, whatever the policy.", at.StartedAt, at.RunID, at.how()),
	})
}

// reconcile is the gate's reconciliation for a run found mid-call after a
// crash. A call to a tool that is not read-only, cut off while executing,
// pauses on interruptedDecision, which is agentrt's interrupted_side_effect
// with the proxy's capability: it runs again only if an operator approves.
// Anything else continues, and the proxy then ends the run, since nothing
// unknown is left in it.
func (p *Proxy) reconcile(_ context.Context, view agentrt.RunView) (agentrt.Reconciliation, error) {
	st := interruptedStep(view.Steps)
	if st == nil {
		return agentrt.Reconciliation{Outcome: agentrt.ReconcileContinue}, nil
	}
	t, ok := p.tools[st.Decision.Tool]
	if !ok {
		// The runtime cannot pause a call to a tool it no longer has, and
		// fails the run as a conflict.
		pd := agentrt.PolicyDecision{Outcome: agentrt.RequireApproval, Kind: agentrt.InterruptedSideEffect, Reason: interruptedReason}
		return agentrt.Reconciliation{Outcome: agentrt.ReconcileWaiting, Pause: &pd, Detail: "the tool is no longer offered"}, nil
	}
	if t.class == agentrt.ReadOnly {
		return agentrt.Reconciliation{Outcome: agentrt.ReconcileContinue}, nil
	}
	args := st.Decision.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	req := agentrt.ToolRequest{RunID: view.Run.ID, StepID: st.ID, Spec: t.spec, Args: args}
	pd, err := interruptedDecision(t, req, attempt{RunID: view.Run.ID, StepID: st.ID, StartedAt: stamp3339(st.StartedAt), Ended: endedCutOff})
	if err != nil {
		return agentrt.Reconciliation{}, err
	}
	return agentrt.Reconciliation{Outcome: agentrt.ReconcileWaiting, Pause: &pd}, nil
}

// stepStarted is when a step started, read from the store, for the policy
// at collection, which is not shown the step it decides.
func (p *Proxy) stepStarted(ctx context.Context, runID, stepID string) (time.Time, error) {
	steps, err := p.store.ListSteps(ctx, runID)
	if err != nil {
		return time.Time{}, err
	}
	for _, st := range steps {
		if st.ID == stepID {
			return st.StartedAt, nil
		}
	}
	return time.Time{}, fmt.Errorf("proxy: run %s has no step %s", runID, stepID)
}

// interruptedStep is the step agentrt marked interrupted while it was
// executing a tool call, if any: its observation says so.
func interruptedStep(steps []agentrt.Step) *agentrt.Step {
	for i := len(steps) - 1; i >= 0; i-- {
		if interruptedExecuting(steps[i]) {
			return &steps[i]
		}
	}
	return nil
}

func interruptedExecuting(st agentrt.Step) bool {
	prev, ok := interruptedFrom(st)
	return ok && prev == agentrt.StepExecuting
}

// interruptedBeforeExecuting reports a tool step agentrt marked
// interrupted before its tool started, while deciding or allowed: nothing
// ran.
func interruptedBeforeExecuting(st agentrt.Step) bool {
	prev, ok := interruptedFrom(st)
	return ok && prev != "" && prev != agentrt.StepExecuting
}

// interruptedFrom is the status a tool step had when agentrt marked it
// interrupted, from its observation.
func interruptedFrom(st agentrt.Step) (agentrt.StepStatus, bool) {
	if st.Status != agentrt.StepInterrupted || st.Observation == nil || st.Observation.Kind != agentrt.ObserveInterrupted ||
		st.Decision == nil || st.Decision.Kind != agentrt.DecideToolCall {
		return "", false
	}
	var prev struct {
		PreviousStatus agentrt.StepStatus `json:"previous_status"`
	}
	if json.Unmarshal(st.Observation.Content, &prev) != nil {
		return "", false
	}
	return prev.PreviousStatus, true
}

// latestFor is a step's latest approval in insertion order, the only one
// that can be a grant.
func latestFor(approvals []agentrt.Approval, stepID string) *agentrt.Approval {
	var out *agentrt.Approval
	for i := range approvals {
		if approvals[i].StepID == stepID {
			out = &approvals[i]
		}
	}
	return out
}
