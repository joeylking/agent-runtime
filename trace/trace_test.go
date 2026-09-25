package trace_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

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
	want := e.At.Local().Format("15:04:05.000") + "  " + agentrt.EventStepDecided
	if !strings.HasPrefix(line, want) {
		t.Fatalf("line = %q, want it to start with %q", line, want)
	}
	if !strings.HasSuffix(line, ` {"kind":"tool_call"}`) {
		t.Fatalf("line = %q, want the payload last", line)
	}
	// The type column is padded to 22 so the payloads line up.
	if got := strings.Index(line, `{`); got != len("15:04:05.000")+2+22+1 {
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
