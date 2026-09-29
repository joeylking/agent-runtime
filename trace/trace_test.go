package trace_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
)

func event(seq int64, typ, payload string) agentrt.Event {
	at := time.Date(2026, 9, 25, 14, 3, 4, 500_000_000, time.UTC)
	return agentrt.Event{Seq: seq, RunID: "r1", StepID: "s1", At: at, Type: typ, Payload: json.RawMessage(payload)}
}

func TestWriter_AlignedLine(t *testing.T) {
	var buf bytes.Buffer
	obs := trace.Writer(&buf)
	e := event(1, agentrt.EventStepDecided, `{"kind":"tool_call"}`)
	obs(e)
	line := strings.TrimSuffix(buf.String(), "\n")
	want := e.At.Local().Format("2006-01-02 15:04:05.000 -0700") + "  " + agentrt.EventStepDecided
	if !strings.HasPrefix(line, want) {
		t.Fatalf("line = %q, want it to start with %q", line, want)
	}
	if !strings.HasSuffix(line, ` {"kind":"tool_call"}`) {
		t.Fatalf("line = %q, want the payload last", line)
	}
	// The type column is padded to 22 so the payloads line up.
	prefix := e.At.Local().Format("2006-01-02 15:04:05.000 -0700")
	if got := strings.Index(line, `{`); got != len(prefix)+2+22+1 {
		t.Fatalf("payload starts at column %d in %q", got, line)
	}
}

func TestJSONL_OneObjectPerEvent(t *testing.T) {
	var buf bytes.Buffer
	obs := trace.JSONL(&buf)
	obs(event(1, agentrt.EventRunStarted, ""))
	obs(event(2, agentrt.EventStepPolicy, `{"reason":"a < b & c"}`))
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var first struct {
		Seq     int64           `json:"seq"`
		RunID   string          `json:"run_id"`
		StepID  string          `json:"step_id"`
		At      string          `json:"at"`
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || first.RunID != "r1" || first.StepID != "s1" || first.Type != agentrt.EventRunStarted || string(first.Payload) != "{}" {
		t.Fatalf("first = %+v", first)
	}
	if first.At != "2026-09-25T14:03:04.5Z" {
		t.Fatalf("at = %q, want RFC3339Nano", first.At)
	}
	if !strings.Contains(lines[1], `"reason":"a < b & c"`) {
		t.Fatalf("payload was rewritten: %s", lines[1])
	}
}

func TestTrace_ConcurrentEventsStayWhole(t *testing.T) {
	for _, obsFor := range []func(*bytes.Buffer) agentrt.Observer{
		func(b *bytes.Buffer) agentrt.Observer { return trace.Writer(b) },
		func(b *bytes.Buffer) agentrt.Observer { return trace.JSONL(b) },
	} {
		var buf bytes.Buffer
		obs := obsFor(&buf)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				obs(event(int64(i), agentrt.EventStepStarted, `{"index":0}`))
			}(i)
		}
		wg.Wait()
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) != 20 {
			t.Fatalf("got %d lines, want 20", len(lines))
		}
		for _, l := range lines {
			if strings.Count(l, agentrt.EventStepStarted) != 1 || strings.Count(l, `{"index":0}`) != 1 {
				t.Fatalf("interleaved line %q", l)
			}
		}
	}
}

// TestSanitize_TheAuditsExactStrings reproduces the probe's payloads: a
// model-supplied tool name and goal built to clear the screen, forge an
// APPROVAL WAITING block, and set the window title, plus a C1 CSI byte a
// policy's preview carried verbatim.
func TestSanitize_TheAuditsExactStrings(t *testing.T) {
	evil := "\x1b[2J\x1b[H\x1b[32mAPPROVAL WAITING  looks-safe\x1b[0m\rX"
	got := trace.Sanitize(evil)
	if strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("escape or bare CR survived: %q", got)
	}
	if !strings.Contains(got, `\x1b[2J`) {
		t.Fatalf("ESC was not escaped visibly: %q", got)
	}
	if !strings.Contains(got, "X") {
		t.Fatalf("trailing text lost: %q", got)
	}

	toolName := "no\x1b[31msuch\x1b]0;pwned\x07"
	got = trace.Sanitize(toolName)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("ESC/BEL survived: %q", got)
	}
	if !strings.Contains(got, `\x07`) {
		t.Fatalf("BEL was not escaped: %q", got)
	}

	// A C1 control (U+009B, CSI) delivered as an actual rune, not a
	// backslash-u escape typed by a model.
	c1 := "\u009b31m C1 CSI and \\u001b[2J"
	got = trace.Sanitize(c1)
	if strings.ContainsRune(got, 0x9B) {
		t.Fatalf("raw C1 byte survived: %q", got)
	}
	if !strings.Contains(got, `\x9b`) {
		t.Fatalf("C1 was not escaped: %q", got)
	}
	// The literal backslash-u text the model typed is not itself a control
	// byte and must pass through unchanged.
	if !strings.Contains(got, `\u001b[2J`) {
		t.Fatalf("literal text was altered: %q", got)
	}
}

func TestSanitize_NewlinesBecomeSpacesNotEscapes(t *testing.T) {
	got := trace.Sanitize("line one\nline two\r\nline three")
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("newline survived: %q", got)
	}
	if strings.Contains(got, `\x0a`) || strings.Contains(got, `\x0d`) {
		t.Fatalf("newline was escaped instead of spaced: %q", got)
	}
	if got != "line one line two  line three" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitize_InvalidUTF8BecomesReplacementCharacter(t *testing.T) {
	got := trace.Sanitize("valid \xff\xfe end")
	if !strings.Contains(got, "�") {
		t.Fatalf("invalid UTF-8 was not replaced: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("raw invalid bytes survived: %q", got)
	}
}

// TestWriter_SanitizesC1InThePayload: the payload is JSON, which escapes C0
// controls, but a C1 control such as U+009B is valid Unicode above the
// ASCII range and is not escaped by encoding/json, so it reaches the text
// writer as a raw two-byte UTF-8 sequence unless the writer sanitizes it.
func TestWriter_SanitizesC1InThePayload(t *testing.T) {
	var buf bytes.Buffer
	obs := trace.Writer(&buf)
	payload, err := json.Marshal(map[string]string{"preview": "\u009b31mCSI"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.ContainsRune(string(payload), 0x9B) {
		t.Fatalf("test payload does not actually carry a raw C1 byte: %q", payload)
	}
	obs(event(1, agentrt.EventStepPolicy, string(payload)))
	if strings.ContainsRune(buf.String(), 0x9B) {
		t.Fatalf("raw C1 byte reached the terminal: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `\x9b`) {
		t.Fatalf("C1 was not escaped: %q", buf.String())
	}
}

// TestJSONL_InvalidPayloadIsEmittedNotDropped: a payload that is not valid
// JSON must not vanish from the trace.
func TestJSONL_InvalidPayloadIsEmittedNotDropped(t *testing.T) {
	var buf bytes.Buffer
	obs := trace.JSONL(&buf)
	obs(event(1, agentrt.EventStepPolicy, "not json"))
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("event was dropped")
	}
	var rec struct {
		Seq            int64  `json:"seq"`
		Type           string `json:"type"`
		Payload        string `json:"payload"`
		InvalidPayload bool   `json:"invalid_payload"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v\n%s", err, line)
	}
	if !rec.InvalidPayload || rec.Payload != "not json" || rec.Seq != 1 {
		t.Fatalf("rec = %+v", rec)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriterWithErr_SurfacesAWriteFailure(t *testing.T) {
	wantErr := errors.New("disk full")
	obs, errFn := trace.WriterWithErr(failingWriter{wantErr})
	if errFn() != nil {
		t.Fatal("Err before any write must be nil")
	}
	obs(event(1, agentrt.EventStepStarted, `{}`))
	if err := errFn(); !errors.Is(err, wantErr) {
		t.Fatalf("Err() = %v, want %v", err, wantErr)
	}
}

func TestJSONLWithErr_SurfacesAWriteFailure(t *testing.T) {
	wantErr := errors.New("disk full")
	obs, errFn := trace.JSONLWithErr(failingWriter{wantErr})
	obs(event(1, agentrt.EventStepStarted, `{}`))
	if err := errFn(); !errors.Is(err, wantErr) {
		t.Fatalf("Err() = %v, want %v", err, wantErr)
	}
}
