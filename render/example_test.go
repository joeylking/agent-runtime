package render_test

import (
	"encoding/json"
	"fmt"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/render"
)

// Messages renders the opening message and then, per recorded step, the
// assistant turn that asked for a tool and the user turn that answers it.
func ExampleMessages() {
	step := func(index int, tool, args, reason, content, summary string) agentrt.Step {
		return agentrt.Step{
			ID: fmt.Sprintf("s%d", index), Index: index, Status: agentrt.StepDone,
			Decision:    &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tool, Args: json.RawMessage(args), Reason: reason},
			Observation: &agentrt.Observation{Kind: agentrt.ObserveToolResult, Content: json.RawMessage(content), Summary: summary},
		}
	}
	in := agentrt.StepInput{
		Run: agentrt.Run{ID: "r1", Goal: "bring the counter to 5"},
		Steps: []agentrt.Step{
			step(0, "read", `{}`, "look first", `{"value":1}`, "value=1"),
			step(1, "add", `{"n":4}`, "add four", `{"value":5}`, "value=5"),
		},
	}
	msgs := render.Messages(in, render.Options{
		Opening: func(agentrt.StepInput) string { return "Bring the counter to 5." },
	})
	for _, m := range msgs {
		kinds := make([]string, 0, len(m.Content))
		for _, b := range m.Content {
			kinds = append(kinds, b.Type)
		}
		fmt.Printf("%s: %s\n", m.Role, strings.Join(kinds, " "))
	}
	// Output:
	// user: text
	// assistant: text tool_use
	// user: tool_result
	// assistant: text tool_use
	// user: tool_result
}

// A reply with no tool use is not a decision the runtime can act on, so Decide
// records the deliberately invalid kind no_tool_call. The driver observes an
// invalid_decision and the next Messages nudges the model from that kind.
func ExampleDecide() {
	dec := render.Decide(agentrt.ModelResponse{
		Text:       "The counter looks right to me.",
		StopReason: "end_turn",
	})
	fmt.Println(dec.Kind)
	fmt.Println(dec.Reason)
	// Output:
	// no_tool_call
	// The counter looks right to me.
}
