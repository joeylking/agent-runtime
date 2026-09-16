// Package scripted provides an Agent that replays a fixed list of decisions.
// It exists so the runtime can be exercised deterministically without a model.
package scripted

import (
	"context"
	"fmt"

	agentrt "github.com/joeylking/agent-runtime"
)

// Agent returns Decisions[i] for the step with index i. It returns an error
// once the script is exhausted, which the driver records as an agent error.
type Agent struct {
	Decisions []agentrt.Decision
}

// Decide implements agentrt.Agent.
func (a *Agent) Decide(_ context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	i := len(in.Steps)
	if i >= len(a.Decisions) {
		return agentrt.Decision{}, fmt.Errorf("scripted: no decision for step %d (script has %d)", i, len(a.Decisions))
	}
	return a.Decisions[i], nil
}

// ToolCall builds a tool_call decision from a JSON argument string.
func ToolCall(tool, args, reason string) agentrt.Decision {
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tool, Args: []byte(args), Reason: reason}
}

// Complete builds a complete decision.
func Complete(result string) agentrt.Decision {
	return agentrt.Decision{Kind: agentrt.DecideComplete, Result: []byte(result)}
}

// Fail builds a fail decision.
func Fail(message string) agentrt.Decision {
	return agentrt.Decision{Kind: agentrt.DecideFail, Message: message}
}
