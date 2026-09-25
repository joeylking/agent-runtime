package render_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/render"
)

func toolStep(index int, tool, args, reason string, obs *agentrt.Observation) agentrt.Step {
	return agentrt.Step{
		ID:          fmt.Sprintf("s%d", index),
		Index:       index,
		Status:      agentrt.StepDone,
		Decision:    &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tool, Args: json.RawMessage(args), Reason: reason},
		Observation: obs,
	}
}

func result(content, summary string) *agentrt.Observation {
	return &agentrt.Observation{Kind: agentrt.ObserveToolResult, Content: json.RawMessage(content), Summary: summary}
}

func failed(kind agentrt.ObservationKind, content, summary string) *agentrt.Observation {
	return &agentrt.Observation{Kind: kind, Content: json.RawMessage(content), Summary: summary}
}

func input(steps ...agentrt.Step) agentrt.StepInput {
	return agentrt.StepInput{Run: agentrt.Run{ID: "r1", Goal: "g", StepCount: len(steps) + 1}, Steps: steps}
}

func fixedOpening(text string) func(agentrt.StepInput) string {
	return func(agentrt.StepInput) string { return text }
}

// golden renders the messages as indented JSON so a whole sequence can be
// compared at once.
func golden(t *testing.T, msgs []agentrt.Message) string {
	t.Helper()
	b, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func requireGolden(t *testing.T, msgs []agentrt.Message, want string) {
	t.Helper()
	got := golden(t, msgs)
	if got != strings.TrimSpace(want) {
		t.Fatalf("rendered sequence:\n got:\n%s\n want:\n%s", got, strings.TrimSpace(want))
	}
}

func TestMessages_WholeSequence(t *testing.T) {
	in := input(
		toolStep(0, "read", `{"n":1}`, "look first", result(`{"value":1}`, "value=1")),
		toolStep(1, "add", `{"n":"x"}`, "add one", failed(agentrt.ObserveInvalidDecision, `{"error":"bad type"}`, "invalid decision: bad type")),
		agentrt.Step{ID: "s2", Index: 2, Status: agentrt.StepFailed, Decision: &agentrt.Decision{Kind: agentrt.KindNoToolCall, Reason: "I will now consider the options."},
			Observation: failed(agentrt.ObserveInvalidDecision, `{"error":"unknown decision kind"}`, "invalid decision: unknown decision kind \"no_tool_call\"")},
	)
	msgs := render.Messages(in, render.Options{Opening: fixedOpening("Goal: g")})
	requireGolden(t, msgs, `
[
  {
    "role": "user",
    "content": [
      {
        "type": "text",
        "text": "Goal: g"
      }
    ]
  },
  {
    "role": "assistant",
    "content": [
      {
        "type": "text",
        "text": "look first"
      },
      {
        "type": "tool_use",
        "tool_use_id": "step_0",
        "name": "read",
        "input": {
          "n": 1
        }
      }
    ]
  },
  {
    "role": "user",
    "content": [
      {
        "type": "tool_result",
        "tool_use_id": "step_0",
        "content": "{\"value\":1}"
      }
    ]
  },
  {
    "role": "assistant",
    "content": [
      {
        "type": "text",
        "text": "add one"
      },
      {
        "type": "tool_use",
        "tool_use_id": "step_1",
        "name": "add",
        "input": {
          "n": "x"
        }
      }
    ]
  },
  {
    "role": "user",
    "content": [
      {
        "type": "tool_result",
        "tool_use_id": "step_1",
        "content": "{\"error\":\"bad type\"}",
        "is_error": true
      }
    ]
  },
  {
    "role": "assistant",
    "content": [
      {
        "type": "text",
        "text": "I will now consider the options."
      }
    ]
  },
  {
    "role": "user",
    "content": [
      {
        "type": "text",
        "text": "Your reply contained no executable tool call. Respond with exactly one tool call."
      }
    ]
  }
]`)
}

func TestMessages_SkipsTheStepBeingDecided(t *testing.T) {
	in := input(
		toolStep(0, "read", `{}`, "", result(`{}`, "ok")),
		agentrt.Step{ID: "s1", Index: 1, Status: agentrt.StepDeciding},
	)
	msgs := render.Messages(in, render.Options{Opening: fixedOpening("open")})
	if len(msgs) != 3 {
		t.Fatalf("messages = %s", golden(t, msgs))
	}
}

func TestMessages_RecentWindowAndContentCap(t *testing.T) {
	var steps []agentrt.Step
	for i := 0; i < 4; i++ {
		steps = append(steps, toolStep(i, "read", `{}`, "", result(fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("read %d", i))))
	}
	// A result over the cap is summarized even inside the window.
	big := strings.Repeat("z", 40)
	steps = append(steps, toolStep(4, "read", `{}`, "", result(`"`+big+`"`, "big result")))
	msgs := render.Messages(input(steps...), render.Options{Opening: fixedOpening("open"), RecentResults: 2, MaxContentBytes: 30})
	var got []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				got = append(got, b.Content)
			}
		}
	}
	want := []string{"[summary] read 0", "[summary] read 1", "[summary] read 2", `{"i":3}`, "[summary] big result"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("results:\n got  %q\n want %q", got, want)
	}
}

func TestMessages_InterruptedResultAsksAgain(t *testing.T) {
	in := input(toolStep(0, "apply", `{}`, "", failed(agentrt.ObserveInterrupted, `{"previous_status":"executing"}`, "step interrupted while executing")))
	msgs := render.Messages(in, render.Options{Opening: fixedOpening("open")})
	b := msgs[2].Content[0]
	if !b.IsError || !strings.HasPrefix(b.Content, "The process was interrupted while this tool ran") || !strings.Contains(b.Content, "answers from its record") {
		t.Fatalf("interrupted result = %+v", b)
	}
}

func TestMessages_DeniedAndTruncatedNudges(t *testing.T) {
	denied := input(toolStep(0, "wipe", `{}`, "", failed(agentrt.ObservePolicyDenied, `{"reason":"destructive"}`, "denied: destructive")))
	if b := render.Messages(denied, render.Options{Opening: fixedOpening("open")})[2].Content[0]; b.Type != "tool_result" || !b.IsError {
		t.Fatalf("denied result = %+v", b)
	}
	truncated := input(agentrt.Step{ID: "s0", Index: 0, Status: agentrt.StepFailed,
		Decision: &agentrt.Decision{Kind: agentrt.KindTruncated, Reason: "cut off"}})
	b := render.Messages(truncated, render.Options{Opening: fixedOpening("open")})[2].Content[0]
	if b.Type != "text" || !strings.HasPrefix(b.Text, "Your reply hit the output token cap") {
		t.Fatalf("truncated nudge = %+v", b)
	}
	// A tool call whose step has no observation at all is nudged, not left
	// with an unanswered tool_use block.
	unanswered := input(agentrt.Step{ID: "s0", Index: 0, Status: agentrt.StepFailed,
		Decision: &agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: "read", Args: json.RawMessage(`{}`)}})
	if b := render.Messages(unanswered, render.Options{Opening: fixedOpening("open")})[2].Content[0]; b.Type != "text" || !strings.Contains(b.Text, "not an executable tool call") {
		t.Fatalf("unanswered = %+v", b)
	}
}

func TestMessages_HooksOverrideAndFallBack(t *testing.T) {
	in := input(
		toolStep(0, "read", `{}`, "reason text", result(`{"a":1}`, "ok")),
		toolStep(1, "read", `{}`, "reason text", result(`{"a":2}`, "ok")),
	)
	opts := render.Options{
		Opening:   fixedOpening("open"),
		ToolUseID: func(st agentrt.Step) string { return fmt.Sprintf("toolu_step_%d", st.Index) },
		Assistant: func(st agentrt.Step, id string) []agentrt.ContentBlock {
			if st.Index == 0 {
				return []agentrt.ContentBlock{{Type: "raw", Text: "verbatim"}}
			}
			return nil // fall back to the synthesized turn
		},
		Observation: func(st agentrt.Step, id string, full bool) agentrt.ContentBlock {
			if st.Index == 0 {
				return agentrt.ContentBlock{Type: "tool_result", ToolUseID: id, Name: st.Decision.Tool, Content: "[" + string(st.Observation.Kind) + "] " + st.Observation.Summary}
			}
			return agentrt.ContentBlock{} // fall back to the default
		},
	}
	msgs := render.Messages(in, opts)
	if msgs[1].Content[0].Type != "raw" {
		t.Fatalf("assistant hook ignored: %+v", msgs[1])
	}
	if b := msgs[2].Content[0]; b.ToolUseID != "toolu_step_0" || b.Name != "read" || b.Content != "[tool_result] ok" {
		t.Fatalf("observation hook ignored: %+v", b)
	}
	if b := msgs[3].Content[1]; b.Type != "tool_use" || b.ToolUseID != "toolu_step_1" {
		t.Fatalf("assistant fallback = %+v", b)
	}
	if b := msgs[4].Content[0]; b.Content != `{"a":2}` || b.Name != "" {
		t.Fatalf("observation fallback = %+v", b)
	}
}

func TestMessages_MissingOpeningPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a renderer without an opening message must not be usable")
		}
	}()
	render.Messages(input(), render.Options{})
}

func TestDecide_StopReasonsAndToolCall(t *testing.T) {
	if d := render.Decide(agentrt.ModelResponse{StopReason: "refusal", Text: "no"}); d.Kind != agentrt.DecideFail || !strings.Contains(d.Message, "refusal") {
		t.Fatalf("refusal = %+v", d)
	}
	// Both pseudo kinds are invalid decisions by design; the driver records
	// invalid_decision and Messages nudges on the next step.
	d := render.Decide(agentrt.ModelResponse{StopReason: "max_tokens", ToolUses: []agentrt.ToolUse{{ID: "t", Name: "read"}}})
	if d.Kind != agentrt.KindTruncated || d.Tool != "" {
		t.Fatalf("max_tokens = %+v, want a truncated pseudo decision that cannot execute", d)
	}
	if d := render.Decide(agentrt.ModelResponse{Text: "  thinking out loud  "}); d.Kind != agentrt.KindNoToolCall || d.Reason != "thinking out loud" {
		t.Fatalf("no tool use = %+v", d)
	}
	long := strings.Repeat("a", 600)
	d = render.Decide(agentrt.ModelResponse{Text: long, ToolUses: []agentrt.ToolUse{{ID: "t1", Name: "add", Args: nil}, {ID: "t2", Name: "wipe"}}})
	if d.Kind != agentrt.DecideToolCall || d.Tool != "add" || string(d.Args) != "{}" {
		t.Fatalf("tool call = %+v, want the first tool use with defaulted arguments", d)
	}
	if d.Reason != strings.Repeat("a", 500)+"…" {
		t.Fatalf("reason is %d bytes, want it truncated to %d", len(d.Reason), render.ReasonBytes)
	}
}

func TestTruncate_MarksTheCut(t *testing.T) {
	if got := render.Truncate("abc", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := render.Truncate("abcd", 3); got != "abc…" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncate_DoesNotSplitRunes(t *testing.T) {
	s := "abécd" // é is two bytes at offsets 2-3
	if got := render.Truncate(s, 3); got != "ab…" {
		t.Fatalf("Truncate(%q, 3) = %q, want %q", s, got, "ab…")
	}
	if got := render.Truncate(s, 4); got != "abé…" {
		t.Fatalf("Truncate(%q, 4) = %q, want %q", s, got, "abé…")
	}
	if got := render.Truncate("abc", 3); got != "abc" {
		t.Fatalf("Truncate short = %q", got)
	}
}
