package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"

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
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "anthropic:claude-opus-5" || anthropic.Name("claude-opus-5") != m.Name() || m.RawAssistantBlock() != anthropic.DefaultRawAssistantBlock {
		t.Fatalf("name = %s, raw block = %s", m.Name(), m.RawAssistantBlock())
	}
	m, err = anthropic.New(anthropic.Config{Model: "claude-opus-5", Name: "claude-opus-5", RawAssistantBlock: "casework.raw_assistant", APIKey: "test-key"})
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
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, m.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, "claude-opus-5"); err == nil {
		t.Fatal("the bare model id must not be priced")
	}
	for _, model := range []string{"claude-fable-5-1", "claude-mythos-5-1", "claude-fable-5", "claude-mythos-5", "claude-opus-5-5", "claude-opus-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4-5"} {
		if _, err := providers.PriceFor(prices, anthropic.Name(model)); err != nil {
			t.Errorf("current model %s: %v", model, err)
		}
	}
	for name, p := range prices {
		if p.InputPerMTok <= 0 || p.OutputPerMTok <= 0 || p.CachedInputPerMTok <= 0 {
			t.Errorf("%s: incomplete price %+v", name, p)
		}
	}
}

// roundTrip answers every request in-process, so a test can see where a
// request with no BaseURL would have gone without reaching it.
type roundTrip struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rt *roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, r)
	rt.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(toolUseMessage)), Request: r}, nil
}

func TestNew_ReadsOnlyTheKeyFromTheEnvironment(t *testing.T) {
	env := &fake{responses: []response{{200, toolUseMessage}}}
	srv := httptest.NewServer(env)
	t.Cleanup(srv.Close)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token-from-env")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: yes")
	t.Setenv("ANTHROPIC_PROFILE", "nonexistent")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := anthropic.New(anthropic.Config{Model: "claude-opus-5"}); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("no key anywhere = %v, want New to refuse", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "key-from-env")
	rt := &roundTrip{}
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), userRequest()); err != nil {
		t.Fatal(err)
	}
	r := rt.reqs[0]
	if r.URL.Host != "api.anthropic.com" || r.Header.Get("X-Api-Key") != "key-from-env" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Leak") != "" {
		t.Fatalf("request went to %s with key %q, authorization %q, custom header %q", r.URL, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.Header.Get("X-Leak"))
	}
	if len(env.requests) != 0 {
		t.Fatal("ANTHROPIC_BASE_URL was honoured")
	}
	// An explicit key wins over the environment's.
	m, _ = anthropic.New(anthropic.Config{Model: "claude-opus-5", APIKey: "explicit", HTTPClient: &http.Client{Transport: rt}})
	m.Generate(context.Background(), userRequest())
	if got := rt.reqs[1].Header.Get("X-Api-Key"); got != "explicit" {
		t.Fatalf("key sent = %q", got)
	}
}

func TestNew_RefusesAnUnpricedModelUnlessNamed(t *testing.T) {
	if _, err := anthropic.New(anthropic.Config{Model: "claude-unknown-9", APIKey: "k"}); !errors.Is(err, providers.ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
	if _, err := anthropic.New(anthropic.Config{Model: "claude-unknown-9", Name: "mine", APIKey: "k"}); err != nil {
		t.Fatalf("a named model prices itself: %v", err)
	}
}

func TestMaxTokens_TooLargeWithoutStreamingIsPermanentAndNeverSent(t *testing.T) {
	if _, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", APIKey: "k", MaxTokens: 32000}); err == nil {
		t.Fatal("a cap the SDK will not send was accepted")
	}
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{})
	req := userRequest()
	req.MaxOutputTokens = 32000
	_, err := m.Generate(context.Background(), req)
	var tr agentrt.TransientError
	if err == nil || errors.As(err, &tr) || len(f.requests) != 0 {
		t.Fatalf("err = %v, requests = %d, want a permanent error and nothing sent", err, len(f.requests))
	}
}

func TestGenerate_ErrorsCarryNeitherTheRequestNorTheKey(t *testing.T) {
	for _, tc := range []struct {
		status    int
		transient bool
	}{{401, false}, {400, false}, {409, true}, {529, true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(tc.status)
			io.WriteString(w, "<html>bad gateway</html>")
		}))
		m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", BaseURL: srv.URL, APIKey: "sk-ant-SECRET"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Generate(context.Background(), userRequest())
		srv.Close()
		var tr agentrt.TransientError
		var se providers.StatusError
		var sdkErr *sdk.Error
		if !errors.As(err, &se) || se.Status != tc.status || se.Body != "<html>bad gateway</html>" || se.RetryAfter != 2*time.Second || errors.As(err, &sdkErr) {
			t.Fatalf("%d: err = %#v, want a StatusError and no SDK error", tc.status, err)
		}
		if errors.As(err, &tr) != tc.transient {
			t.Fatalf("%d: transient = %v", tc.status, !tc.transient)
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if out := fmt.Sprintf(format, err); strings.Contains(out, "SECRET") {
				t.Fatalf("%d: %s printed the key: %s", tc.status, format, out)
			}
		}
	}
}

func TestConfigAndModel_PrintWithoutTheKey(t *testing.T) {
	cfg := anthropic.Config{Model: "claude-opus-5", APIKey: "sk-ant-SECRET"}
	m, err := anthropic.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{cfg, *m, m} {
			if out := fmt.Sprintf(format, v); strings.Contains(out, "SECRET") {
				t.Fatalf("%s printed the key: %s", format, out)
			}
		}
	}
}

func TestGenerate_ResponseBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"content":[{"type":"text","text":"`+strings.Repeat("a", providers.MaxBodyBytes)+`"}]}`)
	}))
	t.Cleanup(srv.Close)
	m, _ := anthropic.New(anthropic.Config{Model: "claude-opus-5", BaseURL: srv.URL, APIKey: "k"})
	_, err := m.Generate(context.Background(), userRequest())
	var tr agentrt.TransientError
	if !errors.Is(err, providers.ErrBodyTooLarge) || errors.As(err, &tr) {
		t.Fatalf("err = %v, want a permanent too-large error", err)
	}
}

func TestGenerate_UndecodableReplyIsAServedError(t *testing.T) {
	for _, body := range []string{`<html>ok</html>`, `{"id":"m","usage":{"input_tokens":10`} {
		f := &fake{responses: []response{{200, body}}}
		m := newModel(t, f, anthropic.Config{})
		_, err := m.Generate(context.Background(), userRequest())
		var served agentrt.ServedError
		var tr agentrt.TransientError
		if !errors.As(err, &served) || errors.As(err, &tr) {
			t.Fatalf("%s: err = %#v, want a served error", body, err)
		}
	}
}

func TestGenerate_FoldsOneHourCacheWritesAtTwice(t *testing.T) {
	cached := `{"id":"m","type":"message","role":"assistant","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],
"usage":{"input_tokens":100,"output_tokens":10,"cache_creation_input_tokens":501,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":401,"ephemeral_1h_input_tokens":100}}}`
	f := &fake{responses: []response{{200, cached}}}
	resp, err := newModel(t, f, anthropic.Config{}).Generate(context.Background(), userRequest())
	if err != nil {
		t.Fatal(err)
	}
	// 100 uncached + ceil(1.25 * 401) = 502 + 2 * 100 = 200.
	if want := 100 + 502 + 200; resp.Usage.InputTokens != want {
		t.Fatalf("input = %d, want %d", resp.Usage.InputTokens, want)
	}
}

func TestGenerate_ReplayedToolInputKeepsItsNumbers(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, string(b))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, toolUseMessage)
	}))
	t.Cleanup(srv.Close)
	m, _ := anthropic.New(anthropic.Config{Model: "claude-opus-5", BaseURL: srv.URL, APIKey: "k"})
	raw := `{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"tool_use","id":"b","name":"t","input":{"n":9007199254740995}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{Messages: []agentrt.Message{
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "a", Name: "t", Input: json.RawMessage(`{"n":9007199254740993}`)}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "a", Content: "ok"}}},
		{Role: "assistant", Content: []agentrt.ContentBlock{{Type: m.RawAssistantBlock(), Text: raw}}},
		{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "b", Content: "ok"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"9007199254740993", "9007199254740995"} {
		if !strings.Contains(f.paths[0], `"n":`+n) {
			t.Fatalf("sent %s, want %s intact", f.paths[0], n)
		}
	}
}

func TestStrictSubset_PropertyNamesAreNotKeywords(t *testing.T) {
	in := map[string]any{}
	json.Unmarshal([]byte(`{"type":"object","properties":{"pattern":{"type":"string","pattern":"^a"},"maximum":{"type":"integer","maximum":3},"path":{"type":"string"},
		"opt":{"type":["object","null"],"properties":{"a":{"type":"string"}}},"mode":{"enum":[{"pattern":"kept"}],"default":{"maximum":1}}},
		"required":["pattern","path"],"$defs":{"x":{"type":"string","maxLength":3}}}`), &in)
	out, _ := json.Marshal(anthropic.StrictSubset(in))
	want := `{"$defs":{"x":{"type":"string"}},"additionalProperties":false,"properties":{"maximum":{"type":"integer"},"mode":{"default":{"maximum":1},"enum":[{"pattern":"kept"}]},` +
		`"opt":{"additionalProperties":false,"properties":{"a":{"type":"string"}},"type":["object","null"]},"path":{"type":"string"},"pattern":{"type":"string"}},"required":["pattern","path"],"type":"object"}`
	if string(out) != want {
		t.Fatalf("strict subset =\n%s\nwant\n%s", out, want)
	}
}

func TestStrict_CarriesTopLevelKeywordsSoARefResolves(t *testing.T) {
	f := &fake{responses: []response{{200, toolUseMessage}}}
	m := newModel(t, f, anthropic.Config{Strict: true})
	tool := agentrt.ToolSpec{Name: "t", Description: "d", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string","pattern":"^a"}},"description":"top","additionalProperties":true}`)}
	if _, err := m.Generate(context.Background(), userRequest(tool)); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(f.requests[0]["tools"].([]any)[0].(map[string]any)["input_schema"])
	want := `{"$defs":{"x":{"type":"string"}},"additionalProperties":false,"description":"top","properties":{"a":{"$ref":"#/$defs/x"}},"type":"object"}`
	if string(got) != want {
		t.Fatalf("sent %s, want %s", got, want)
	}
}

func TestStrict_RefusesWhatItCannotExpress(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
		`{"type":"object","properties":{"a":{"type":"object","additionalProperties":{"type":"string"}}}}`,
		`{"type":"object","properties":{"n":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"}}}}}`,
		`{"type":"object","properties":{"self":{"$ref":"#"}}}`,
		`{"type":"object","properties":{"a":{"$ref":"#/$defs/a"}},"$defs":{"a":{"items":{"$ref":"#/$defs/b"}},"b":{"anyOf":[{"$ref":"#/$defs/a"}]}}}`,
	} {
		f := &fake{responses: []response{{200, toolUseMessage}}}
		m := newModel(t, f, anthropic.Config{Strict: true})
		_, err := m.Generate(context.Background(), userRequest(agentrt.ToolSpec{Name: "t", InputSchema: json.RawMessage(schema)}))
		if err == nil || len(f.requests) != 0 {
			t.Errorf("%s: err = %v, requests = %d, want refused before sending", schema, err, len(f.requests))
		}
		// Without Strict the schema goes as it is.
		f = &fake{responses: []response{{200, toolUseMessage}}}
		if _, err := newModel(t, f, anthropic.Config{}).Generate(context.Background(), userRequest(agentrt.ToolSpec{Name: "t", InputSchema: json.RawMessage(schema)})); err != nil {
			t.Errorf("%s without strict: %v", schema, err)
		}
	}
}

// TestStrict_CaseworkSchemasAreByteIdentical is the proof that casework's
// strict tool definitions did not move: testdata holds every tool schema in
// casework/internal/tools and the tools array the adapter sent for them
// before the structural rewrite of StrictSubset.
func TestStrict_CaseworkSchemasAreByteIdentical(t *testing.T) {
	raw, err := os.ReadFile("testdata/casework_schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/casework_strict.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []struct{ Name, Schema string }
	if err := json.Unmarshal(raw, &list); err != nil || len(list) != 12 {
		t.Fatalf("%d schemas, %v", len(list), err)
	}
	var specs []agentrt.ToolSpec
	for _, s := range list {
		specs = append(specs, agentrt.ToolSpec{Name: s.Name, Description: "d", InputSchema: json.RawMessage(s.Schema)})
	}
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, toolUseMessage)
	}))
	t.Cleanup(srv.Close)
	m, err := anthropic.New(anthropic.Config{Model: "claude-opus-5", Name: "claude-opus-5", RawAssistantBlock: "casework.raw_assistant", Strict: true, Effort: "medium", APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), userRequest(specs...)); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Tools json.RawMessage `json:"tools"`
	}
	json.Unmarshal(body, &sent)
	if !bytes.Equal(sent.Tools, golden) {
		t.Fatalf("strict tools changed:\n%s\nwant\n%s", sent.Tools, golden)
	}
}
