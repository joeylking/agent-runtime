// Package ollama adapts a local Ollama server to agentrt.Model through its
// native chat API with tool calling. It is the free provider a consumer
// develops against: nothing here reaches beyond the configured host, and a
// model it serves costs nothing, which Free records so that free and
// unpriced stay distinguishable.
//
// The adapter does not stream and does not retry. The runtime's accounting
// caller owns retries, so every attempt is recorded; a timeout, and a read
// that fails after the status line (the connection was lost mid-response, so
// the request may have been served), are both returned bare so that caller
// charges them as ambiguous. A 429 or a 5xx becomes a providers.StatusError
// wrapped in agentrt.TransientError, which the caller retries; any other
// non-2xx is the StatusError alone, which ends the run. A reply the server
// served but that cannot be used, because it does not decode or carries an
// error field, is an agentrt.ServedError with whatever usage the reply
// reported.
//
// A reply whose token counts are negative, do not fit, or are not integers
// is an agentrt.ServedError with zero usage naming the count, so the
// accounting caller charges its own estimate rather than a count it cannot
// believe.
//
// A request has no deadline of its own: the caller's context is what bounds
// it, and providers.DefaultTimeout (15 minutes) is only a backstop for a
// context with none. A response body over providers.MaxBodyBytes (16 MiB)
// fails the read rather than being read in full. No redirect is followed: a
// 3xx is a permanent providers.StatusError naming only the host it pointed
// to.
//
// Host resolves the server once, at New: the configured Host, then
// OLLAMA_HOST, then DefaultHost; nothing else is read from the environment.
//
// Ollama's chat API has no switch for parallel tool calls, so a reply may
// carry several. Every one is returned; the runtime executes the first and
// records that the others were dropped.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
)

// DefaultHost is where an Ollama server listens when OLLAMA_HOST says
// nothing.
const DefaultHost = "127.0.0.1:11434"

// DefaultNumCtx is the context window requested from the server. Ollama's
// own default is far smaller than a tool-using agent's rendered context,
// and a server that silently truncates the prompt is worse than one that
// refuses it.
const DefaultNumCtx = 32768

// Config configures the adapter.
type Config struct {
	// Model is the Ollama model id, for example qwen3:30b-a3b. It is
	// required.
	Model string
	// Name overrides what Name() reports. The default is
	// "ollama:<model>"; an override exists for a consumer whose
	// recordings or price table already use another name.
	Name string
	// Host is the server, with or without an http or https scheme. Empty
	// means OLLAMA_HOST, then DefaultHost. New refuses any other scheme.
	Host string
	// Think leaves the model's thinking mode on. It is off by default
	// because tool-calling models are steadier without it.
	Think bool
	// NumCtx is the requested context window. Zero means DefaultNumCtx.
	NumCtx int
	// HTTPClient overrides the client used for requests. The adapter uses a
	// copy whose transport refuses a body over providers.MaxBodyBytes and,
	// unless the client has a CheckRedirect of its own, follows no
	// redirect. Nil means a client with providers.DefaultTimeout, a backstop
	// to the caller's context.
	HTTPClient *http.Client
}

// Model is one Ollama model on one server.
type Model struct {
	cfg    Config
	name   string
	host   string
	client *http.Client
}

// New builds the adapter. It refuses an empty model id and a host that is
// not an http or https server.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("ollama: model is required")
	}
	host, err := resolveHost(cfg.Host)
	if err != nil {
		return nil, err
	}
	m := &Model{cfg: cfg, name: cfg.Name, host: host, client: providers.Client(cfg.HTTPClient)}
	if m.name == "" {
		m.name = Name(cfg.Model)
	}
	if m.cfg.NumCtx <= 0 {
		m.cfg.NumCtx = DefaultNumCtx
	}
	return m, nil
}

// Host resolves a configured host: the argument, then OLLAMA_HOST, then
// DefaultHost, with http:// prefixed when no scheme is given. It is
// exported because a consumer that probes the server before a run needs
// the same answer the adapter will use. A host New would refuse, such as
// one with another scheme, resolves to the empty string.
func Host(host string) string {
	h, err := resolveHost(host)
	if err != nil {
		return ""
	}
	return h
}

func resolveHost(host string) (string, error) {
	if host == "" {
		host = os.Getenv("OLLAMA_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("ollama: host %q: %w", host, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("ollama: host %q: the scheme must be http or https", host)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("ollama: host %q is not a server address", host)
	}
	return strings.TrimRight(host, "/"), nil
}

// Name is what an adapter for model reports when Config.Name is unset, so
// a consumer can key a price table or a recording directory before it
// builds the adapter.
func Name(model string) string { return "ollama:" + model }

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
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
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
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Tools    []tool         `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Think    bool           `json:"think"`
	Options  map[string]any `json:"options,omitempty"`
}

// maxEcho bounds how much server text an error message repeats.
const maxEcho = 512

// usage is the part of a reply decoded before the rest, so that it survives
// a reply that does not otherwise decode, count by count, so that a count
// that does not fit is refused by name rather than failing the decode.
type usage struct {
	PromptEvalCount json.RawMessage `json:"prompt_eval_count"`
	EvalCount       json.RawMessage `json:"eval_count"`
}

// usageOf reads the usage a reply reports. Absent counts are zero; a count
// that is not a non-negative integer that fits is an error wrapping
// providers.ErrUnusableUsage.
func usageOf(raw []byte) (agentrt.Usage, error) {
	var counts usage
	if json.Unmarshal(raw, &counts) != nil {
		return agentrt.Usage{}, nil
	}
	var u agentrt.Usage
	var err error
	if u.InputTokens, err = providers.UsageCount("prompt_eval_count", counts.PromptEvalCount); err != nil {
		return agentrt.Usage{}, err
	}
	if u.OutputTokens, err = providers.UsageCount("eval_count", counts.EvalCount); err != nil {
		return agentrt.Usage{}, err
	}
	return u, providers.CheckUsage(u)
}

type chatResponse struct {
	Message    message `json:"message"`
	DoneReason string  `json:"done_reason"`
	Error      string  `json:"error"`
}

// Generate implements agentrt.Model.
func (m *Model) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	cr := chatRequest{Model: m.cfg.Model, Stream: false, Think: m.cfg.Think,
		Options: map[string]any{"temperature": 0, "num_ctx": m.cfg.NumCtx}}
	if req.MaxOutputTokens > 0 {
		cr.Options["num_predict"] = req.MaxOutputTokens
	}
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
	body, err := json.Marshal(cr)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(hreq)
	if err != nil {
		return agentrt.ModelResponse{}, providers.ClassifyTransport(err)
	}
	defer providers.DrainClose(resp.Body)
	raw, err := providers.ReadBody(resp.Body)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	if err := providers.ClassifyResponse("ollama", resp, raw); err != nil {
		return agentrt.ModelResponse{}, err
	}
	// The usage is read on its own first, so a reply that is served but
	// unusable is still charged what it reported. Usage that cannot be
	// believed is charged nothing here, and the accounting caller charges
	// its own estimate.
	used, err := usageOf(raw)
	if err != nil {
		return agentrt.ModelResponse{}, agentrt.ServedError{Err: fmt.Errorf("ollama: %w", err)}
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return agentrt.ModelResponse{}, agentrt.ServedError{Usage: used, Err: fmt.Errorf("ollama: decode: %w", err)}
	}
	// A 200 carrying an error field is the server refusing the request,
	// for example a context length it cannot serve.
	if out.Error != "" {
		return agentrt.ModelResponse{}, agentrt.ServedError{Usage: used, Err: fmt.Errorf("ollama: %s", providers.Truncate(out.Error, maxEcho))}
	}
	mr := agentrt.ModelResponse{Text: out.Message.Content, Raw: raw, Usage: used}
	for i, tc := range out.Message.ToolCalls {
		args := orEmptyObject(tc.Function.Arguments)
		if args[0] != '{' {
			return agentrt.ModelResponse{}, agentrt.ServedError{Usage: used, Err: fmt.Errorf("ollama: tool call %s: arguments are not a JSON object: %s",
				providers.Truncate(tc.Function.Name, maxEcho), providers.Truncate(string(args), maxEcho))}
		}
		id := tc.ID
		if id == "" {
			id = providers.SynthesizeToolUseID(i)
		}
		mr.ToolUses = append(mr.ToolUses, agentrt.ToolUse{ID: id, Name: tc.Function.Name, Args: args})
	}
	// A reply cut off by num_predict may carry a half-written call. Drop
	// the calls, keep the usage: the request was served and charged, and
	// the runtime's renderer nudges on the recorded truncation.
	if out.DoneReason == "length" {
		mr.ToolUses, mr.StopReason = nil, "max_tokens"
		return mr, nil
	}
	mr.StopReason = stopReason(out.DoneReason, len(mr.ToolUses) > 0)
	return mr, nil
}

// stopReason maps Ollama's done_reason onto the runtime's vocabulary. An
// unrecognized reason is passed through rather than reported as a normal
// end of turn, because a consumer reading the recorded response should see
// what the server actually said.
func stopReason(done string, toolUses bool) string {
	if toolUses {
		return "tool_use"
	}
	switch done {
	case "stop", "":
		return "end_turn"
	}
	return done
}

// convert maps one runtime message to Ollama messages. Tool results become
// role "tool" messages that follow the assistant message carrying the call.
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
			tc.ID = b.ToolUseID
			tc.Function.Name, tc.Function.Arguments = b.Name, orEmptyObject(b.Input)
			calls = append(calls, tc)
		case "tool_result":
			results = append(results, message{Role: "tool", Content: b.Content, ToolName: b.Name})
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
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}
