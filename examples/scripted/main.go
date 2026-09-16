// Command scripted runs one deterministic agent run against in-memory tools
// and prints the persisted event trace. It makes no network calls.
//
//	go run ./examples/scripted [-db path]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

type counter struct {
	mu sync.Mutex
	n  int
}

type tool struct {
	spec agentrt.ToolSpec
	fn   func(agentrt.ToolCall) (agentrt.ToolResult, error)
}

func (t tool) Spec() agentrt.ToolSpec { return t.spec }
func (t tool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return t.fn(c)
}

func main() {
	dbPath := flag.String("db", "", "SQLite file (default: temporary file)")
	flag.Parse()
	if *dbPath == "" {
		*dbPath = filepath.Join(os.TempDir(), fmt.Sprintf("agentrt-example-%d.db", time.Now().UnixNano()))
	}
	if err := run(*dbPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dbPath string) error {
	store, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	c := &counter{}
	tools := []agentrt.Tool{
		tool{spec: agentrt.ToolSpec{Name: "read_counter", Description: "Read the counter.", InputSchema: []byte(`{"type":"object","additionalProperties":false}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
			fn: func(agentrt.ToolCall) (agentrt.ToolResult, error) {
				c.mu.Lock()
				defer c.mu.Unlock()
				return agentrt.ToolResult{Content: mustJSON(map[string]int{"value": c.n}), Summary: fmt.Sprintf("counter=%d", c.n)}, nil
			}},
		tool{spec: agentrt.ToolSpec{Name: "add", Description: "Add n to the counter.", InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":1}},"required":["n"],"additionalProperties":false}`), SideEffect: agentrt.LocalMutation, Timeout: time.Second},
			fn: func(call agentrt.ToolCall) (agentrt.ToolResult, error) {
				var args struct{ N int }
				if err := json.Unmarshal(call.Args, &args); err != nil {
					return agentrt.ToolResult{}, err
				}
				c.mu.Lock()
				defer c.mu.Unlock()
				c.n += args.N
				return agentrt.ToolResult{Content: mustJSON(map[string]int{"value": c.n}), Summary: fmt.Sprintf("added %d, counter=%d", args.N, c.n)}, nil
			}},
		tool{spec: agentrt.ToolSpec{Name: "wipe", Description: "Reset everything.", InputSchema: []byte(`{"type":"object","additionalProperties":false}`), SideEffect: agentrt.Destructive, Timeout: time.Second},
			fn: func(agentrt.ToolCall) (agentrt.ToolResult, error) {
				return agentrt.ToolResult{}, errors.New("must never run")
			}},
	}

	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read_counter", `{}`, "Inspect the current value."),
		scripted.ToolCall("add", `{"n":3}`, "Increase it."),
		scripted.ToolCall("add", `{"n":"three"}`, "A malformed request the runtime must reject."),
		scripted.ToolCall("wipe", `{}`, "A destructive request the policy must deny."),
		scripted.ToolCall("add", `{"n":2}`, "Increase it again."),
		scripted.Complete(`{"final_counter":5}`),
	}}

	driver, err := agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: tools,
		Observer: func(e agentrt.Event) {
			fmt.Printf("%s  %-22s %s\n", e.At.Local().Format("15:04:05.000"), e.Type, summarize(e))
		},
	})
	if err != nil {
		return err
	}

	fmt.Printf("database: %s\n\n", dbPath)
	run, err := driver.Start(context.Background(), "Bring the counter to 5 without wiping it.", agentrt.DefaultLimits())
	if err != nil {
		return err
	}
	fmt.Printf("\nrun %s: %s (%s) steps=%d result=%s\n", run.ID, run.Status, run.Reason, run.StepCount, run.Result)
	if c.n != 5 {
		return fmt.Errorf("counter is %d, expected 5", c.n)
	}
	return nil
}

func summarize(e agentrt.Event) string {
	var m map[string]any
	if json.Unmarshal(e.Payload, &m) != nil {
		return string(e.Payload)
	}
	switch e.Type {
	case agentrt.EventStepDecided:
		if m["kind"] == "tool_call" {
			return fmt.Sprintf("%s %s  — %s", m["tool"], compact(m["args"]), m["reason"])
		}
		return fmt.Sprint(m["kind"])
	case agentrt.EventStepPolicy:
		return fmt.Sprintf("%s: %s", m["outcome"], m["reason"])
	case agentrt.EventStepToolFinished:
		if s, ok := m["summary"]; ok {
			return fmt.Sprintf("%s (%vms)", s, m["duration_ms"])
		}
		return fmt.Sprintf("error: %s", m["error"])
	case agentrt.EventStepFailed:
		return fmt.Sprint(m["detail"])
	case agentrt.EventRunFinished:
		return fmt.Sprintf("%s %s", m["status"], m["reason"])
	}
	return ""
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
