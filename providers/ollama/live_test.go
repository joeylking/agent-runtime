//go:build live

// The live tests call a local Ollama server, which is free but is neither
// deterministic nor available in CI, so they are behind the live tag and
// skip when no server answers.
package ollama_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers/ollama"
)

// liveModel is the tool-calling model the live tests use.
const liveModel = "qwen3:30b-a3b"

// requireServer skips unless something is listening where the adapter would
// send its request.
func requireServer(t *testing.T) {
	t.Helper()
	host := ollama.Host("")
	addr := host[len("http://"):]
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("no Ollama server at %s: %v", host, err)
	}
	conn.Close()
}

func TestLive_ToolUseRoundTrip(t *testing.T) {
	requireServer(t)
	m, err := ollama.New(ollama.Config{Model: liveModel})
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
	t.Logf("%s: %s(%s) in=%d out=%d", m.Name(), resp.ToolUses[0].Name, resp.ToolUses[0].Args, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}
