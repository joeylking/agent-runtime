// Package openai adapts an OpenAI-compatible chat completions endpoint to
// agentrt.Model. It speaks the shape that OpenAI documents and that most
// local servers implement, including Ollama's /v1, so one adapter covers a
// hosted model and a local one.
//
// The adapter does not stream and does not retry. The runtime's accounting
// caller owns retries, so every attempt is recorded; a timeout is returned
// bare so that caller charges it as ambiguous.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

// DefaultBaseURL is OpenAI's own endpoint. A local server is configured
// explicitly, for example http://127.0.0.1:11434/v1 for Ollama.
const DefaultBaseURL = "https://api.openai.com/v1"

// Config configures the adapter.
type Config struct {
	// Model is the provider's model id. It is required.
	Model string
	// Name overrides what Name() reports. The default is
	// "openai:<model>"; an override exists for a consumer whose
	// recordings or price table already use another name.
	Name string
	// BaseURL is the endpoint the /chat/completions path is appended to.
	// Empty means DefaultBaseURL.
	BaseURL string
	// APIKey is sent as a bearer token. Empty means OPENAI_API_KEY, and an
	// empty key is allowed: a local server needs none.
	APIKey string
	// HTTPClient overrides the client used for requests.
	HTTPClient *http.Client
}

// Model is one model on one OpenAI-compatible endpoint.
type Model struct {
	cfg    Config
	name   string
	base   string
	key    string
	client *http.Client
}

// New builds the adapter. It refuses an empty model id.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("openai: model is required")
	}
	m := &Model{cfg: cfg, name: cfg.Name, base: cfg.BaseURL, key: cfg.APIKey, client: cfg.HTTPClient}
	if m.name == "" {
		m.name = "openai:" + cfg.Model
	}
	if m.base == "" {
		m.base = DefaultBaseURL
	}
	m.base = strings.TrimRight(m.base, "/")
	if m.key == "" {
		m.key = os.Getenv("OPENAI_API_KEY")
	}
	if m.client == nil {
		m.client = &http.Client{}
	}
	return m, nil
}

// Free records each adapter's model as costing nothing under the name it
// reports, which is what a local model costs. Keying on the adapter keeps a
// Config.Name override and the price table in step.
func Free(models ...*Model) agentrt.PriceTable {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name()
	}
	return providers.Free(names...)
}

// Name implements agentrt.Model.
func (m *Model) Name() string { return m.name }

type message struct {
	Role string `json:"role"`
	// Content is always sent, even empty: a tool message without it is
	// rejected, and an assistant turn that only called a tool has none.
	Content string `json:"content"`
	// ToolCalls applies to an assistant turn, ToolCallID to a tool turn.
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments is JSON encoded as a string, in both directions.
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model             string    `json:"model"`
	Messages          []message `json:"messages"`
	Tools             []tool    `json:"tools,omitempty"`
	ToolChoice        string    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool     `json:"parallel_tool_calls,omitempty"`
	Temperature       int       `json:"temperature"`
	MaxTokens         int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Generate implements agentrt.Model.
func (m *Model) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	cr := chatRequest{Model: m.cfg.Model, Temperature: 0, MaxTokens: req.MaxOutputTokens}
	if req.System != "" {
		cr.Messages = append(cr.Messages, message{Role: "system", Content: req.System})
	}
	for _, msg := range req.Messages {
		cr.Messages = append(cr.Messages, convert(msg)...)
	}
	for _, ts := range req.Tools {
		var t tool
		t.Type = "function"
		t.Function.Name, t.Function.Description, t.Function.Parameters = ts.Name, ts.Description, ts.InputSchema
		cr.Tools = append(cr.Tools, t)
	}
	if len(cr.Tools) > 0 {
		// The runtime executes one call per step, so parallel calls are
		// refused rather than discarded after the fact.
		no := false
		cr.ToolChoice, cr.ParallelToolCalls = "auto", &no
	}
	body, err := json.Marshal(cr)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if m.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+m.key)
	}
	resp, err := m.client.Do(hreq)
	if err != nil {
		return agentrt.ModelResponse{}, providers.ClassifyTransport(err)
	}
	defer resp.Body.Close()
	raw, err := providers.ReadBody(resp.Body)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	if err := providers.ClassifyStatus("openai", resp.StatusCode, raw); err != nil {
		return agentrt.ModelResponse{}, err
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("openai: decode: %w", err)
	}
	if out.Error != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("openai: %s: %s", out.Error.Type, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return agentrt.ModelResponse{}, errors.New("openai: response carried no choice")
	}
	choice := out.Choices[0]
	mr := agentrt.ModelResponse{Text: choice.Message.Content, Raw: raw, Usage: agentrt.Usage{
		InputTokens:       out.Usage.PromptTokens,
		OutputTokens:      out.Usage.CompletionTokens,
		CachedInputTokens: out.Usage.PromptTokensDetails.CachedTokens,
	}}
	for i, tc := range choice.Message.ToolCalls {
		args, err := arguments(tc)
		if err != nil {
			return agentrt.ModelResponse{}, err
		}
		id := tc.ID
		if id == "" {
			id = providers.SynthesizeToolUseID(i)
		}
		mr.ToolUses = append(mr.ToolUses, agentrt.ToolUse{ID: id, Name: tc.Function.Name, Args: args})
	}
	mr.StopReason = stopReason(choice.FinishReason, len(mr.ToolUses) > 0)
	// A reply cut off by the output cap or stopped by a filter may carry a
	// half-written call. Drop the calls, keep the usage: the request was
	// served and charged, and the runtime's renderer nudges on the
	// recorded truncation or fails the run on the refusal.
	if mr.StopReason == "max_tokens" || mr.StopReason == "refusal" {
		mr.ToolUses = nil
	}
	return mr, nil
}

// arguments decodes a tool call's arguments, which arrive as a JSON string.
// They are validated here so that nothing but JSON reaches the runtime's
// schema validation and the audit log.
func arguments(tc toolCall) (json.RawMessage, error) {
	s := strings.TrimSpace(tc.Function.Arguments)
	if s == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(s)) {
		return nil, fmt.Errorf("openai: tool call %s: arguments are not JSON: %s", tc.Function.Name, s)
	}
	return json.RawMessage(s), nil
}

// stopReason maps a finish_reason onto the runtime's vocabulary. An
// unrecognized reason is passed through rather than reported as a normal
// end of turn, because a consumer reading the recorded response should see
// what the server actually said.
func stopReason(finish string, toolUses bool) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
	// Some servers answer a tool call with no finish reason at all.
	if toolUses {
		return "tool_use"
	}
	if finish == "" {
		return "end_turn"
	}
	return finish
}

// convert maps one runtime message onto chat completions messages. A tool
// result becomes its own role "tool" message, which must follow the
// assistant message carrying the call it answers.
func convert(msg agentrt.Message) []message {
	var text strings.Builder
	var calls []toolCall
	var results []message
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			var tc toolCall
			tc.ID, tc.Type = b.ToolUseID, "function"
			tc.Function.Name, tc.Function.Arguments = b.Name, string(orEmptyObject(b.Input))
			calls = append(calls, tc)
		case "tool_result":
			results = append(results, message{Role: "tool", ToolCallID: b.ToolUseID, Content: b.Content})
		}
	}
	if msg.Role == "assistant" {
		return []message{{Role: "assistant", Content: text.String(), ToolCalls: calls}}
	}
	var out []message
	if text.Len() > 0 {
		out = append(out, message{Role: "user", Content: text.String()})
	}
	return append(out, results...)
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
