// Package trace provides the two observers an operator wants while a run
// executes: aligned lines for a terminal and JSON Lines for a collector.
// Both are safe for concurrent use, because the runtime delivers events
// from whichever goroutine committed them.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
)

// textTimeLayout prints the aligned line's timestamp with date, time, and
// zone: a time with no date is ambiguous across midnight and no zone is
// ambiguous across a operator's and a server's own local time.
const textTimeLayout = "2006-01-02 15:04:05.000 -0700"

// Sanitize makes a string that may originate from a model or a remote server
// safe to print to a terminal. Invalid UTF-8 becomes the replacement
// character. C0 controls, DEL, and C1 controls (U+0080-U+009F) become a
// visible \xHH escape, because left alone they can clear the screen, move
// the cursor, set the window title, or forge output such as a fake
// "APPROVAL WAITING" block. A newline becomes a space instead of an escape,
// so a field stays one line rather than growing a control-code footprint.
// JSON output is never passed through Sanitize: it escapes C0 already and a
// consumer parsing JSON is not a terminal.
func Sanitize(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			b.WriteByte(' ')
		case needsEscape(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// needsEscape reports whether r is a C0 control (including DEL) or a C1
// control, the two ranges a terminal interprets rather than displays.
func needsEscape(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
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
