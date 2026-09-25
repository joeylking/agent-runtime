// Package ollama adapts a local Ollama server to agentrt.Model through its
// native chat API with tool calling. It is the free provider a consumer
// develops against: nothing here reaches beyond the configured host, and a
// model it serves costs nothing, which Free records so that free and
// unpriced stay distinguishable.
//
// The adapter does not stream and does not retry. The runtime's accounting
// caller owns retries, so every attempt is recorded; a timeout is returned
// bare so that caller charges it as ambiguous.
package ollama

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
	// Host is the server, with or without a scheme. Empty means
	// OLLAMA_HOST, then DefaultHost.
	Host string
	// Think leaves the model's thinking mode on. It is off by default
	// because tool-calling models are steadier without it.
	Think bool
	// NumCtx is the requested context window. Zero means DefaultNumCtx.
	NumCtx int
	// HTTPClient overrides the client used for requests.
	HTTPClient *http.Client
}

// Model is one Ollama model on one server.
type Model struct {
	cfg    Config
	name   string
	host   string
	client *http.Client
}

// New builds the adapter. It refuses an empty model id.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("ollama: model is required")
	}
	m := &Model{cfg: cfg, name: cfg.Name, host: Host(cfg.Host), client: cfg.HTTPClient}
	if m.name == "" {
		m.name = "ollama:" + cfg.Model
	}
	if m.cfg.NumCtx <= 0 {
		m.cfg.NumCtx = DefaultNumCtx
	}
	if m.client == nil {
		m.client = &http.Client{}
	}
	return m, nil
}

// Host resolves a configured host: the argument, then OLLAMA_HOST, then
// DefaultHost, with http:// prefixed when no scheme is given. It is
// exported because a consumer that probes the server before a run needs
// the same answer the adapter will use.
func Host(host string) string {
	if host == "" {
		host = os.Getenv("OLLAMA_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
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

type chatResponse struct {
	Message         message `json:"message"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	Error           string  `json:"error"`
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
	defer resp.Body.Close()
	raw, err := providers.ReadBody(resp.Body)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	if err := providers.ClassifyStatus("ollama", resp.StatusCode, raw); err != nil {
		return agentrt.ModelResponse{}, err
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return agentrt.ModelResponse{}, fmt.Errorf("ollama: decode: %w", err)
	}
	// A 200 carrying an error field is the server refusing the request,
	// for example a context length it cannot serve.
	if out.Error != "" {
		return agentrt.ModelResponse{}, fmt.Errorf("ollama: %s", out.Error)
	}
	mr := agentrt.ModelResponse{Text: out.Message.Content, Raw: raw,
		Usage: agentrt.Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount}}
	for i, tc := range out.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = providers.SynthesizeToolUseID(i)
		}
		mr.ToolUses = append(mr.ToolUses, agentrt.ToolUse{ID: id, Name: tc.Function.Name, Args: orEmptyObject(tc.Function.Arguments)})
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
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
