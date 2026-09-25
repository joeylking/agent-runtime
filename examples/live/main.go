// Command live runs one agent against a real local model, with in-memory
// tools and a policy that stops the run for an operator before the counter
// is changed. It is the shortest complete consumer: an Agent built from
// render, three tools, a policy, and a terminal tool to finish on.
//
//	go run ./examples/live
//	go run ./cmd/agentrt -db <path> approve <run>
//	go run ./examples/live -db <path> -resume <run>
//
// The default model is Ollama's own API; -model openai:<id> with -base-url
// pointing at Ollama's /v1 runs the same thing through the
// OpenAI-compatible adapter. The database defaults to a fixed path in the
// temporary directory so the three commands above address the same run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/providers"
	"github.com/joeylking/agent-runtime/providers/ollama"
	"github.com/joeylking/agent-runtime/providers/openai"
	"github.com/joeylking/agent-runtime/render"
	"github.com/joeylking/agent-runtime/trace"
)

const goal = "Bring the counter to 5 without wiping it, then finish."

// system is the whole prompt: the runtime supplies the tools, the renderer
// supplies the transcript, and the policy is not negotiable, so there is
// nothing else to say to the model.
const system = `You control a counter through tools.
- Every reply is exactly one tool call.
- Read the counter before you change it.
- Reach the target in as few changes as possible: one add of the whole difference.
- Never call wipe.
- When the counter has reached the target, call finish with its value.
A denied or failed call is answered with its reason; read it and adapt.`

// maxOutputTokens is generous because a local model narrates its reasoning
// into the reply before the tool call, and a reply cut off mid-call costs a
// step: the runtime records it as truncated and nudges on the next one.
const maxOutputTokens = 4096

func main() {
	dbPath := flag.String("db", filepath.Join(os.TempDir(), "agentrt-live.db"), "SQLite file")
	modelName := flag.String("model", "ollama:qwen3:30b-a3b", "provider:model, either ollama: or openai:")
	baseURL := flag.String("base-url", "", "host for ollama:, base URL for openai: (default Ollama's /v1)")
	resume := flag.String("resume", "", "resume this run instead of starting one")
	flag.Parse()
	if err := run(*dbPath, *modelName, *baseURL, *resume); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dbPath, modelName, baseURL, resumeID string) error {
	model, prices, err := buildModel(modelName, baseURL)
	if err != nil {
		return err
	}
	if _, err := providers.PriceFor(prices, model.Name()); err != nil {
		return err
	}
	store, err := agentrt.OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	c := &counter{}
	driver, err := agentrt.NewDriver(agentrt.Config{
		Store:    store,
		Agent:    &modelAgent{},
		Policy:   approveTheFirstChange(),
		Tools:    tools(c),
		Observer: trace.Writer(os.Stderr),
		Model:    &agentrt.ModelConfig{Model: model, Prices: prices},
	})
	if err != nil {
		return err
	}

	fmt.Printf("database: %s\nmodel:    %s\n\n", dbPath, model.Name())
	ctx := context.Background()
	var r agentrt.Run
	if resumeID != "" {
		r, err = driver.Resume(ctx, resumeID)
	} else {
		limits := agentrt.DefaultLimits()
		limits.MaxSteps, limits.MaxModelCalls, limits.MaxOutputTokensPerCall = 12, 12, maxOutputTokens
		r, err = driver.Start(ctx, goal, limits)
	}
	if err != nil {
		return err
	}
	// The run returned by a pause predates the last step's accounting, so
	// the totals are read back.
	if fresh, ferr := store.GetRun(ctx, r.ID); ferr == nil {
		r = fresh
	}
	report(r, dbPath, store)
	return nil
}

// report prints the run's outcome and, when it is waiting, the exact
// commands that grant the approval and continue the run.
func report(r agentrt.Run, dbPath string, store *agentrt.Store) {
	fmt.Printf("\nrun %s: %s", r.ID, r.Status)
	if r.Reason != "" {
		fmt.Printf(" (%s)", r.Reason)
	}
	fmt.Printf(" steps=%d calls=%d tokens=%d/%d result=%s\n",
		r.StepCount, r.ModelCalls, r.Usage.InputTokens, r.Usage.OutputTokens, r.Result)
	if r.Status != agentrt.StatusWaitingForApproval {
		return
	}
	approvals, err := store.ListApprovals(context.Background(), r.ID)
	if err != nil {
		return
	}
	for _, a := range approvals {
		if a.Status == agentrt.ApprovalPending {
			fmt.Printf("\nwaiting for approval %s: %s\n  %s\n", a.ID, a.Kind, a.Presentation)
		}
	}
	fmt.Printf("\ngrant it, then continue the run:\n  go run ./cmd/agentrt -db %s approve %s\n  go run ./examples/live -db %s -resume %s\n", dbPath, r.ID, dbPath, r.ID)
}

// buildModel returns the adapter named by provider:model, with the price
// table that records it as free, because both providers here are local.
func buildModel(name, baseURL string) (agentrt.Model, agentrt.PriceTable, error) {
	if id, ok := strings.CutPrefix(name, "ollama:"); ok {
		// Thinking is on: this model narrates its reasoning either way,
		// and with thinking on the server routes the narration to its own
		// channel instead of leaving a tool call written out as prose,
		// which the runtime would record as a reply that called nothing.
		m, err := ollama.New(ollama.Config{Model: id, Host: baseURL, Think: true})
		return m, ollama.Free(m), err
	}
	if id, ok := strings.CutPrefix(name, "openai:"); ok {
		if baseURL == "" {
			baseURL = "http://" + ollama.DefaultHost + "/v1"
		}
		m, err := openai.New(openai.Config{Model: id, BaseURL: baseURL})
		return m, openai.Free(m), err
	}
	return nil, nil, fmt.Errorf("model %q must start with ollama: or openai:", name)
}

// modelAgent is the whole agent: render the recorded steps, ask the model,
// map the reply back to a decision. Nothing domain-specific is left but the
// opening message.
type modelAgent struct{}

// Decide implements agentrt.Agent.
func (a *modelAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	msgs := render.Messages(in, render.Options{Opening: opening})
	resp, err := in.Model.Generate(ctx, agentrt.ModelRequest{System: system, Messages: msgs, Tools: in.Tools, MaxOutputTokens: maxOutputTokens})
	if err != nil {
		return agentrt.Decision{}, err
	}
	return render.Decide(resp), nil
}

// opening is the one place a domain fact belongs.
func opening(in agentrt.StepInput) string {
	return "The counter starts at 0. " + in.Run.Goal
}

// approveTheFirstChange allows reads, denies everything that is not a read
// or a local mutation, and requires an operator for the first change of a
// run. Later changes inherit that grant: the operator authorized changing
// the counter, not one particular change, which is also why a resumed run
// continues instead of pausing again.
func approveTheFirstChange() agentrt.Policy {
	return agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, view agentrt.RunView) (agentrt.PolicyDecision, error) {
		switch req.Spec.SideEffect {
		case agentrt.ReadOnly:
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "reads need no authorization"}, nil
		case agentrt.LocalMutation:
			for _, a := range view.Approvals {
				if a.Status == agentrt.ApprovalApproved {
					return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "an operator authorized changing the counter in approval " + a.ID}, nil
				}
			}
			return agentrt.NeedApproval("counter_change", "the first change to the counter needs an operator",
				map[string]any{"tool": req.Spec.Name, "args": json.RawMessage(req.Args)},
				map[string]any{"question": "May the agent change the counter?", "call": req.Spec.Name, "arguments": json.RawMessage(req.Args)})
		}
		return agentrt.PolicyDecision{Outcome: agentrt.Deny, Reason: "side effect " + string(req.Spec.SideEffect) + " is not allowed in this example"}, nil
	})
}

// counter is the state the tools share. A resumed process starts from zero
// and the recorded steps still add up, because every change is re-derived
// from the model's next call rather than replayed.
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

// tools are the three from examples/scripted plus the terminal tool the
// model finishes on: render.Decide never produces a complete decision, so a
// model-driven run ends on a terminal tool.
func tools(c *counter) []agentrt.Tool {
	return []agentrt.Tool{
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
		tool{spec: agentrt.ToolSpec{Name: "finish", Description: "Report the final counter and end the run.", InputSchema: []byte(`{"type":"object","properties":{"final_counter":{"type":"integer"}},"required":["final_counter"],"additionalProperties":false}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second, Terminal: true},
			fn: func(call agentrt.ToolCall) (agentrt.ToolResult, error) {
				c.mu.Lock()
				defer c.mu.Unlock()
				return agentrt.ToolResult{Content: mustJSON(map[string]int{"final_counter": c.n}), Summary: fmt.Sprintf("finished at %d", c.n)}, nil
			}},
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
