package replay

import (
	"encoding/json"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
)

func toolUseRequest(input string) agentrt.ModelRequest {
	return agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "t1", Name: "get", Input: json.RawMessage(input)}}}}}
}

// Integers beyond float64 precision are distinct requests and must not
// share a recording.
func TestKey_LargeIntegersDoNotCollide(t *testing.T) {
	a, err := Key("m", toolUseRequest(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Key("m", toolUseRequest(`{"id":9007199254740992}`))
	if a == b {
		t.Fatal("requests differing in a large integer share a key")
	}
}

// Keys of existing recordings must not move: this one was computed before
// numbers kept their literal text. Every recording in both consumers was
// checked the same way when the change was made.
func TestKey_Pinned(t *testing.T) {
	req := agentrt.ModelRequest{System: "sys <b> & co", MaxOutputTokens: 4096, Tools: []agentrt.ToolSpec{{Name: "read", Description: "d", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","maximum":100}}}`), SideEffect: agentrt.ReadOnly}},
		Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "hello é 🙂"}}}, {Role: "assistant", Content: []agentrt.ContentBlock{{Type: "tool_use", ToolUseID: "t1", Name: "read", Input: json.RawMessage(`{"n":42,"f":0.5}`)}}}}}
	got, err := Key("claude-opus-5", req)
	if err != nil {
		t.Fatal(err)
	}
	if want := "8b7c5f45e2045e72856cd3d8b8a6a559cce84ca6ee2967fbebe279c80609078e"; got != want {
		t.Fatalf("key = %s, want %s", got, want)
	}
}
