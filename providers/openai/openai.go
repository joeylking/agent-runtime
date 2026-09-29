// Package openai adapts an OpenAI-compatible chat completions endpoint to
// agentrt.Model. It speaks the shape that OpenAI documents and that most
// local servers implement, including Ollama's /v1, so one adapter covers a
// hosted model and a local one.
//
// The adapter does not stream and does not retry. The runtime's accounting
// caller owns retries, so every attempt is recorded; a timeout, and a read
// that fails after the status line (the connection was lost mid-response, so
// the request may have been served), are both returned bare so that caller
// charges them as ambiguous. A 429 or a 5xx becomes a providers.StatusError
// wrapped in agentrt.TransientError, which the caller retries; any other
// non-2xx is the StatusError alone, which ends the run. A reply the server
// served but that cannot be used, because it does not decode, carries an
// error field or no choice, or has a tool call whose arguments are not a
// JSON object, is an agentrt.ServedError with whatever usage the reply
// reported.
//
// A request has no deadline of its own: the caller's context is what bounds
// it, and providers.DefaultTimeout (15 minutes) is only a backstop for a
// context with none. A response body over providers.MaxBodyBytes (16 MiB)
// fails the read rather than being read in full.
//
// The key goes only where it was meant to: OPENAI_API_KEY is read only for
// OpenAI's own endpoint, never for a configured BaseURL, and New refuses to
// send any key over plain http to a host that is not loopback.
//
// Parallel tool calls are turned off in the request, but a server may
// ignore that. Every call is returned; the runtime executes the first and
// records that the others were dropped.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
	// APIKey is sent as a bearer token. Empty means OPENAI_API_KEY when
	// BaseURL is empty or DefaultBaseURL, and no key otherwise: the
	// environment's key is OpenAI's and is never sent to another server. An
	// empty key is allowed, since a local server needs none. A key is
	// refused over http unless the host is loopback.
	APIKey string
	// HTTPClient overrides the client used for requests. The adapter uses a
	// copy whose transport refuses a body over providers.MaxBodyBytes. Nil
	// means a client with providers.DefaultTimeout, a backstop to the
	// caller's context.
	HTTPClient *http.Client
}

// String redacts the key, so a Config that is logged or printed does not
// leak it.
func (c Config) String() string {
	return fmt.Sprintf("openai.Config{Model:%q Name:%q BaseURL:%q APIKey:%s}", c.Model, c.Name, c.BaseURL, redact(c.APIKey))
}

// GoString redacts the key for %#v.
func (c Config) GoString() string { return c.String() }

func redact(key string) string {
	if key == "" {
		return `""`
	}
	return "[redacted]"
}

// Model is one model on one OpenAI-compatible endpoint.
type Model struct {
	cfg    Config
	name   string
	base   string
	key    string
	client *http.Client
}

// New builds the adapter. It refuses an empty model id, a BaseURL that is
// not an http or https URL, and a key bound for a host that is not loopback
// over plain http.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("openai: model is required")
	}
	m := &Model{cfg: cfg, name: cfg.Name, base: strings.TrimRight(cfg.BaseURL, "/"), key: cfg.APIKey, client: providers.Client(cfg.HTTPClient)}
	if m.name == "" {
		m.name = Name(cfg.Model)
	}
	if m.base == "" {
		m.base = DefaultBaseURL
	}
	if m.key == "" && m.base == DefaultBaseURL {
		m.key = os.Getenv("OPENAI_API_KEY")
	}
	u, err := url.Parse(m.base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("openai: base URL %q is not an http or https URL", m.base)
	}
	if m.key != "" && u.Scheme == "http" && !loopback(u.Hostname()) {
		return nil, fmt.Errorf("openai: refusing to send an API key over plain http to %s", u.Host)
	}
	return m, nil
}

// loopback reports whether host names this machine, the one place a key may
// travel unencrypted.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// String redacts the key, so a Model that is logged or printed does not
// leak it.
func (m Model) String() string {
	return fmt.Sprintf("openai.Model{Name:%q BaseURL:%q APIKey:%s}", m.name, m.base, redact(m.key))
}

// GoString redacts the key for %#v.
func (m Model) GoString() string { return m.String() }

// Name is what an adapter for model reports when Config.Name is unset, so
// a consumer can key a price table or a recording directory before it
// builds the adapter.
func Name(model string) string { return "openai:" + model }

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

// maxEcho bounds how much server or model text an error message repeats.
const maxEcho = 512

type usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type chatResponse struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
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
	defer providers.DrainClose(resp.Body)
	raw, err := providers.ReadBody(resp.Body)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	if err := providers.ClassifyResponse("openai", resp, raw); err != nil {
		return agentrt.ModelResponse{}, err
	}
	// The usage is read on its own first, so a reply that is served but
	// unusable is still charged what it reported.
	var counts struct {
		Usage usage `json:"usage"`
	}
	json.Unmarshal(raw, &counts)
	used := agentrt.Usage{
		InputTokens:       counts.Usage.PromptTokens,
		OutputTokens:      counts.Usage.CompletionTokens,
		CachedInputTokens: counts.Usage.PromptTokensDetails.CachedTokens,
	}
	served := func(err error) (agentrt.ModelResponse, error) {
		return agentrt.ModelResponse{}, agentrt.ServedError{Usage: used, Err: err}
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return served(fmt.Errorf("openai: decode: %w", err))
	}
	if out.Error != nil {
		return served(fmt.Errorf("openai: %s: %s", providers.Truncate(out.Error.Type, maxEcho), providers.Truncate(out.Error.Message, maxEcho)))
	}
	if len(out.Choices) == 0 {
		return served(errors.New("openai: response carried no choice"))
	}
	choice := out.Choices[0]
	mr := agentrt.ModelResponse{Text: choice.Message.Content, Raw: raw, Usage: used}
	for i, tc := range choice.Message.ToolCalls {
		args, err := arguments(tc)
		if err != nil {
			return served(err)
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
	if !json.Valid([]byte(s)) || s[0] != '{' {
		return nil, fmt.Errorf("openai: tool call %s: arguments are not a JSON object: %s", providers.Truncate(tc.Function.Name, maxEcho), providers.Truncate(s, maxEcho))
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
