// Package render is the generic half of a model-driven agent: it turns a
// run's recorded steps into a message list and a model response into a
// decision. What is left to the consumer is the opening message, which is
// the only place domain facts belong, and the hooks below.
//
// The rendering rules: an opening user message, then per recorded step an
// assistant turn (the decision's reason as text, plus a tool_use block with
// a synthesized id) and a user turn with the tool_result answering it.
// Results older than the recent window, and results over the content cap,
// are replaced by their summary. A step whose decision was not a tool call
// is answered with a text nudge keyed on the decision kind, so the model is
// told what went wrong rather than left to guess.
//
// Both consumers are reproduced byte for byte. casework's renderer is the
// default plus its own ids and raw assistant turns:
//
//	render.Options{
//		Opening:       func(agentrt.StepInput) string { return opening },
//		RecentResults: a.RecentResults,
//		ToolUseID: func(st agentrt.Step) string {
//			if t, ok := turns[st.Index]; ok && t.Model == a.ModelName {
//				if id := firstToolUseID(t.Raw); id != "" {
//					return id
//				}
//			}
//			return fmt.Sprintf("toolu_step_%d", st.Index)
//		},
//		Assistant: func(st agentrt.Step, _ string) []agentrt.ContentBlock {
//			if t, ok := turns[st.Index]; ok && t.Model == a.ModelName {
//				return []agentrt.ContentBlock{{Type: rawBlock, Text: string(t.Raw)}} // the adapter's RawAssistantBlock
//			}
//			return nil // synthesize
//		},
//	}
//
// repo-steward's renderer keeps the default ids and window and overrides
// both turns, because its assistant turn carries no reason text and its
// results are rendered as a labelled head plus content:
//
//	calls := func(st agentrt.Step) bool {
//		return st.Decision.Kind == agentrt.DecideToolCall && st.Decision.Tool != ""
//	}
//	render.Options{
//		Opening:       a.opening,
//		RecentResults: a.RecentResults,
//		Assistant: func(st agentrt.Step, id string) []agentrt.ContentBlock {
//			if !calls(st) {
//				return nil // the default text turn
//			}
//			return []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: id, Name: st.Decision.Tool, Input: orEmpty(st.Decision.Args)}}
//		},
//		Observation: func(st agentrt.Step, id string, full bool) agentrt.ContentBlock {
//			if !calls(st) {
//				return agentrt.ContentBlock{} // the default nudge
//			}
//			return agentrt.ContentBlock{Type: "tool_result", ToolUseID: id, Name: st.Decision.Tool,
//				Content: renderObservation(st, full), IsError: st.Observation != nil && st.Observation.Failure()}
//		},
//	}
//
// The guards matter: since Decide records KindNoToolCall and KindTruncated,
// every consumer's hooks are called for steps that called nothing, and
// falling back to the defaults there is what delivers the nudge. A hook
// that formats results its own way and wants the default for interrupted
// steps calls DefaultObservation, or uses InterruptedText directly.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// Defaults for Options.
const (
	// DefaultRecentResults is how many of the latest results are rendered
	// in full.
	DefaultRecentResults = 6
	// DefaultMaxContentBytes caps one rendered result.
	DefaultMaxContentBytes = 24000
	// ReasonBytes caps the reason Decide records from a model's text, so a
	// long narration cannot dominate the next render.
	ReasonBytes = 500
)

// InterruptedText re-requests a tool whose outcome the interruption left
// unknown. The runtime never re-executes a side effect itself, so the model
// must ask again and the tool answers from its own record. It is exported
// so an Observation hook with its own result format keeps this sentence.
const InterruptedText = "The process was interrupted while this tool ran; its outcome is unknown to you. Re-request it to establish the outcome: an already applied change answers from its record."

// Options configure Messages. Only Opening is required; every hook may be
// nil and the default applies.
type Options struct {
	// Opening renders the first user message from the run's facts. It is
	// the consumer's domain text and the only required option.
	Opening func(in agentrt.StepInput) string
	// RecentResults is how many of the latest results are rendered in full;
	// older ones are replaced by their summary. Zero means
	// DefaultRecentResults.
	RecentResults int
	// MaxContentBytes replaces a result larger than this by its summary.
	// Zero means DefaultMaxContentBytes. A custom Observation applies its
	// own cap.
	MaxContentBytes int
	// ToolUseID names a step's tool_use block. Zero means "step_<index>".
	// Hooks see only the Step, so its recorded Index is the identity to key
	// on; the store orders steps by it and it has no gaps.
	ToolUseID func(step agentrt.Step) string
	// Assistant renders a step's assistant turn. Returning nil falls back
	// to the synthesized turn, so a hook can override only the steps it
	// knows about. It is called for every recorded step, including one
	// whose decision was not a tool call.
	Assistant func(step agentrt.Step, toolUseID string) []agentrt.ContentBlock
	// Observation renders the user turn answering a step. Returning a block
	// with no type falls back to the default, which is DefaultObservation
	// with MaxContentBytes. It too is called for a step that called nothing;
	// falling back there delivers the nudge.
	Observation func(step agentrt.Step, toolUseID string, full bool) agentrt.ContentBlock
}

func (o Options) recent() int {
	if o.RecentResults <= 0 {
		return DefaultRecentResults
	}
	return o.RecentResults
}

func (o Options) maxContent() int {
	if o.MaxContentBytes <= 0 {
		return DefaultMaxContentBytes
	}
	return o.MaxContentBytes
}

func (o Options) toolUseID(st agentrt.Step) string {
	if o.ToolUseID != nil {
		return o.ToolUseID(st)
	}
	return fmt.Sprintf("step_%d", st.Index)
}

// Messages renders the conversation for one decision. Steps with no
// recorded decision, which is the step being decided and any step
// interrupted before it decided, are skipped.
func Messages(in agentrt.StepInput, opts Options) []agentrt.Message {
	if opts.Opening == nil {
		panic("render: Options.Opening is required")
	}
	msgs := []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: opts.Opening(in)}}}}
	n := len(in.Steps)
	for i, st := range in.Steps {
		if st.Decision == nil {
			continue
		}
		id := opts.toolUseID(st)
		blocks := synthesize(st, id)
		if opts.Assistant != nil {
			if custom := opts.Assistant(st, id); custom != nil {
				blocks = custom
			}
		}
		full := i >= n-opts.recent()
		block := DefaultObservation(st, id, full, opts.maxContent())
		if opts.Observation != nil {
			if custom := opts.Observation(st, id, full); custom.Type != "" {
				block = custom
			}
		}
		msgs = append(msgs,
			agentrt.Message{Role: "assistant", Content: blocks},
			agentrt.Message{Role: "user", Content: []agentrt.ContentBlock{block}})
	}
	return msgs
}

// synthesize renders a recorded decision as an assistant turn: the reason
// as text when there is one, then the tool_use block. A decision that was
// not a tool call has only its text, because there is nothing to call.
func synthesize(st agentrt.Step, toolUseID string) []agentrt.ContentBlock {
	d := st.Decision
	if d.Kind != agentrt.DecideToolCall || d.Tool == "" {
		text := d.Reason
		if text == "" {
			text = "(no tool call)"
		}
		return []agentrt.ContentBlock{{Type: "text", Text: text}}
	}
	var blocks []agentrt.ContentBlock
	if d.Reason != "" {
		blocks = append(blocks, agentrt.ContentBlock{Type: "text", Text: d.Reason})
	}
	return append(blocks, agentrt.ContentBlock{Type: "tool_use", ToolUseID: toolUseID, Name: d.Tool, Input: orEmptyObject(d.Args)})
}

// DefaultObservation renders what a step produced, or a nudge when there
// was no tool call to answer: the tool_result carries the content when the
// step is within the recent window and under maxBytes, otherwise its
// summary; an interrupted observation carries InterruptedText. An
// Observation hook may call it for the steps it does not format itself.
func DefaultObservation(st agentrt.Step, toolUseID string, full bool, maxBytes int) agentrt.ContentBlock {
	if st.Decision.Kind != agentrt.DecideToolCall || st.Decision.Tool == "" || st.Observation == nil {
		return agentrt.ContentBlock{Type: "text", Text: nudge(st)}
	}
	o := st.Observation
	content := string(o.Content)
	if !full || len(content) > maxBytes {
		content = "[summary] " + o.Summary
	}
	if o.Kind == agentrt.ObserveInterrupted {
		content = InterruptedText
	}
	return agentrt.ContentBlock{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: o.Failure()}
}

// nudge tells the model what was wrong with a reply that asked for nothing
// executable, keyed on the recorded decision kind.
func nudge(st agentrt.Step) string {
	switch st.Decision.Kind {
	case agentrt.KindNoToolCall:
		return "Your reply contained no executable tool call. Respond with exactly one tool call."
	case agentrt.KindTruncated:
		return "Your reply hit the output token cap before the tool call was complete, so nothing ran. Respond with exactly one short tool call."
	}
	msg := "Your reply was not an executable tool call"
	if st.Observation != nil {
		msg += " (" + st.Observation.Summary + ")"
	}
	return msg + ". Respond with exactly one valid tool call."
}

// Decide maps a model response onto a decision. A refusal fails the run,
// because a model that declined will decline again. A reply cut off by the
// output cap and a reply with no tool use become the deliberately invalid
// kinds agentrt.KindTruncated and agentrt.KindNoToolCall: the driver
// records an invalid_decision observation and Messages nudges on the next
// step, which costs one step against the limits and never executes a
// partial tool call.
func Decide(resp agentrt.ModelResponse) agentrt.Decision {
	switch resp.StopReason {
	case "refusal":
		return agentrt.Decision{Kind: agentrt.DecideFail, Message: "the model declined to continue (refusal)"}
	case "max_tokens":
		return agentrt.Decision{Kind: agentrt.KindTruncated, Reason: "the reply hit the output token cap; the tool call was not executed"}
	}
	text := strings.TrimSpace(resp.Text)
	if len(resp.ToolUses) == 0 {
		return agentrt.Decision{Kind: agentrt.KindNoToolCall, Reason: text}
	}
	tu := resp.ToolUses[0]
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tu.Name, Args: orEmptyObject(tu.Args), Reason: Truncate(text, ReasonBytes)}
}

// Truncate shortens s to at most n bytes without splitting a character
// and marks the cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
