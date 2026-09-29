package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	// Two quotes and the marker's 13 encoded bytes leave 49 for the text.
	got := text(t, res.Content)
	if len(res.Content) != 64 || got != strings.Repeat("x", 49)+truncationMarker {
		t.Errorf("content = %s (%d bytes), want 64 recorded bytes ending in the marker", res.Content, len(res.Content))
	}
	if res.Summary != "fs/read_file: 64 bytes, truncated" {
		t.Errorf("summary = %q, want the recorded size and the truncation", res.Summary)
	}
}

// TestCall_CapCountsEncodedBytes is the audit's probe: a control character
// is one byte of text and six of JSON, and the cap is on the JSON.
func TestCall_CapCountsEncodedBytes(t *testing.T) {
	f := newFake(t)
	f.add("ctl", "Control characters.", readSchema, nil, textHandler(strings.Repeat("\x01", 100)))
	f.add("runes", "Multibyte text.", readSchema, nil, textHandler(strings.Repeat("世", 100)))
	f.add("fits", "Exactly the cap.", readSchema, nil, textHandler(strings.Repeat("\x01", 16)))
	m := mustPin(t, f)
	server := f.server()
	server.MaxContentBytes = 100
	tools, rep, err := Load(context.Background(), server, m, Rules{
		"ctl":   {SideEffect: agentrt.ReadOnly},
		"runes": {SideEffect: agentrt.ReadOnly},
		"fits":  {SideEffect: agentrt.ReadOnly},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()

	for _, tl := range tools {
		res, err := call(t, tl, `{"path":"a"}`)
		if err != nil {
			t.Fatalf("%s: %v", tl.Spec().Name, err)
		}
		if len(res.Content) > 100 {
			t.Errorf("%s: %d recorded bytes, cap 100", tl.Spec().Name, len(res.Content))
		}
		if want := fmt.Sprintf(": %d bytes", len(res.Content)); !strings.Contains(res.Summary, want) {
			t.Errorf("%s: summary %q, want the recorded size", tl.Spec().Name, res.Summary)
		}
		got := text(t, res.Content)
		switch tl.Spec().Name {
		case "fs_fits":
			if got != strings.Repeat("\x01", 16) || strings.Contains(res.Summary, "truncated") {
				t.Errorf("fits: %q %q, want 98 bytes kept whole", got, res.Summary)
			}
		case "fs_runes":
			if !utf8.ValidString(got) || !strings.HasSuffix(got, truncationMarker) {
				t.Errorf("runes: %q, want whole runes and the marker", got)
			}
		default:
			if !strings.HasSuffix(got, truncationMarker) || !strings.Contains(res.Summary, "truncated") {
				t.Errorf("%s: %q %q, want it marked", tl.Spec().Name, got, res.Summary)
			}
		}
	}
}

func TestCapText_NeverExceedsTheCap(t *testing.T) {
	inputs := []string{
		strings.Repeat("a", 300),
		strings.Repeat("\x01", 300),
		strings.Repeat("é", 300),
		strings.Repeat("\"\\\n", 300),
		strings.Repeat("\u2028", 300),
		strings.Repeat("\xff", 300),
		strings.Repeat("😀a\x00", 300),
	}
	least := len(jsonString(truncationMarker))
	for _, in := range inputs {
		for max := 0; max < 200; max++ {
			raw, _, truncated := capText(in, max)
			if limit := maxInt(max, least); len(raw) > limit {
				t.Fatalf("%q cap %d: %d bytes", in[:8], max, len(raw))
			}
			if !truncated || !json.Valid(raw) {
				t.Fatalf("%q cap %d: truncated %v, valid %v", in[:8], max, truncated, json.Valid(raw))
			}
			var s string
			_ = json.Unmarshal(raw, &s)
			if !strings.HasSuffix(s, truncationMarker) {
				t.Fatalf("%q cap %d: %q has no marker", in[:8], max, s)
			}
			// The longest fitting prefix: one more rune would not fit.
			kept := strings.TrimSuffix(s, truncationMarker)
			if len(kept) < len(in) && utf8.ValidString(in) {
				_, size := utf8.DecodeRuneInString(in[len(kept):])
				if next := jsonString(in[:len(kept)+size] + truncationMarker); len(next) <= max {
					t.Fatalf("%q cap %d: kept %d bytes, %d more would fit", in[:8], max, len(kept), size)
				}
			}
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestCall_OversizedStructuredContentFallsBackToText is the audit's probe:
// structured content cut to fit became a JSON string. It now stays an
// object when it fits and is replaced by the text blocks when it does not,
// and the summary says which.
func TestCall_OversizedStructuredContentFallsBackToText(t *testing.T) {
	f := newFake(t)
	big := map[string]any{"k": strings.Repeat("x", 200)}
	f.add("with_text", "Structured and text.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: big, Content: []sdk.Content{&sdk.TextContent{Text: "summary text"}}}, nil
	})
	f.add("alone", "Structured only.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: big}, nil
	})
	f.add("small", "Structured that fits.", readSchema, nil, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: map[string]any{"k": 1}, Content: []sdk.Content{&sdk.TextContent{Text: "one"}}}, nil
	})
	m := mustPin(t, f)
	server := f.server()
	server.MaxContentBytes = 100
	tools, rep, err := Load(context.Background(), server, m, Rules{
		"with_text": {SideEffect: agentrt.ReadOnly},
		"alone":     {SideEffect: agentrt.ReadOnly},
		"small":     {SideEffect: agentrt.ReadOnly},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rep.Connection.Close()

	res, err := call(t, byName(t, tools, "fs_with_text"), `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := text(t, res.Content); got != "summary text" {
		t.Errorf("content = %q, want the text blocks", got)
	}
	if want := "fs/with_text: 14 bytes, truncated, structured content of 208 bytes dropped for the text"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}

	res, err = call(t, byName(t, tools, "fs_alone"), `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := text(t, res.Content); got != structuredDropped || !strings.Contains(res.Summary, "dropped") {
		t.Errorf("content = %q, summary = %q, want the drop said in both", got, res.Summary)
	}

	res, err = call(t, byName(t, tools, "fs_small"), `{"path":"a"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if string(res.Content) != `{"k":1}` || res.Summary != "fs/small: 7 bytes" {
		t.Errorf("content = %s, summary = %q, want the object kept", res.Content, res.Summary)
	}
}

// TestFixed_ACallThatSetsAFixedParameterFails is the audit's probe: the
// fixed value used to replace the model's silently, so the approval showed
// /etc and the server received /safe.
func TestFixed_ACallThatSetsAFixedParameterFails(t *testing.T) {
	f := newFake(t)
	f.add("read_file", "Read a file.", rootSchema, nil, echoHandler)
	m := mustPin(t, f)
	tools, _, err := load(t, f, m, Rules{"read_file": {
		SideEffect: agentrt.ReadOnly,
		Fixed:      map[string]json.RawMessage{"root": json.RawMessage(`"/safe"`)},
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = call(t, tools[0], `{"path":"p","root":"/etc"}`)
	if err == nil || !strings.Contains(err.Error(), `"root" is fixed`) {
		t.Errorf("err = %v, want the fixed parameter refused", err)
	}
}

// TestRules_HiddenNamesMustBeSoundToHide is the audit's probe: a misspelled
// Deny was accepted silently, and a denied name the schema's combinators
// still required was removed from properties alone.
func TestRules_HiddenNamesMustBeSoundToHide(t *testing.T) {
	const (
		anyOf     = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"anyOf":[{"required":["force"]},{"required":["path"]}]}`
		pattern   = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"patternProperties":{"^zz":{"type":"boolean"}}}`
		dependent = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"dependentRequired":{"path":["force"]}}`
		ifThen    = `{"type":"object","properties":{"path":{"type":"string"},"root":{"type":"string"}},"if":{"properties":{"path":{"const":"x"}}},"then":{"required":["root"]}}`
		defs      = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"$defs":{"f":{"properties":{"force":{"const":true}}}}}`
		nested    = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"},"opts":{"type":"object","properties":{"force":{"type":"boolean"}}}},"allOf":[{"required":["path"]}]}`
		unrelated = `{"type":"object","properties":{"path":{"type":"string"},"force":{"type":"boolean"}},"oneOf":[{"required":["path"]}]}`
	)
	f := newFake(t)
	f.add("misspelled", "w", writeSchema, nil, echoHandler)
	f.add("no_such_fixed", "w", writeSchema, nil, echoHandler)
	f.add("any_of", "w", anyOf, nil, echoHandler)
	f.add("pattern", "w", pattern, nil, echoHandler)
	f.add("dependent", "w", dependent, nil, echoHandler)
	f.add("if_then", "w", ifThen, nil, echoHandler)
	f.add("defs", "w", defs, nil, echoHandler)
	f.add("nested", "w", nested, nil, echoHandler)
	f.add("unrelated", "w", unrelated, nil, echoHandler)
	m := mustPin(t, f)

	deny := func() Rule { return Rule{SideEffect: agentrt.LocalMutation, Deny: []string{"force"}} }
	_, rep, err := load(t, f, m, Rules{
		"misspelled":    {SideEffect: agentrt.LocalMutation, Deny: []string{"forse"}},
		"no_such_fixed": {SideEffect: agentrt.LocalMutation, Fixed: map[string]json.RawMessage{"nonexistent": json.RawMessage(`1`)}},
		"any_of":        deny(),
		"pattern":       deny(),
		"dependent":     deny(),
		"if_then":       {SideEffect: agentrt.LocalMutation, Fixed: map[string]json.RawMessage{"root": json.RawMessage(`"/srv"`)}},
		"defs":          deny(),
		"nested":        deny(),
		"unrelated":     deny(),
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{
		"misspelled":    `hides "forse", which is not a top-level property`,
		"no_such_fixed": `hides "nonexistent", which is not a top-level property`,
		"any_of":        `input schema's anyOf mentions it`,
		"pattern":       "patternProperties",
		"dependent":     `input schema's dependentRequired mentions it`,
		"if_then":       `input schema's then mentions it`,
		"defs":          `input schema's $defs mentions it`,
	}
	for tool, reason := range want {
		if got := refusal(rep, tool); !strings.Contains(got, reason) {
			t.Errorf("%s: refusal %q, want %q", tool, got, reason)
		}
	}
	if len(rep.Registered) != 2 || rep.Registered[0].Tool != "nested" || rep.Registered[1].Tool != "unrelated" {
		t.Errorf("registered = %+v, want the two tools whose hidden name nothing else mentions", rep.Registered)
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
