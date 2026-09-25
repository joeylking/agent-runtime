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

// truncationMarker ends content that hit the cap. The cap applies to the
// content itself, so the marker is what exceeds it.
const truncationMarker = "\n[truncated]"

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
	for name, value := range t.fixed {
		args[name] = value
	}
	// The driver already bounds ctx by Spec().Timeout; this bound is for a
	// caller that is not the driver, such as a test or an operator probe.
	ctx, cancel := context.WithTimeout(ctx, t.spec.Timeout)
	defer cancel()
	res, err := t.conn.session.CallTool(ctx, &sdk.CallToolParams{Name: t.remote, Arguments: args})
	if err != nil {
		return agentrt.ToolResult{}, err
	}
	if res.NeedsInput() {
		return agentrt.ToolResult{}, fmt.Errorf("%s: the server asked for more input, which this adapter does not support", t.spec.Name)
	}
	content, text, truncated := t.content(res)
	summary := fmt.Sprintf("%s/%s: %d bytes", t.conn.server, t.remote, len(content))
	if truncated {
		summary += ", truncated"
	}
	if res.IsError {
		return agentrt.ToolResult{}, fmt.Errorf("%s, error: %s", summary, text)
	}
	return agentrt.ToolResult{Content: content, Summary: summary}, nil
}

// content maps a result to the JSON the runtime records. Structured content
// is the result when the server sent it; otherwise the text blocks are joined
// and a block that is not text becomes a placeholder, because base64 image
// data is not something to put in front of the model here. The text is
// returned too, for the message an isError result becomes.
func (t *tool) content(res *sdk.CallToolResult) (json.RawMessage, string, bool) {
	if res.StructuredContent != nil {
		if raw, err := marshalCanonical(res.StructuredContent); err == nil {
			if len(raw) <= t.maxContent {
				return raw, string(raw), false
			}
			cut := truncate(string(raw), t.maxContent)
			return jsonString(cut), cut, true
		}
	}
	text := joinContent(res.Content)
	truncated := len(text) > t.maxContent
	if truncated {
		text = truncate(text, t.maxContent)
	}
	return jsonString(text), text, truncated
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

// truncate cuts s to max bytes on a rune boundary and marks the cut.
func truncate(s string, max int) string {
	if max > len(s) {
		max = len(s)
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + truncationMarker
}

func jsonString(s string) json.RawMessage {
	raw, err := marshalCanonical(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return raw
}
