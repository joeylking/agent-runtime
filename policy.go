package agentrt

import (
	"context"
	"encoding/json"
	"fmt"
)

// PolicyFunc adapts a function to the Policy interface.
type PolicyFunc func(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error)

// Evaluate implements Policy.
func (f PolicyFunc) Evaluate(ctx context.Context, req ToolRequest, view RunView) (PolicyDecision, error) {
	return f(ctx, req, view)
}

// SideEffectPolicy maps each side-effect class to an outcome. It is the
// runtime's only built-in policy; consumers wrap or replace it.
type SideEffectPolicy map[SideEffect]PolicyOutcome

// DefaultPolicy allows reads and local mutations, requires approval for
// remote mutations, and denies destructive tools.
func DefaultPolicy() SideEffectPolicy {
	return SideEffectPolicy{
		ReadOnly:       Allow,
		LocalMutation:  Allow,
		RemoteMutation: RequireApproval,
		Destructive:    Deny,
	}
}

// Evaluate implements Policy. A side-effect class with no mapping is denied.
func (p SideEffectPolicy) Evaluate(_ context.Context, req ToolRequest, _ RunView) (PolicyDecision, error) {
	outcome, ok := p[req.Spec.SideEffect]
	if !ok {
		return PolicyDecision{Outcome: Deny, Reason: fmt.Sprintf("no policy for side effect %q", req.Spec.SideEffect)}, nil
	}
	d := PolicyDecision{Outcome: outcome, Reason: fmt.Sprintf("side effect %s is %s by policy", req.Spec.SideEffect, outcome)}
	if outcome == RequireApproval {
		d.Kind = string(req.Spec.SideEffect)
		d.Capability = mustJSON(map[string]any{"tool": req.Spec.Name, "args": json.RawMessage(orEmptyObject(req.Args))})
		d.Presentation = d.Capability
	}
	return d, nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
