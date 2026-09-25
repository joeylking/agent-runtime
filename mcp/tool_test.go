package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// call runs one tool with the arguments the model would have sent.
func call(t *testing.T, tl agentrt.Tool, args string) (agentrt.ToolResult, error) {
	t.Helper()
	return tl.Call(context.Background(), agentrt.ToolCall{RunID: "run", StepID: "step", Args: json.RawMessage(args)})
}

// text decodes content the adapter wrote as a JSON string.
func text(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(content, &s); err != nil {
		t.Fatalf("content %s is not a JSON string: %v", content, err)
	}
	return s
}

func TestDeny_RemovedFromTheSchemaAndRefusedAtCall(t *testing.T) {
	f := newFake(t)
	f.add("write_file", "Write a file.", writeSchema, nil, echoHandler)
	m := mustPin(t, f)

	tools, _, err := load(t, f, m, Rules{"write_file": {SideEffect: agentrt.LocalMutation, Deny: []string{"force"}}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	schema := string(tools[0].Spec().InputSchema)
	if strings.Contains(schema, "force") {
		t.Errorf("schema = %s, want force removed", schema)
	}
	if _, err := call(t, tools[0], `{"path":"a","text":"hi"}`); err != nil {
		t.Fatalf("call without the denied parameter: %v", err)
	}
	_, err = call(t, tools[0], `{"path":"a","text":"hi","force":true}`)
	if err == nil || !strings.Contains(err.Error(), `"force" is denied`) {
		t.Errorf("err = %v, want the denied parameter refused", err)
	}
}

func TestFixed_RemovedFromTheSchemaAndInjectedAtCall(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", rootSchema, nil, echoHandler)
	m := mustPin(t, f)

	tools, _, err := load(t, f, m, Rules{"read_file": {
		SideEffect: agentrt.ReadOnly,
		Fixed:      map[string]json.RawMessage{"root": json.RawMessage(`"/srv/sandbox"`)},
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	schema := string(tools[0].Spec().InputSchema)
	if strings.Contains(schema, "root") {
		t.Errorf("schema = %s, want root removed from properties and required", schema)
	}
	if !strings.Contains(schema, `"required":["path"]`) {
		t.Errorf("schema = %s, want path still required", schema)
	}
	res, err := call(t, tools[0], `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if sent := text(t, res.Content); !strings.Contains(sent, `"root":"/srv/sandbox"`) {
		t.Errorf("server received %s, want the fixed value injected", sent)
	}
}

func TestCall_StructuredAndTextResults(t *testing.T) {
	f := newFake(t)
	f.add("structured", "Structured.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: map[string]any{"lines": 2, "ok": true}}, nil
	})
	f.add("mixed", "Text and an image.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: "first"},
			&sdk.ImageContent{Data: []byte("not shown"), MIMEType: "image/png"},
			&sdk.TextContent{Text: "last"},
		}}, nil
	})
	m := mustPin(t, f)
	tools, _, err := load(t, f, m, Rules{
		"structured": {SideEffect: agentrt.ReadOnly},
		"mixed":      {SideEffect: agentrt.ReadOnly},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := call(t, byName(t, tools, "fs_structured"), `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := string(res.Content); got != `{"lines":2,"ok":true}` {
		t.Errorf("content = %s, want the structured content as JSON", got)
	}
	if res.Summary != "fs/structured: 21 bytes" {
		t.Errorf("summary = %q", res.Summary)
	}

	res, err = call(t, byName(t, tools, "fs_mixed"), `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := text(t, res.Content); got != "first\n[image omitted]\nlast" {
		t.Errorf("content = %q, want the text blocks joined and the image left out", got)
	}
}

func TestCall_IsErrorBecomesAnError(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "no such file"}}}, nil
	})
	m := mustPin(t, f)
	tools, _, err := load(t, f, m, Rules{"read_file": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	res, err := call(t, tools[0], `{"path":"a"}`)
	if err == nil {
		t.Fatal("an isError result must be an error, so the driver records tool_error")
	}
	if !strings.Contains(err.Error(), "no such file") || !strings.Contains(err.Error(), "error:") {
		t.Errorf("err = %v", err)
	}
	if res.Content != nil {
		t.Errorf("content = %s, want nothing on an error", res.Content)
	}
	if errors.As(err, &agentrt.ErrAbortRun{}) {
		t.Error("a server failure must never abort the run")
	}
}

func TestCall_OversizedContentIsTruncated(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", readSchema, nil, textHandler(strings.Repeat("x", 500)))
	m := mustPin(t, f)
	server := f.server()
	server.MaxContentBytes = 64
	tools, rep, err := Load(context.Background(), server, m, Rules{"read_file": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()

	res, err := call(t, tools[0], `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	got := text(t, res.Content)
	if got != strings.Repeat("x", 64)+truncationMarker {
		t.Errorf("content = %q, want 64 bytes and the marker", got)
	}
	if !strings.Contains(res.Summary, "truncated") {
		t.Errorf("summary = %q, want it to say so", res.Summary)
	}
}

func TestCall_InputRequiredIsRefused(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{InputRequests: sdk.InputRequestMap{
			"which": &sdk.ElicitParams{Message: "which file?"},
		}}, nil
	})
	m := mustPin(t, f)
	tools, _, err := load(t, f, m, Rules{"read_file": {SideEffect: agentrt.ReadOnly}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, err = call(t, tools[0], `{"path":"a"}`)
	if err == nil || !strings.Contains(err.Error(), "asked for more input") {
		t.Errorf("err = %v, want an input-required result refused", err)
	}
}

func TestCall_TimeoutIsHonoured(t *testing.T) {
	f := newFake(t)
	f.add("slow", "Never answers.", readSchema, nil, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	m := mustPin(t, f)
	tools, _, err := load(t, f, m, Rules{"slow": {SideEffect: agentrt.ReadOnly, Timeout: 50 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	start := time.Now()
	if _, err := call(t, tools[0], `{"path":"a"}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the rule's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("call took %s, want it bounded by the rule", elapsed)
	}
}

func byName(t *testing.T, tools []agentrt.Tool, name string) agentrt.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Spec().Name == name {
			return tl
		}
	}
	t.Fatalf("no tool named %q in %v", name, names(tools))
	return nil
}
