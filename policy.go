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

// NeedApproval builds a require_approval decision, marshalling the
// capability an operator would be granting and the presentation they are
// shown. Both enter the approval's hash, so a changed presentation cannot
// inherit an old approval. It is named for what a policy does rather than
// for the RequireApproval outcome it carries.
func NeedApproval(kind, reason string, capability, presentation any) (PolicyDecision, error) {
	cap, err := marshalField("capability", capability)
	if err != nil {
		return PolicyDecision{}, err
	}
	pres, err := marshalField("presentation", presentation)
	if err != nil {
		return PolicyDecision{}, err
	}
	return PolicyDecision{Outcome: RequireApproval, Reason: reason, Kind: kind, Capability: cap, Presentation: pres}, nil
}

func marshalField(name string, v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("agentrt: approval %s: %w", name, err)
	}
	return b, nil
}

// DecodePresentation unmarshals an approval's presentation into T, for a
// front end that renders it or a consumer that rechecks it before resuming.
func DecodePresentation[T any](a Approval) (T, error) {
	var out T
	if len(a.Presentation) == 0 {
		return out, fmt.Errorf("agentrt: approval %s has no presentation", a.ID)
	}
	if err := json.Unmarshal(a.Presentation, &out); err != nil {
		return out, fmt.Errorf("agentrt: approval %s presentation: %w", a.ID, err)
	}
	return out, nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
