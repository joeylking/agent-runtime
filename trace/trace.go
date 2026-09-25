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
	"sync"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// Writer returns an observer printing one line per event: the local time,
// two spaces, the event type padded to 22 columns, and the payload. This is
// the format both consumers print to stderr with -trace.
func Writer(w io.Writer) agentrt.Observer {
	var mu sync.Mutex
	return func(e agentrt.Event) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s  %-22s %s\n", e.At.Local().Format("15:04:05.000"), e.Type, string(e.Payload))
	}
}

// JSONL returns an observer writing one JSON object per event. The payload
// is passed through as recorded: the encoder's HTML escaping is off, so a
// payload containing <, >, or & is not rewritten on its way out.
func JSONL(w io.Writer) agentrt.Observer {
	var mu sync.Mutex
	return func(e agentrt.Event) {
		payload := e.Payload
		if len(bytes.TrimSpace(payload)) == 0 {
			payload = json.RawMessage("{}")
		}
		rec := struct {
			Seq     int64           `json:"seq"`
			RunID   string          `json:"run_id"`
			StepID  string          `json:"step_id,omitempty"`
			At      string          `json:"at"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}{Seq: e.Seq, RunID: e.RunID, StepID: e.StepID, At: e.At.Format(time.RFC3339Nano), Type: e.Type, Payload: payload}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(rec); err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Write(buf.Bytes())
	}
}
