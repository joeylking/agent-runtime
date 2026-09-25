//go:build live

// The live test calls a local OpenAI-compatible server, which is free but
// is neither deterministic nor available in CI, so it is behind the live tag
// and skips when no server answers. Ollama serves this shape at /v1, which
// is what the test uses.
package openai_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers/openai"
)

const (
	liveModel = "qwen3:30b-a3b"
	liveHost  = "127.0.0.1:11434"
)

// liveBaseURL is Ollama's OpenAI-compatible endpoint, from OLLAMA_HOST or
// the default local address. It skips when nothing is listening there.
func liveBaseURL(t *testing.T) string {
	t.Helper()
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = liveHost
	}
	host = strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(host, "http://"), "https://"), "/")
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Skipf("no OpenAI-compatible server at %s: %v", host, err)
	}
	conn.Close()
	return "http://" + host + "/v1"
}

func TestLive_ToolUseRoundTrip(t *testing.T) {
	base := liveBaseURL(t)
	m, err := openai.New(openai.Config{Model: liveModel, BaseURL: base})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	resp, err := m.Generate(ctx, agentrt.ModelRequest{
		System:   "You act only by calling a tool. Never answer in prose.",
		Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "Add 3 to the counter."}}}},
		Tools: []agentrt.ToolSpec{{Name: "add", Description: "Add n to the counter.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)}},
		MaxOutputTokens: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" || len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "add" {
		t.Fatalf("resp = %+v", resp)
	}
	var args struct{ N int }
	if err := json.Unmarshal(resp.ToolUses[0].Args, &args); err != nil || args.N != 3 {
		t.Fatalf("args = %s (%v)", resp.ToolUses[0].Args, err)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 || len(resp.Raw) == 0 {
		t.Fatalf("usage or raw missing: %+v", resp)
	}
	t.Logf("%s at %s: %s(%s) in=%d out=%d", m.Name(), base, resp.ToolUses[0].Name, resp.ToolUses[0].Args, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
