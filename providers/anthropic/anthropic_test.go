package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/anthropic"
)

// fake is a minimal Messages API that records requests and serves queued
// responses, so the adapter's contract is tested without the provider. No
// test in this module reaches the real API.
type fake struct {
	mu        sync.Mutex
	paths     []string
	requests  []map[string]any
	responses []response
}

type response struct {
	status int
	body   string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(body, &m)
	f.paths = append(f.paths, r.URL.Path)
	f.requests = append(f.requests, m)
	if len(f.responses) == 0 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	io.WriteString(w, resp.body)
}

// newModel points a model at the fake. Strict and the effort mirror what
// casework runs with.
func newModel(t *testing.T, f *fake, cfg anthropic.Config) *anthropic.Model {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.BaseURL, cfg.APIKey = srv.URL, "test-key"
	if cfg.Model == "" {
		cfg.Model = "claude-opus-5"
	}
	m, err := anthropic.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const toolUseMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"tool_use","stop_sequence":null,
"content":[{"type":"thinking","thinking":"consider the case","signature":"sig_abc"},{"type":"text","text":"Reading the conversation."},{"type":"tool_use","id":"toolu_01","name":"read_conversation","input":{}}],
"usage":{"input_tokens":120,"output_tokens":30,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

var spec = agentrt.ToolSpec{Name: "read_conversation", Description: "Read it.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1}},"required":[],"additionalProperties":false}`)}

func userRequest(tools ...agentrt.ToolSpec) agentrt.ModelRequest {
	return agentrt.ModelRequest{Tools: tools, Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}}}}
}

func TestNew_RequiresAModelAndNamesItself(t *testing.T) {
	if _, err := anthropic.New(anthropic.Config{}); err == nil {
		t.Fatal("an empty model id was accepted")
	}
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "anthropic:claude-opus-5" || m.RawAssistantBlock() != anthropic.DefaultRawAssistantBlock {
		t.Fatalf("name = %s, raw block = %s", m.Name(), m.RawAssistantBlock())
	}
	m, err = anthropic.New(anthropic.Config{Model: "claude-opus-5", Name: "claude-opus-5", RawAssistantBlock: "casework.raw_assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "claude-opus-5" || m.RawAssistantBlock() != "casework.raw_assistant" {
		t.Fatalf("name = %s, raw block = %s", m.Name(), m.RawAssistantBlock())
	}
}

func TestGenerate_MapsToolUseAndUsage(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{Strict: true, Effort: "medium"})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{System: "sys", Tools: []agentrt.ToolSpec{spec}, MaxOutputTokens: 1000,
		Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_conversation" || resp.ToolUses[0].ID != "toolu_01" || string(resp.ToolUses[0].Args) != "{}" {
		t.Fatalf("tool uses %+v", resp.ToolUses)
	}
	if resp.Usage != (agentrt.Usage{InputTokens: 120, OutputTokens: 30}) || resp.StopReason != "tool_use" || resp.Text != "Reading the conversation." {
		t.Fatalf("resp %+v", resp)
	}
	if len(resp.Raw) == 0 {
		t.Fatal("raw message not returned")
	}
	if f.paths[0] != "/v1/messages" {
		t.Fatalf("path %s", f.paths[0])
	}
	req := f.requests[0]
	tool := req["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["strict"] != true || schema["additionalProperties"] != false {
		t.Fatalf("tool definition %v", tool)
	}
	if limit := schema["properties"].(map[string]any)["limit"].(map[string]any); limit["minimum"] != nil {
		t.Fatalf("strict mode kept a rejected keyword: %v", schema)
	}
	if req["tool_choice"].(map[string]any)["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice %v", req["tool_choice"])
	}
	if req["output_config"].(map[string]any)["effort"] != "medium" || req["max_tokens"].(float64) != 1000 {
		t.Fatalf("request %v", req)
	}
	if req["system"].([]any)[0].(map[string]any)["text"] != "sys" {
		t.Fatalf("system %v", req["system"])
	}
}

func TestGenerate_WithoutStrictSendsTheWholeSchema(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{})
	if _, err := m.Generate(context.Background(), userRequest(spec)); err != nil {
		t.Fatal(err)
	}
	tool := f.requests[0]["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if _, ok := tool["strict"]; ok {
		t.Fatalf("strict sent without Strict: %v", tool)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("a schema keyword outside properties and required was dropped: %v", schema)
	}
	if limit := schema["properties"].(map[string]any)["limit"].(map[string]any); limit["minimum"].(float64) != 1 {
		t.Fatalf("the full schema was not sent: %v", schema)
	}
	if f.requests[0]["max_tokens"].(float64) != anthropic.DefaultMaxTokens {
		t.Fatalf("max_tokens %v", f.requests[0]["max_tokens"])
	}
}

func TestGenerate_RawAssistantTurnReplaysThinkingBlocks(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}, {200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{RawAssistantBlock: "casework.raw_assistant"})
	first, err := m.Generate(context.Background(), userRequest(spec))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), agentrt.ModelRequest{Tools: []agentrt.ToolSpec{spec}, Messages: []agentrt.Message{
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: m.RawAssistantBlock(), Text: string(first.Raw)}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "toolu_01", Content: `{"ok":true}`}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := f.requests[1]["messages"].([]any)
	content := msgs[1].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("assistant content %v", content)
	}
	think := content[0].(map[string]any)
	if think["type"] != "thinking" || think["signature"] != "sig_abc" || think["thinking"] != "consider the case" {
		t.Fatalf("thinking block not replayed intact: %v", think)
	}
	if tu := content[2].(map[string]any); tu["type"] != "tool_use" || tu["id"] != "toolu_01" {
		t.Fatalf("tool_use not replayed: %v", tu)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "toolu_01" {
		t.Fatalf("tool_result %v", res)
	}
}

func TestGenerate_RawBlockTypeIsTheConfiguredOne(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{})
	// Under the default name, another consumer's block type is not a raw
	// turn and is refused rather than sent as something the API would take
	// for text.
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{Messages: []agentrt.Message{
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "casework.raw_assistant", Text: toolUseMessage}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "unsupported content block type") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerate_SynthesizedTurnWithoutRaw(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{})
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{Tools: []agentrt.ToolSpec{spec}, Messages: []agentrt.Message{
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "text", Text: "reason"}, {Type: "tool_use", ToolUseID: "toolu_step_0", Name: "read_conversation"}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "toolu_step_0", Content: "x", IsError: true}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "text", Text: "   "}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := f.requests[0]["messages"].([]any)
	call := msgs[1].(map[string]any)["content"].([]any)[1].(map[string]any)
	if input, ok := call["input"].(map[string]any); !ok || len(input) != 0 {
		t.Fatalf("an empty tool use input must be sent as {}: %v", call)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["is_error"] != true || res["tool_use_id"] != "toolu_step_0" {
		t.Fatalf("tool_result %v", res)
	}
	// An assistant turn whose only text was blank cannot be sent empty.
	blank := msgs[3].(map[string]any)["content"].([]any)
	if len(blank) != 1 || blank[0].(map[string]any)["text"] != "(no output)" {
		t.Fatalf("empty message %v", blank)
	}
}

func TestGenerate_RefusalAndMaxTokensKeepUsageDropToolUse(t *testing.T) {
	refusal := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"refusal","stop_sequence":null,"content":[{"type":"text","text":"I can't help with that."}],"usage":{"input_tokens":50,"output_tokens":9,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	truncated := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"max_tokens","stop_sequence":null,"content":[{"type":"tool_use","id":"toolu_9","name":"read_conversation","input":{}}],"usage":{"input_tokens":70,"output_tokens":4096,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
	f := &fake{responses: []response{{200, refusal}, {200, truncated}}}
	m := newModel(t, f, anthropic.Config{})
	r1, err := m.Generate(context.Background(), userRequest())
	if err != nil || r1.StopReason != "refusal" || len(r1.ToolUses) != 0 || r1.Usage.InputTokens != 50 || r1.Usage.OutputTokens != 9 {
		t.Fatalf("refusal: %+v %v", r1, err)
	}
	r2, err := m.Generate(context.Background(), userRequest())
	if err != nil || r2.StopReason != "max_tokens" || len(r2.ToolUses) != 0 || r2.Usage.OutputTokens != 4096 {
		t.Fatalf("max_tokens: %+v %v", r2, err)
	}
}

func TestGenerate_FoldsCacheUsage(t *testing.T) {
	cached := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"end_turn","stop_sequence":null,"content":[{"type":"text","text":"ok"}],
"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":401,"cache_read_input_tokens":2000}}`
	f := &fake{responses: []response{{200, cached}}}
	m := newModel(t, f, anthropic.Config{})
	resp, err := m.Generate(context.Background(), userRequest())
	if err != nil {
		t.Fatal(err)
	}
	// 100 uncached + 2000 read + ceil(1.25 * 401) = 502 written.
	want := agentrt.Usage{InputTokens: 100 + 2000 + 502, OutputTokens: 10, CachedInputTokens: 2000}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
	// The cost a cache write is folded into is charged at the input rate.
	price := anthropic.Prices()["anthropic:claude-opus-5"]
	if price.InputPerMTok == 0 || price.CachedInputPerMTok != price.InputPerMTok/10 {
		t.Fatalf("price = %+v", price)
	}
}

func TestGenerate_ErrorsAreClassifiedAndNotRetriedBySDK(t *testing.T) {
	f := &fake{responses: []response{
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		{529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`},
		{400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`},
	}}
	m := newModel(t, f, anthropic.Config{})
	var tr agentrt.TransientError
	if _, err := m.Generate(context.Background(), userRequest()); !errors.As(err, &tr) {
		t.Fatalf("429 not transient: %v", err)
	}
	if _, err := m.Generate(context.Background(), userRequest()); !errors.As(err, &tr) {
		t.Fatalf("529 not transient: %v", err)
	}
	if _, err := m.Generate(context.Background(), userRequest()); err == nil || errors.As(err, &tr) {
		t.Fatalf("400 should be a plain error: %v", err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("the SDK retried on its own: %d requests for 3 calls", len(f.requests))
	}
}

func TestStrictSubset_RemovesRejectedKeywords(t *testing.T) {
	in := map[string]any{}
	json.Unmarshal([]byte(`{"type":"object","properties":{
		"findings":{"type":"array","maxItems":20,"minItems":2,"items":{"type":"object","properties":{"claim":{"type":"string","minLength":1,"maxLength":500},"refs":{"type":"array","maxItems":10,"minItems":1,"items":{"type":"string","pattern":"^ev_"}}},"required":["claim"],"additionalProperties":false}},
		"limit":{"type":"integer","minimum":1,"maximum":50},
		"nested":{"type":"object","properties":{"x":{"type":"string"}}}
	},"required":["findings"],"additionalProperties":false}`), &in)
	out, _ := json.Marshal(anthropic.StrictSubset(in))
	s := string(out)
	for _, bad := range []string{"maxItems", "minLength", "maxLength", "minimum", "maximum", "pattern", `"minItems":2`} {
		if strings.Contains(s, bad) {
			t.Errorf("strict subset still contains %s: %s", bad, s)
		}
	}
	for _, good := range []string{`"minItems":1`, `"required":["claim"]`, `"nested":{"additionalProperties":false`} {
		if !strings.Contains(s, good) {
			t.Errorf("strict subset lost %s: %s", good, s)
		}
	}
}

func TestPrices_KeyedByTheDefaultName(t *testing.T) {
	prices := anthropic.Prices()
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, m.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, "claude-opus-5"); err == nil {
		t.Fatal("the bare model id must not be priced")
	}
	for name, p := range prices {
		if p.InputPerMTok <= 0 || p.OutputPerMTok <= 0 || p.CachedInputPerMTok <= 0 {
			t.Errorf("%s: incomplete price %+v", name, p)
		}
	}
}
