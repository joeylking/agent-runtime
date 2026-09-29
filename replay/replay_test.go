package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

type fixed struct{ resp agentrt.ModelResponse }

func (fixed) Name() string { return "m" }
func (f fixed) Generate(context.Context, agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	return f.resp, nil
}

// A recording is written owner-only, through a file the recorder created
// itself, into a directory it creates owner-only: a name prepared in the
// directory beforehand is not written through, and nothing is left beside
// the recordings. What it wrote replays, under the key it always had.
func TestRecorder_WritesOwnerOnlyThroughItsOwnFile(t *testing.T) {
	req := toolUseRequest(`{"n":1}`)
	key, _ := Key("m", req)
	inner := fixed{agentrt.ModelResponse{Text: "hi <b>", Raw: json.RawMessage(`{"a": 1}`)}}

	dir := filepath.Join(t.TempDir(), "rec")
	if _, err := (&Recorder{Inner: inner, Dir: dir}).Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode %v", fi.Mode())
		}
		if fi, err := os.Stat(filepath.Join(dir, key+".json")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("recording: %v, %v", fi, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 || entries[0].Name() != key+".json" {
		t.Fatalf("directory holds %v", entries)
	}
	got, err := (&Replayer{ModelName: "m", Dir: dir}).Generate(context.Background(), req)
	if err != nil || got.Text != "hi <b>" || string(got.Raw) != `{"a": 1}` {
		t.Fatalf("replayed %+v, %v", got, err)
	}

	// The name the recorder used to write through, prepared by someone
	// else as a link to a file of the operator's.
	shared := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o600)
	if err := os.Symlink(victim, filepath.Join(shared, key+".tmp")); err != nil {
		t.Skip("no symbolic links here")
	}
	if _, err := (&Recorder{Inner: inner, Dir: shared}).Generate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("the recorder wrote through a prepared link: victim = %q", b)
	}
}
