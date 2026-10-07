// Package trace provides the two observers an operator wants while a run
// executes: aligned lines for a terminal and JSON Lines for a collector.
// Both are safe for concurrent use, because the runtime delivers events
// from whichever goroutine committed them. WriteRecheck renders a re-check
// report (agentrt.Recheck) for the same terminal.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// textTimeLayout prints the aligned line's timestamp with date, time, and
// zone: a time with no date is ambiguous across midnight and no zone is
// ambiguous across a operator's and a server's own local time.
const textTimeLayout = "2006-01-02 15:04:05.000 -0700"

// maxCombining is how many combining marks are kept on one base character
// before the rest are cut. A handful is enough for any real script (Hindi
// and Thai stack at most two or three); beyond that the only use is to grow
// a line's on-screen height, or hide text under a pile of marks.
const maxCombining = 4

// Sanitize makes a string that may originate from a model or a remote server
// safe to print to a terminal. Invalid UTF-8 becomes the replacement
// character. C0 controls, DEL, and C1 controls (U+0080-U+009F) become a
// visible \xHH escape, because left alone they can clear the screen, move
// the cursor, set the window title, or forge output such as a fake
// "APPROVAL WAITING" block. Bidirectional overrides and isolates, zero-width
// and other invisible formatting characters, the line and paragraph
// separators, and the Unicode tag block become a visible \u{XXXX} escape,
// because none of them execute anything but all of them change how the
// surrounding text is displayed without changing what it is: a bidi
// override can make "rm -rf ~" read as something else, and an invisible
// character can hide inside what looks like a short, safe argument. A run
// of more than maxCombining combining marks on one base character is cut,
// leaving the first maxCombining and a marker for how many were dropped, so
// a base character cannot be turned into a multi-line block of glyphs.
// Ordinary non-Latin text, emoji, and legitimate combining sequences are
// left exactly as given. A newline becomes a space instead of an escape, so
// a field stays one line rather than growing a control-code footprint.
// JSON output is never passed through Sanitize: it escapes C0 already and a
// consumer parsing JSON is not a terminal.
func Sanitize(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, needsEscape) && !strings.ContainsFunc(s, needsWideEscape) && !strings.ContainsFunc(s, unicode.IsMark) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	var marks []rune
	flushMarks := func() {
		if len(marks) == 0 {
			return
		}
		n := len(marks)
		if n > maxCombining {
			n = maxCombining
		}
		for _, m := range marks[:n] {
			b.WriteRune(m)
		}
		if dropped := len(marks) - n; dropped > 0 {
			fmt.Fprintf(&b, "[+%d combining marks dropped]", dropped)
		}
		marks = marks[:0]
	}
	for _, r := range s {
		if unicode.IsMark(r) {
			marks = append(marks, r)
			continue
		}
		flushMarks()
		switch {
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
		case needsEscape(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case needsWideEscape(r):
			fmt.Fprintf(&b, `\u{%04x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	flushMarks()
	return b.String()
}

// needsEscape reports whether r is a C0 control (including DEL) or a C1
// control, the two ranges a terminal interprets rather than displays.
func needsEscape(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// needsWideEscape reports whether r changes how surrounding text is
// displayed without being a C0/C1 control: a bidirectional override or
// isolate, a zero-width or other invisible formatting character, a line or
// paragraph separator (valid inside a Go string but not inside the one
// terminal line Sanitize's caller prints it as), or a character from the
// Unicode tag block (historically used for language tags, and since reused
// to smuggle invisible instructions into text a reviewer only skims).
func needsWideEscape(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x061C: // Arabic letter mark
		return true
	case r == 0x200E || r == 0x200F: // LRM, RLM
		return true
	case r >= 0x200B && r <= 0x200D: // zero width space/non-joiner/joiner
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner .. invisible plus
		return true
	case r == 0xFEFF: // byte order mark / zero width no-break space
		return true
	case r == 0x00AD: // soft hyphen
		return true
	case r == 0x180E: // Mongolian vowel separator
		return true
	case r == 0x2028 || r == 0x2029: // line separator, paragraph separator
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tag block
		return true
	default:
		return false
	}
}

// Writer returns an observer printing one line per event: the local date,
// time, and zone, two spaces, the event type padded to 22 columns, and the
// payload. This is the format both consumers print to stderr with -trace.
// The payload is JSON and already escapes C0 controls, but not C1 controls
// or DEL, so it is sanitized before it reaches the terminal.
func Writer(w io.Writer) agentrt.Observer {
	obs, _ := WriterWithErr(w)
	return obs
}

// WriterWithErr is Writer plus a way to learn whether a write to w failed.
// Err returns the first write error, if any; call it after the run whose
// events are being traced has finished.
func WriterWithErr(w io.Writer) (obs agentrt.Observer, errFn func() error) {
	var mu sync.Mutex
	var firstErr error
	obs = func(e agentrt.Event) {
		line := fmt.Sprintf("%s  %-22s %s\n", e.At.Local().Format(textTimeLayout), e.Type, Sanitize(string(e.Payload)))
		mu.Lock()
		defer mu.Unlock()
		if _, err := io.WriteString(w, line); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	errFn = func() error {
		mu.Lock()
		defer mu.Unlock()
		return firstErr
	}
	return obs, errFn
}

// jsonlRecord is one JSON Lines record: an event with its payload passed
// through as recorded.
type jsonlRecord struct {
	Seq     int64           `json:"seq"`
	RunID   string          `json:"run_id"`
	StepID  string          `json:"step_id,omitempty"`
	At      string          `json:"at"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// invalidPayloadRecord is emitted in place of jsonlRecord when the event's
// payload will not encode as JSON: the payload survives as a string rather
// than being dropped, and invalid_payload marks that it is not the
// original structured form.
type invalidPayloadRecord struct {
	Seq            int64  `json:"seq"`
	RunID          string `json:"run_id"`
	StepID         string `json:"step_id,omitempty"`
	At             string `json:"at"`
	Type           string `json:"type"`
	Payload        string `json:"payload"`
	InvalidPayload bool   `json:"invalid_payload"`
}

// JSONL returns an observer writing one JSON object per event. The payload
// is passed through as recorded: the encoder's HTML escaping is off, so a
// payload containing <, >, or & is not rewritten on its way out. An event
// whose payload will not encode as JSON is not dropped: it is emitted with
// the payload as a JSON string and invalid_payload set to true.
func JSONL(w io.Writer) agentrt.Observer {
	obs, _ := JSONLWithErr(w)
	return obs
}

// JSONLWithErr is JSONL plus a way to learn whether a write to w failed.
// Err returns the first write error, if any; call it after the run whose
// events are being traced has finished.
func JSONLWithErr(w io.Writer) (obs agentrt.Observer, errFn func() error) {
	var mu sync.Mutex
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	obs = func(e agentrt.Event) {
		payload := e.Payload
		if len(bytes.TrimSpace(payload)) == 0 {
			payload = json.RawMessage("{}")
		}
		at := e.At.Format(time.RFC3339Nano)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		err := enc.Encode(jsonlRecord{Seq: e.Seq, RunID: e.RunID, StepID: e.StepID, At: at, Type: e.Type, Payload: payload})
		if err != nil {
			buf.Reset()
			enc = json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err = enc.Encode(invalidPayloadRecord{Seq: e.Seq, RunID: e.RunID, StepID: e.StepID, At: at, Type: e.Type, Payload: string(payload), InvalidPayload: true}); err != nil {
				mu.Lock()
				note(err)
				mu.Unlock()
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		_, werr := w.Write(buf.Bytes())
		note(werr)
	}
	errFn = func() error {
		mu.Lock()
		defer mu.Unlock()
		return firstErr
	}
	return obs, errFn
}
