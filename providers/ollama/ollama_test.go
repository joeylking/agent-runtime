package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/ollama"
)

// server returns a model pointed at a fake /api/chat, so the whole contract
// is tested without a server or a network.
func server(t *testing.T, handler func(w http.ResponseWriter, body map[string]any)) *ollama.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		handler(w, body)
	}))
	t.Cleanup(srv.Close)
	return newModel(t, ollama.Config{Model: "test-model", Host: srv.URL})
}

func newModel(t *testing.T, cfg ollama.Config) *ollama.Model {
	t.Helper()
	m, err := ollama.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNew_RequiresAModelAndNamesItself(t *testing.T) {
	if _, err := ollama.New(ollama.Config{}); err == nil {
		t.Fatal("an empty model id was accepted")
	}
	if got := newModel(t, ollama.Config{Model: "qwen3:30b-a3b"}).Name(); got != "ollama:qwen3:30b-a3b" {
		t.Fatalf("name = %s", got)
	}
	if got := newModel(t, ollama.Config{Model: "qwen3:30b-a3b", Name: "local"}).Name(); got != "local" {
		t.Fatalf("overridden name = %s", got)
	}
}

func TestHost_DefaultsAndPrefixesTheScheme(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	if got := ollama.Host(""); got != "http://"+ollama.DefaultHost {
		t.Fatalf("default host = %s", got)
	}
	t.Setenv("OLLAMA_HOST", "box:1234/")
	if got := ollama.Host(""); got != "http://box:1234" {
		t.Fatalf("environment host = %s", got)
	}
	if got := ollama.Host("https://box:443"); got != "https://box:443" {
		t.Fatalf("configured host = %s", got)
	}
}

func TestGenerate_MapsRequestAndToolCalls(t *testing.T) {
	var got map[string]any
	m := server(t, func(w http.ResponseWriter, body map[string]any) {
		got = body
		w.Write([]byte(`{"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":{"path":"main.go"}}}]},"done_reason":"stop","prompt_eval_count":142,"eval_count":30}`))
	})
	req := agentrt.ModelRequest{
		System: "sys",
		Messages: []agentrt.Message{
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "goal"}}},
			{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "step_0", Name: "list_candidates", Input: []byte(`{}`)}}},
			{Role: "user", Content: []agentrt.ContentBlock{{Type: "tool_result", ToolUseID: "step_0", Name: "list_candidates", Content: "[]"}}},
		},
		Tools:           []agentrt.ToolSpec{{Name: "read_file", Description: "read", InputSchema: []byte(`{"type":"object"}`)}},
		MaxOutputTokens: 512,
	}
	resp, err := m.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_file" || string(resp.ToolUses[0].Args) != `{"path":"main.go"}` || resp.ToolUses[0].ID != "call_1" {
		t.Fatalf("tool uses = %+v", resp.ToolUses)
	}
	if resp.Usage.InputTokens != 142 || resp.Usage.OutputTokens != 30 || resp.StopReason != "tool_use" || resp.Text != "thinking" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Raw) == 0 {
		t.Fatal("raw response not returned")
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" || msgs[2].(map[string]any)["role"] != "assistant" || msgs[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("messages = %v", msgs)
	}
	if name := msgs[3].(map[string]any)["tool_name"]; name != "list_candidates" {
		t.Fatalf("tool message lost its tool_name: %v", msgs[3])
	}
	if calls := msgs[2].(map[string]any)["tool_calls"].([]any); calls[0].(map[string]any)["function"].(map[string]any)["name"] != "list_candidates" {
		t.Fatalf("assistant tool_calls = %v", calls)
	}
	if got["model"] != "test-model" || got["stream"] != false || got["think"] != false {
		t.Fatalf("request fields = %v", got)
	}
	opts := got["options"].(map[string]any)
	if opts["temperature"].(float64) != 0 || opts["num_predict"].(float64) != 512 || opts["num_ctx"].(float64) != ollama.DefaultNumCtx {
		t.Fatalf("options = %v", opts)
	}
	tools := got["tools"].([]any)
	if tools[0].(map[string]any)["function"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestGenerate_TextOnlyAndMissingIDs(t *testing.T) {
	m := server(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Write([]byte(`{"message":{"role":"assistant","content":"just text","tool_calls":[{"function":{"name":"git_diff"}}]},"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID == "" || string(resp.ToolUses[0].Args) != "{}" {
		t.Fatalf("tool uses = %+v", resp.ToolUses)
	}
}

func TestGenerate_StopReasonsMapToTheRuntimeVocabulary(t *testing.T) {
	m := server(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Write([]byte(`{"message":{"role":"assistant","content":"done"},"done_reason":"stop","prompt_eval_count":5,"eval_count":2}`))
	})
	resp, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	if err != nil || resp.StopReason != "end_turn" {
		t.Fatalf("stop: %+v %v", resp, err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any) {
		w.Write([]byte(`{"message":{"role":"assistant","content":"half a c","tool_calls":[{"function":{"name":"add"}}]},"done_reason":"length","prompt_eval_count":5,"eval_count":512}`))
	})
	resp, err = m.Generate(context.Background(), agentrt.ModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "max_tokens" || len(resp.ToolUses) != 0 {
		t.Fatalf("a truncated reply must carry no tool use: %+v", resp)
	}
	if resp.Usage.OutputTokens != 512 {
		t.Fatalf("a truncated reply is still charged: %+v", resp.Usage)
	}
}

func TestGenerate_ErrorsClassified(t *testing.T) {
	var tr agentrt.TransientError
	m := server(t, func(w http.ResponseWriter, _ map[string]any) {
		http.Error(w, "overloaded", http.StatusServiceUnavailable)
	})
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); !errors.As(err, &tr) {
		t.Fatalf("5xx not transient: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any) { http.Error(w, "model not found", http.StatusNotFound) })
	_, err := m.Generate(context.Background(), agentrt.ModelRequest{})
	var se providers.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound || errors.As(err, &tr) {
		t.Fatalf("4xx should be a permanent status error: %v", err)
	}
	m = server(t, func(w http.ResponseWriter, _ map[string]any) { w.Write([]byte(`{"error":"context length exceeded"}`)) })
	if _, err := m.Generate(context.Background(), agentrt.ModelRequest{}); err == nil {
		t.Fatal("an error in a 200 body was ignored")
	}
	// A server that is not listening is transient: the caller retries.
	down := newModel(t, ollama.Config{Model: "x", Host: "127.0.0.1:1"})
	if _, err := down.Generate(context.Background(), agentrt.ModelRequest{}); !errors.As(err, &tr) {
		t.Fatalf("connection refused not transient: %v", err)
	}
}

func TestFree_PricesTheReportedName(t *testing.T) {
	m := newModel(t, ollama.Config{Model: "qwen3:30b-a3b"})
	prices := ollama.Free(m)
	if _, err := providers.PriceFor(prices, "ollama:qwen3:30b-a3b"); err != nil {
		t.Fatal(err)
	}
	if _, err := providers.PriceFor(prices, "qwen3:30b-a3b"); err == nil {
		t.Fatal("the bare model id must not be priced")
	}
	// A name override moves the price with it.
	named := newModel(t, ollama.Config{Model: "qwen3:30b-a3b", Name: "local"})
	if _, err := providers.PriceFor(ollama.Free(named), "local"); err != nil {
		t.Fatal(err)
	}
}
