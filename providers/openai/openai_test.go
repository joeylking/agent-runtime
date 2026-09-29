package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/openai"
)

// server returns a model pointed at a fake /chat/completions, so the whole
// contract is tested without a provider.
func server(t *testing.T, handler func(w http.ResponseWriter, body map[string]any, r *http.Request)) *openai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		handler(w, body, r)
	}))
	t.Cleanup(srv.Close)
	return newModel(t, openai.Config{Model: "test-model", BaseURL: srv.URL + "/v1", APIKey: "test-key"})
}

func newModel(t *testing.T, cfg openai.Config) *openai.Model {
	t.Helper()
	m, err := openai.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const toolCallResponse = `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"looking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":142,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":64}}}`

func TestNew_RequiresAModelAndNamesItself(t *testing.T) {
	if _, err := openai.New(openai.Config{}); err == nil {
		t.Fatal("an empty model id was accepted")
	}
	if got := newModel(t, openai.Config{Model: "gpt-oss:20b"}).Name(); got != "openai:gpt-oss:20b" {
		t.Fatalf("name = %s", got)
	}
	if got := newModel(t, openai.Config{Model: "gpt-oss:20b", Name: "local"}).Name(); got != "local" {
		t.Fatalf("overridden name = %s", got)
	}
}

func TestGenerate_MapsRequestToolCallsAndUsage(t *testing.T) {
	var got map[string]any
	var auth string
	m := server(t, func(w http.ResponseWriter, body map[string]any, r *http.Request) {
		got, auth = body, r.Header.Get("Authorization")
		w.Write([]byte(toolCallResponse))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{
		System: "sys",
		Messages: []agentrt.Message{
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "goal"}}},
			{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "text", Text: "why"}, {Type: "tool_use", ToolUseID: "step_0", Name: "list", Input: nil}}},
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "step_0", Name: "list", Content: "[]"}}},
		},
		Tools:           []agentrt.ToolSpec{{Name: "read_file", Description: "read", InputSchema: []byte(`{"type":"object"}`)}},
		MaxOutputTokens: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID != "call_1" || resp.ToolUses[0].Name != "read_file" || string(resp.ToolUses[0].Args) != `{"path":"main.go"}` {
		t.Fatalf("tool uses = %+v", resp.ToolUses)
	}
	if resp.StopReason != "tool_use" || resp.Text != "looking" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (agentrt.Usage{InputTokens: 142, OutputTokens: 30, CachedInputTokens: 64}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Fatal("raw response not returned")
	}
	if auth != "Bearer test-key" {
		t.Fatalf("authorization = %q", auth)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "sys" {
		t.Fatalf("system message = %v", msgs[0])
	}
	assistant := msgs[2].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if assistant["role"] != "assistant" || call["type"] != "function" || call["id"] != "step_0" {
		t.Fatalf("assistant message = %v", assistant)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "list" || fn["arguments"] != "{}" {
		t.Fatalf("empty input must be sent as {}: %v", fn)
	}
	result := msgs[3].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "step_0" || result["content"] != "[]" {
		t.Fatalf("tool result message = %v", result)
	}
	if got["tool_choice"] != "auto" || got["parallel_tool_calls"] != false || got["temperature"].(float64) != 0 || got["max_tokens"].(float64) != 512 {
		t.Fatalf("request fields = %v", got)
	}
	tools := got["tools"].([]any)
	fn = tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestGenerate_NoToolsSendsNoToolChoice(t *testing.T) {
	var got map[string]any
	m := server(t, func(w http.ResponseWriter, body map[string]any, _ *http.Request) {
		got = body
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "end_turn" || resp.Text != "hello" {
		t.Fatalf("resp = %+v", resp)
	}
	if _, ok := got["tool_choice"]; ok {
		t.Fatalf("tool_choice sent without tools: %v", got)
	}
	if _, ok := got["parallel_tool_calls"]; ok {
		t.Fatalf("parallel_tool_calls sent without tools: %v", got)
	}
}

func TestGenerate_FinishReasonsMapAndDropPartialCalls(t *testing.T) {
	cases := []struct {
		finish, want string
		toolUses     int
	}{
		{"tool_calls", "tool_use", 1},
		{"stop", "end_turn", 1},
		{"length", "max_tokens", 0},
		{"content_filter", "refusal", 0},
	}
	for _, c := range cases {
		m := server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x","tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"{\"n\":1}"}}]},"finish_reason":"` + c.finish + `"}],"usage":{"prompt_tokens":7,"completion_tokens":11}}`))
		})
		resp, err := m.Generate(context.Background(), agentrt.ModelRequest{})
		if err != nil {
			t.Fatalf("%s: %v", c.finish, err)
		}
		if resp.StopReason != c.want || len(resp.ToolUses) != c.toolUses {
			t.Fatalf("%s: stop %q tool uses %d", c.finish, resp.StopReason, len(resp.ToolUses))
		}
		if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 11 {
			t.Fatalf("%s: a served reply is charged whatever it stopped for: %+v", c.finish, resp.Usage)
		}
	}
}

func TestGenerate_ArgumentsMustBeJSON(t *testing.T) {
	m := server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"add","arguments":"n=1"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
	})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err == nil {
		t.Fatal("arguments that are not JSON were accepted")
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"add","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || string(resp.ToolUses[0].Args) != "{}" || resp.ToolUses[0].ID == "" {
		t.Fatalf("empty arguments and a missing id: %+v", resp.ToolUses)
	}
}

func TestGenerate_ErrorsClassified(t *testing.T) {
	var tr agentrt.TransientError
	m := server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); !errors.As(err, &tr) {
		t.Fatalf("429 not transient: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		http.Error(w, "no such model", http.StatusBadRequest)
	})
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	var se providers.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest || errors.As(err, &tr) {
		t.Fatalf("400 should be a permanent status error: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
	})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err == nil {
		t.Fatal("an error in a 200 body was ignored")
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) { w.Write([]byte(`{"choices":[]}`)) })
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err == nil {
		t.Fatal("a response with no choice was accepted")
	}
	down := newModel(t, openai.Config{Model: "x", BaseURL: "http://127.0.0.1:1/v1"})
	if _, err := down.Generate(context.Background(), agentrt.ModelRequest{}); !errors.As(err, &tr) {
		t.Fatalf("connection refused not transient: %v", err)
	}
}

func TestGenerate_NoKeySendsNoAuthorization(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)
	m := newModel(t, openai.Config{Model: "local", BaseURL: srv.URL + "/v1"})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Fatalf("a local server was sent %q", auth)
	}
}

func TestNew_EnvironmentKeyGoesOnlyToOpenAI(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-env")
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)
	m := newModel(t, openai.Config{Model: "x", BaseURL: srv.URL + "/v1"})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	// An explicit key still goes to the configured server, which is loopback.
	m = newModel(t, openai.Config{Model: "x", BaseURL: srv.URL + "/v1", APIKey: "sk-explicit"})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if auth[0] != "" || auth[1] != "Bearer sk-explicit" {
		t.Fatalf("authorization sent = %q, want none from the environment", auth)
	}
	if strings.Contains(fmt.Sprint(newModel(t, openai.Config{Model: "x"})), "sk-from-env") {
		t.Fatal("the default endpoint's model printed the environment key")
	}
}

func TestNew_RefusesAKeyOverPlainHTTPToARemoteHost(t *testing.T) {
	for _, base := range []string{"http://api.example.com/v1", "http://10.0.0.5:8080/v1"} {
		if _, err := openai.New(openai.Config{Model: "x", BaseURL: base, APIKey: "sk-x"}); err == nil {
			t.Errorf("%s: a key over http was accepted", base)
		}
		if _, err := openai.New(openai.Config{Model: "x", BaseURL: base}); err != nil {
			t.Errorf("%s without a key: %v", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:11434/v1", "http://localhost:8080/v1", "http://[::1]:1/v1", "https://api.example.com/v1"} {
		if _, err := openai.New(openai.Config{Model: "x", BaseURL: base, APIKey: "sk-x"}); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
	if _, err := openai.New(openai.Config{Model: "x", BaseURL: "ftp://x/v1"}); err == nil {
		t.Error("an ftp base URL was accepted")
	}
}

func TestConfigAndModel_PrintWithoutTheKey(t *testing.T) {
	cfg := openai.Config{Model: "x", APIKey: "sk-SECRET"}
	m, err := openai.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{cfg, *m, m} {
			if out := fmt.Sprintf(format, v); strings.Contains(out, "sk-SECRET") {
				t.Fatalf("%s printed the key: %s", format, out)
			}
		}
	}
}

func TestGenerate_UnusableReplyIsChargedWhatItReported(t *testing.T) {
	const counted = `"usage":{"prompt_tokens":5000,"completion_tokens":300}`
	for _, body := range []string{
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{\"a\": 1"}}]},"finish_reason":"tool_calls"}],` + counted + `}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"[1]"}}]},"finish_reason":"tool_calls"}],` + counted + `}`,
		`{"choices":[],` + counted + `}`,
		`{"error":{"type":"server_error","message":"bad"},` + counted + `}`,
		`{"choices":7,` + counted + `}`,
	} {
		m := server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) { io.WriteString(w, body) })
		_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
		var served agentrt.ServedError
		if !errors.As(err, &served) || served.Usage.InputTokens != 5000 || served.Usage.OutputTokens != 300 {
			t.Fatalf("%s: err = %#v, want a served error carrying the usage", body, err)
		}
	}
}

func TestGenerate_EchoedArgumentsAreBounded(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	m := server(t, func(w http.ResponseWriter, _ map[string]any, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"`+long+`"}}]},"finish_reason":"tool_calls"}],"usage":{}}`)
	})
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	if err == nil || len(err.Error()) > 2048 {
		t.Fatalf("error is %d bytes", len(fmt.Sprint(err)))
	}
}

// hijackMidBody starts a server that answers the status line and headers,
// writes part of the promised body, then cuts the connection: the request
// was received, so a reply may have been billed, but no answer came back.
func hijackMidBody(t *testing.T) *openai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n")
		buf.WriteString(`{"id":"chatcmpl-1","choices":[{"index":0`)
		buf.Flush()
		conn.Close()
	}))
	t.Cleanup(srv.Close)
	return newModel(t, openai.Config{Model: "test-model", BaseURL: srv.URL + "/v1", APIKey: "test-key"})
}

func TestGenerate_MidBodyFailureIsAmbiguousNotPermanent(t *testing.T) {
	m := hijackMidBody(t)
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	var tr agentrt.TransientError
	var se providers.StatusError
	var served agentrt.ServedError
	if err == nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
	if errors.As(err, &tr) || errors.As(err, &se) || errors.As(err, &served) {
		t.Fatalf("err = %#v, want it bare so the accounting caller charges it as ambiguous, not retried or classified", err)
	}
}

func TestFree_PricesTheReportedName(t *testing.T) {
	m, err := openai.New(openai.Config{Model: "gpt-oss:20b"})
	if err != nil {
		t.Fatal(err)
	}
	prices := openai.Free(m)
	if _, err := providers.PriceFor(prices, "openai:gpt-oss:20b"); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, "gpt-oss:20b"); err == nil {
		t.Fatal("the bare model id must not be priced")
	}
}
