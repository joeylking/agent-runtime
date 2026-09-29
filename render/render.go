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
	"strconv"
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
	// ReasonBytes caps the reason Decide records, the model's text and the
	// note naming any tool uses not executed together, so a long narration
	// cannot dominate the next render.
	ReasonBytes = 500
	// NameBytes caps each unexecuted tool use's name in that note: the
	// names are the model's, and nothing else bounds them.
	NameBytes = 64
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
	return "step_" + strconv.Itoa(st.Index)
}

// Messages renders the conversation for one decision from in.Steps, which
// the driver fills with the steps before the one being decided. A step
// with no recorded decision, which is one interrupted before it decided
// (or the step being decided, if a caller includes it), is skipped and
// does not count toward the recent-results window.
func Messages(in agentrt.StepInput, opts Options) []agentrt.Message {
	if opts.Opening == nil {
		panic("render: Options.Opening is required")
	}
	n := 0
	for _, st := range in.Steps {
		if st.Decision != nil {
			n++
		}
	}
	msgs := make([]agentrt.Message, 1, 1+2*n)
	msgs[0] = agentrt.Message{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: opts.Opening(in)}}}
	// Each turn's blocks are carved from one allocation per role, capped
	// at the turn's own blocks so that appending to one cannot reach the next.
	answers := make([]agentrt.ContentBlock, n)
	turns := make([]agentrt.ContentBlock, 0, 2*n)
	i := 0
	for _, st := range in.Steps {
		if st.Decision == nil {
			continue
		}
		full := i >= n-opts.recent()
		i++
		id := opts.toolUseID(st)
		var blocks []agentrt.ContentBlock
		turns, blocks = synthesize(turns, st, id)
		if opts.Assistant != nil {
			if custom := opts.Assistant(st, id); custom != nil {
				blocks = custom
			}
		}
		block := &answers[i-1]
		*block = DefaultObservation(st, id, full, opts.maxContent())
		if opts.Observation != nil {
			if custom := opts.Observation(st, id, full); custom.Type != "" {
				*block = custom
			}
		}
		msgs = append(msgs,
			agentrt.Message{Role: "assistant", Content: blocks},
			agentrt.Message{Role: "user", Content: answers[i-1 : i : i]})
	}
	return msgs
}

// synthesize renders a recorded decision as an assistant turn: the reason
// as text when there is one, then the tool_use block. A decision that was
// not a tool call has only its text, because there is nothing to call. The
// turn is appended to dst and returned capped at its own blocks.
func synthesize(dst []agentrt.ContentBlock, st agentrt.Step, toolUseID string) (rest, turn []agentrt.ContentBlock) {
	start := len(dst)
	d := st.Decision
	if d.Kind != agentrt.DecideToolCall || d.Tool == "" {
		text := d.Reason
		if text == "" {
			text = "(no tool call)"
		}
		dst = append(dst, agentrt.ContentBlock{Type: "text", Text: text})
	} else {
		if d.Reason != "" {
			dst = append(dst, agentrt.ContentBlock{Type: "text", Text: d.Reason})
		}
		dst = append(dst, agentrt.ContentBlock{Type: "tool_use", ToolUseID: toolUseID, Name: d.Tool, Input: Args(*d)})
	}
	return dst, dst[start:len(dst):len(dst)]
}

// Args is a recorded decision's arguments as a tool_use block carries
// them: an empty object when there were none and, for arguments that were
// not JSON, which the runtime records out of band in InvalidArgs, the
// object {"invalid_json": ...} (or "invalid_json_base64") that earlier
// releases stored in their place, so a rendered conversation, and every
// replay key taken over one, is what it was.
func Args(d agentrt.Decision) json.RawMessage {
	if d.InvalidArgs == "" {
		return orEmptyObject(d.Args)
	}
	key := "invalid_json"
	if d.InvalidBase64 {
		key += "_base64"
	}
	b, _ := json.Marshal(map[string]string{key: d.InvalidArgs}) // strings always encode
	return b
}

// DefaultObservation renders what a step produced, or a nudge when there
// was no tool call to answer: the tool_result carries the content when the
// step is within the recent window and under maxBytes, otherwise its
// summary; an interrupted observation carries InterruptedText. An
// Observation hook may call it for the steps it does not format itself.
func DefaultObservation(st agentrt.Step, toolUseID string, full bool, maxBytes int) agentrt.ContentBlock {
	if st.Decision == nil || st.Decision.Kind != agentrt.DecideToolCall || st.Decision.Tool == "" || st.Observation == nil {
		return agentrt.ContentBlock{Type: "text", Text: nudge(st)}
	}
	// The content is copied into a string only when it is rendered in full.
	o := st.Observation
	var content string
	switch {
	case o.Kind == agentrt.ObserveInterrupted:
		content = InterruptedText
	case !full || len(o.Content) > maxBytes:
		content = "[summary] " + o.Summary
	default:
		content = string(o.Content)
	}
	return agentrt.ContentBlock{Type: "tool_result", ToolUseID: toolUseID, Content: content, IsError: o.Failure()}
}

// nudge tells the model what was wrong with a reply that asked for nothing
// executable, keyed on the recorded decision kind.
func nudge(st agentrt.Step) string {
	var kind agentrt.DecisionKind
	if st.Decision != nil {
		kind = st.Decision.Kind
	}
	switch kind {
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

// The stop-reason vocabulary provider adapters map onto, and Decide reads.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	// StopContextWindowExceeded means the request no longer fits the
	// model's context window.
	StopContextWindowExceeded = "model_context_window_exceeded"
)

// Decide maps a model response onto a decision. A refusal fails the run,
// because a model that declined will decline again; so does a request
// over the context window, which only grows. A reply cut off by the
// output cap, even one carrying a complete tool use, and a reply with no
// tool use become the deliberately invalid kinds agentrt.KindTruncated and
// agentrt.KindNoToolCall: the driver records an invalid_decision
// observation and Messages nudges on the next step, which costs one step
// against the limits and never executes a partial tool call. Only the
// first tool use executes; when there are more, the reason says how many
// were not executed and names them, each name capped at NameBytes. The
// reason as composed is capped at ReasonBytes.
func Decide(resp agentrt.ModelResponse) agentrt.Decision {
	switch resp.StopReason {
	case StopRefusal:
		return agentrt.Decision{Kind: agentrt.DecideFail, Message: "the model declined to continue (refusal)"}
	case StopContextWindowExceeded:
		return agentrt.Decision{Kind: agentrt.DecideFail, Message: "the conversation no longer fits the model's context window (" + StopContextWindowExceeded + ")"}
	case StopMaxTokens:
		return agentrt.Decision{Kind: agentrt.KindTruncated, Reason: "the reply hit the output token cap; the tool call was not executed"}
	}
	text := strings.TrimSpace(resp.Text)
	if len(resp.ToolUses) == 0 {
		return agentrt.Decision{Kind: agentrt.KindNoToolCall, Reason: Truncate(text, ReasonBytes)}
	}
	tu := resp.ToolUses[0]
	reason := Truncate(text, ReasonBytes)
	if rest := resp.ToolUses[1:]; len(rest) > 0 {
		// The note is composed first, bounded, and the text gets the rest
		// of the cap, so the model is still told what did not run.
		note := unexecuted(rest)
		if text != "" {
			note = " " + note
		}
		reason = Truncate(text, ReasonBytes-len(note)) + note
	}
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: tu.Name, Args: orEmptyObject(tu.Args), Reason: reason}
}

// unexecuted names the tool uses not executed, each name capped at
// NameBytes and the note at half of ReasonBytes; names that do not fit
// are elided.
func unexecuted(rest []agentrt.ToolUse) string {
	head := fmt.Sprintf("[%d further tool call(s) not executed: ", len(rest))
	budget := ReasonBytes/2 - len(head) - len(", …]")
	var b strings.Builder
	b.WriteString(head)
	for i, r := range rest {
		name := Truncate(r.Name, NameBytes)
		sep := ""
		if i > 0 {
			sep = ", "
		}
		if b.Len()-len(head)+len(sep)+len(name) > budget {
			b.WriteString(sep + "…")
			break
		}
		b.WriteString(sep + name)
	}
	b.WriteString("]")
	return b.String()
}

// Truncate shortens s to at most n bytes, the "…" that marks the cut
// included, without splitting a character. When n is too small for the
// mark the cut is unmarked; a negative n is zero.
func Truncate(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) <= n {
		return s
	}
	mark := "…"
	cut := n - len(mark)
	if cut < 0 {
		cut, mark = n, ""
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
