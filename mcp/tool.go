package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// truncationMarker ends text that hit the cap. The cap counts it: recorded
// content, marker and JSON escaping included, is never longer than the cap,
// except that a cap too small to hold the marker is raised to hold it.
const truncationMarker = "\n[truncated]"

// structuredDropped stands in for oversized structured content when the
// server sent no text to fall back to.
const structuredDropped = "[structured content dropped: too large]"

// tool is one server tool as the runtime sees it. It is unexported because a
// consumer only ever receives it as an agentrt.Tool.
type tool struct {
	conn       *Connection
	remote     string
	spec       agentrt.ToolSpec
	deny       []string
	fixed      map[string]json.RawMessage
	maxContent int
}

// Spec implements agentrt.Tool.
func (t *tool) Spec() agentrt.ToolSpec { return t.spec }

// Call sends the model's arguments, which the runtime has already validated
// against the restricted schema, with the operator's fixed parameters
// injected. It never returns agentrt.ErrAbortRun: a server's failure is
// something the agent can be told about, not a reason to end the run.
func (t *tool) Call(ctx context.Context, call agentrt.ToolCall) (agentrt.ToolResult, error) {
	args, err := arguments(call.Args)
	if err != nil {
		return agentrt.ToolResult{}, fmt.Errorf("%s: %w", t.spec.Name, err)
	}
	for _, name := range t.deny {
		if _, ok := args[name]; ok {
			return agentrt.ToolResult{}, fmt.Errorf("%s: parameter %q is denied by the operator", t.spec.Name, name)
		}
	}
	// A fixed value never overrides one the model sent: the approval would
	// show one value and the server receive another.
	for name := range t.fixed {
		if _, ok := args[name]; ok {
			return agentrt.ToolResult{}, fmt.Errorf("%s: parameter %q is fixed by the operator", t.spec.Name, name)
		}
	}
	for name, value := range t.fixed {
		args[name] = value
	}
	// The driver already bounds ctx by Spec().Timeout; this bound is for a
	// caller that is not the driver, such as a test or an operator probe.
	ctx, cancel := context.WithTimeout(ctx, t.spec.Timeout)
	defer cancel()
	ctx, slot := withCallSlot(ctx)
	res, err := t.conn.session.CallTool(ctx, &sdk.CallToolParams{Name: t.remote, Arguments: args})
	if err != nil {
		return agentrt.ToolResult{}, fmt.Errorf("mcp: server %q: tool %q: %w", t.conn.server, t.remote, sanitized{err})
	}
	if res.NeedsInput() {
		return agentrt.ToolResult{}, fmt.Errorf("%s: the server asked for more input, which this adapter does not support", t.spec.Name)
	}
	c := t.content(res, slot.raw())
	summary := fmt.Sprintf("%s/%s: %d bytes", t.conn.server, t.remote, len(c.raw))
	if c.truncated {
		summary += ", truncated"
	}
	if c.dropped > 0 {
		summary += fmt.Sprintf(", structured content of %d bytes dropped for the text", c.dropped)
	}
	if res.IsError {
		return agentrt.ToolResult{}, fmt.Errorf("%s, error: %s", summary, c.text)
	}
	return agentrt.ToolResult{Content: c.raw, Summary: summary}, nil
}

// content is a result as the runtime records it. raw is the recorded JSON
// and every byte count is its length; text is what an isError result says;
// dropped is the encoded size of structured content left out.
type content struct {
	raw       json.RawMessage
	text      string
	truncated bool
	dropped   int
}

// content maps a result to the JSON the runtime records. Structured content
// is the result when the server sent it and it fits; otherwise the text
// blocks are joined and a block that is not text becomes a placeholder,
// because base64 image data is not something to put in front of the model
// here. Structured content that does not fit is never cut, which would make
// an object a string: it is dropped for the text blocks, and the summary
// says so.
//
// Structured content is recorded from the result's raw bytes when they
// were captured, re-encoded the way a pinned schema is, so a number float64
// cannot hold, such as an id past 2^53, is recorded as the server sent it
// and not rounded; every other number is written exactly as before.
func (t *tool) content(res *sdk.CallToolResult, result json.RawMessage) content {
	var dropped int
	if res.StructuredContent != nil {
		if raw, err := structured(res, result); err == nil {
			if len(raw) <= t.maxContent {
				return content{raw: raw, text: string(raw)}
			}
			dropped = len(raw)
		}
	}
	text := joinContent(res.Content)
	if dropped > 0 && text == "" {
		text = structuredDropped
	}
	raw, text, truncated := capText(text, t.maxContent)
	return content{raw: raw, text: text, truncated: truncated || dropped > 0, dropped: dropped}
}

// structured encodes a result's structured content: from the raw result's
// "structuredContent", matched exactly as the SDK matches it, when the raw
// result was captured and carries it, and from the SDK's decoded value
// otherwise.
func structured(res *sdk.CallToolResult, result json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(result) > 0 && json.Unmarshal(result, &fields) == nil {
		if raw, ok := fields["structuredContent"]; ok && string(bytes.TrimSpace(raw)) != "null" {
			return canonicalSchema(raw)
		}
	}
	return marshalCanonical(res.StructuredContent)
}

func joinContent(blocks []sdk.Content) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch c := block.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, "[image omitted]")
		case *sdk.AudioContent:
			parts = append(parts, "[audio omitted]")
		case *sdk.ResourceLink:
			parts = append(parts, "[resource link omitted]")
		case *sdk.EmbeddedResource:
			parts = append(parts, "[embedded resource omitted]")
		default:
			parts = append(parts, "[content omitted]")
		}
	}
	return strings.Join(parts, "\n")
}

// arguments decodes what the model sent. The runtime validated it against an
// object schema, so anything else is a bug rather than a model mistake, and
// it is reported as an error either way.
func arguments(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	if args == nil {
		args = map[string]json.RawMessage{}
	}
	return args, nil
}

// capText encodes s as a JSON string of at most max bytes. When it does not
// fit, the longest prefix that does not split a rune and still fits with the
// marker is kept, so the recorded bytes, escaping and marker included, never
// exceed max.
func capText(s string, max int) (json.RawMessage, string, bool) {
	if raw := jsonString(s); len(raw) <= max {
		return raw, s, false
	}
	if least := len(jsonString(truncationMarker)); max < least {
		max = least
	}
	fits := func(i int) bool { return len(jsonString(s[:cutAt(s, i)]+truncationMarker)) <= max }
	// The encoded length only grows with the prefix, and no prefix longer
	// than max bytes encodes to max bytes or fewer.
	lo, hi := 0, min(len(s), max)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cut := s[:cutAt(s, lo)] + truncationMarker
	return jsonString(cut), cut, true
}

// cutAt moves i back to the start of a valid rune that i would split. Bytes
// that are not valid UTF-8 are encoded one at a time, so a cut between them
// splits nothing.
func cutAt(s string, i int) int {
	for j := i; j >= 0 && j >= i-utf8.UTFMax && j < len(s); j-- {
		if !utf8.RuneStart(s[j]) {
			continue
		}
		if r, size := utf8.DecodeRuneInString(s[j:]); (r != utf8.RuneError || size > 1) && j+size > i {
			return j
		}
		return i
	}
	return i
}

func jsonString(s string) json.RawMessage {
	raw, err := marshalCanonical(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}
