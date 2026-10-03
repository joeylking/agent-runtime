package otel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"
	"github.com/joeylking/agent-runtime/scripted"
)

// The fixtures are real runs through real drivers in one file-backed
// database: a completed run with an invalid decision and a policy denial,
// a run paused on an approval and resumed, a run interrupted by a process
// that died mid-step and resumed through an interrupted_side_effect
// approval, a run cancelled under a running tool whose outcome arrived
// late, a model-backed run with a retried attempt, and runs ended by the
// step limit, loop detection, and a fail decision. Between them they emit
// every event type the runtime has.
type fixtures struct {
	dir   string
	path  string
	store *agentrt.Store
	// run ids by fixture name
	completed, paused, interrupted, cancelled, model, limit, loop, failed string
}

const longGoal = 3000

var (
	fixOnce sync.Once
	fix     *fixtures
	fixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix != nil {
		fix.store.Close()
		os.RemoveAll(fix.dir)
	}
	os.Exit(code)
}

func loadFixtures(t *testing.T) *fixtures {
	t.Helper()
	fixOnce.Do(func() { fix, fixErr = buildFixtures() })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return fix
}

type tool struct {
	spec agentrt.ToolSpec
	fn   func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error)
}

func (t tool) Spec() agentrt.ToolSpec { return t.spec }
func (t tool) Call(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return t.fn(ctx, c)
}

const (
	anySchema    = `{"type":"object"}`
	numberSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`
)

func ok(name string, se agentrt.SideEffect, schema string) tool {
	return tool{spec: agentrt.ToolSpec{Name: name, Description: name, InputSchema: []byte(schema), SideEffect: se, Timeout: 5 * time.Second},
		fn: func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
			return agentrt.ToolResult{Content: []byte(`{"ok":true}`), Summary: name + " done"}, nil
		}}
}

// tools is every fixture's tool set; release unblocks "block".
func tools(release <-chan struct{}) []agentrt.Tool {
	block := tool{spec: agentrt.ToolSpec{Name: "block", Description: "block", InputSchema: []byte(anySchema), SideEffect: agentrt.LocalMutation, Timeout: 30 * time.Second},
		fn: func(ctx context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
			select {
			case <-release:
				return agentrt.ToolResult{Content: []byte(`{"late":true}`), Summary: "finished after cancel"}, nil
			case <-ctx.Done():
				return agentrt.ToolResult{}, ctx.Err()
			}
		}}
	return []agentrt.Tool{ok("read", agentrt.ReadOnly, numberSchema), ok("write", agentrt.LocalMutation, numberSchema), ok("publish", agentrt.RemoteMutation, anySchema), ok("wipe", agentrt.Destructive, anySchema), block}
}

func driver(store *agentrt.Store, agent agentrt.Agent, ts []agentrt.Tool, cfg func(*agentrt.Config)) (*agentrt.Driver, error) {
	c := agentrt.Config{Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: ts, LeaseOwner: "fixtures"}
	if cfg != nil {
		cfg(&c)
	}
	return agentrt.NewDriver(c)
}

func limits(steps int) agentrt.Limits {
	l := agentrt.DefaultLimits()
	l.MaxSteps = steps
	return l
}

// modelAgent asks the model once per step and acts on its first tool use,
// completing on a text-only reply.
type modelAgent struct{}

func (modelAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	resp, err := in.Model.Generate(ctx, agentrt.ModelRequest{System: "s", Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: in.Run.Goal}}}}, Tools: in.Tools, MaxOutputTokens: 100})
	if err != nil {
		return agentrt.Decision{}, err
	}
	if len(resp.ToolUses) == 0 {
		return agentrt.Decision{Kind: agentrt.DecideComplete, Result: []byte(`{"text":"` + resp.Text + `"}`)}, nil
	}
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: resp.ToolUses[0].Name, Args: resp.ToolUses[0].Args, Reason: "the model asked"}, nil
}

func buildFixtures() (*fixtures, error) {
	dir, err := os.MkdirTemp("", "agentrt-otel-")
	if err != nil {
		return nil, err
	}
	f := &fixtures{dir: dir, path: filepath.Join(dir, "runs.db")}
	f.store, err = agentrt.OpenStore(f.path)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	ts := tools(nil)
	start := func(name string, d *agentrt.Driver, goal string, l agentrt.Limits, want agentrt.RunStatus) (string, error) {
		run, err := d.Start(ctx, goal, l)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if run.Status != want {
			return "", fmt.Errorf("%s: run %s %s (%s)", name, run.Status, run.Reason, run.ReasonDetail)
		}
		return run.ID, nil
	}

	// Completed, with an invalid decision and a policy denial on the way.
	d, err := driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, strings.Repeat("because ", 400)),
		scripted.ToolCall("write", `{"n":"x"}`, "malformed"),
		scripted.ToolCall("wipe", `{}`, "denied"),
		scripted.ToolCall("read", `{"n":2}`, "again"),
		scripted.Complete(`{"done":true}`),
	}}, ts, nil)
	if err != nil {
		return nil, err
	}
	if f.completed, err = start("completed", d, strings.Repeat("g", longGoal), limits(10), agentrt.StatusCompleted); err != nil {
		return nil, err
	}

	// Paused on publish, approved, resumed.
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look"),
		scripted.ToolCall("publish", `{}`, "ship it"),
		scripted.Complete(`{"shipped":true}`),
	}}, ts, nil)
	if err != nil {
		return nil, err
	}
	if f.paused, err = start("paused", d, "publish with approval", limits(10), agentrt.StatusWaitingForApproval); err != nil {
		return nil, err
	}
	pending, err := f.store.PendingApprovalIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := agentrt.Approve(ctx, f.store, nil, f.paused, pending[f.paused][0], "operator", "looks right"); err != nil {
		return nil, fmt.Errorf("paused: approve: %w", err)
	}
	if run, err := d.Resume(ctx, f.paused); err != nil || run.Status != agentrt.StatusCompleted {
		return nil, fmt.Errorf("paused: resume: %v %s", err, run.Status)
	}

	// Interrupted: a child process dies while write runs, its lease
	// expires, Resume takes the run over and pauses on the unknown side
	// effect, an operator approves, and the tool runs again.
	f.interrupted = "interrupted-run"
	child := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
	child.Env = append(os.Environ(), "AGENTRT_OTEL_CRASH_DB="+f.path, "AGENTRT_OTEL_CRASH_RUN="+f.interrupted)
	if out, err := child.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("interrupted: child: %v\n%s", err, out)
	}
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("write", `{"n":1}`, "do it"),
		scripted.Complete(`{"written":true}`),
	}}, ts, func(c *agentrt.Config) { c.LeaseOwner = "resumer" })
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		run, err := d.Resume(ctx, f.interrupted)
		if err == nil {
			if run.Status != agentrt.StatusWaitingForApproval {
				return nil, fmt.Errorf("interrupted: resume: run %s %s (%s)", run.Status, run.Reason, run.ReasonDetail)
			}
			break
		}
		if !errors.Is(err, agentrt.ErrRunState) || time.Now().After(deadline) {
			return nil, fmt.Errorf("interrupted: resume: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	pending, err = f.store.PendingApprovalIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := agentrt.Approve(ctx, f.store, nil, f.interrupted, pending[f.interrupted][0], "operator", "run it again"); err != nil {
		return nil, fmt.Errorf("interrupted: approve: %w", err)
	}
	if run, err := d.Resume(ctx, f.interrupted); err != nil || run.Status != agentrt.StatusCompleted {
		return nil, fmt.Errorf("interrupted: second resume: %v %s %s", err, run.Status, run.ReasonDetail)
	}

	// Cancelled while block runs; the tool's outcome arrives late.
	release := make(chan struct{})
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("block", `{}`, "wait"),
		scripted.Complete(`{}`),
	}}, tools(release), nil)
	if err != nil {
		return nil, err
	}
	f.cancelled = "cancelled-run"
	done := make(chan error, 1)
	go func() {
		_, err := d.StartWithID(ctx, f.cancelled, "cancel me", limits(10))
		done <- err
	}()
	deadline = time.Now().Add(20 * time.Second)
	for {
		events, _ := f.store.ListEvents(ctx, f.cancelled)
		if n := len(events); n > 0 && events[n-1].Type == agentrt.EventStepToolStarted {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("cancelled: block never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := agentrt.Cancel(ctx, f.store, nil, f.cancelled, "operator", "changed my mind"); err != nil {
		return nil, fmt.Errorf("cancelled: %w", err)
	}
	close(release)
	// The loop finds the run cancelled at its next write and stops; the
	// tool's late outcome is appended first.
	if err := <-done; err != nil && !errors.Is(err, agentrt.ErrRunState) {
		return nil, fmt.Errorf("cancelled: Start returned %v", err)
	}
	if run, err := f.store.GetRun(ctx, f.cancelled); err != nil || run.Status != agentrt.StatusCancelled {
		return nil, fmt.Errorf("cancelled: run %s %v", run.Status, err)
	}

	// Model-backed: the first attempt fails transiently and is retried.
	m := &replay.Scripted{ModelName: "scripted-model",
		Errors:    []error{agentrt.TransientError{Err: errors.New("429 slow down")}},
		Responses: []agentrt.ModelResponse{{}, {ToolUses: []agentrt.ToolUse{{ID: "t1", Name: "read", Args: []byte(`{"n":7}`)}}, StopReason: "tool_use", Usage: agentrt.Usage{InputTokens: 100, OutputTokens: 20}}, {Text: "done", StopReason: "end_turn", Usage: agentrt.Usage{InputTokens: 120, OutputTokens: 5, CachedInputTokens: 50}}}}
	d, err = driver(f.store, modelAgent{}, ts, func(c *agentrt.Config) {
		c.Model = &agentrt.ModelConfig{Model: m, Prices: agentrt.PriceTable{"scripted-model": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000, CachedInputPerMTok: 100_000}}, MaxRetries: 1, Backoff: time.Millisecond, CallTimeout: time.Second}
	})
	if err != nil {
		return nil, err
	}
	l := limits(10)
	l.MaxModelCalls = 10
	if f.model, err = start("model", d, "read seven", l, agentrt.StatusCompleted); err != nil {
		return nil, err
	}

	// Step limit.
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":2}`, "")}}, ts, nil)
	if err != nil {
		return nil, err
	}
	if f.limit, err = start("limit", d, "too many steps", limits(1), agentrt.StatusFailed); err != nil {
		return nil, err
	}

	// Loop detection.
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, "")}}, ts, nil)
	if err != nil {
		return nil, err
	}
	l = limits(10)
	l.LoopThreshold = 2
	if f.loop, err = start("loop", d, "loop", l, agentrt.StatusFailed); err != nil {
		return nil, err
	}

	// A fail decision.
	d, err = driver(f.store, &scripted.Agent{Decisions: []agentrt.Decision{scripted.Fail(strings.Repeat("no ", 1000))}}, ts, nil)
	if err != nil {
		return nil, err
	}
	if f.failed, err = start("failed", d, "give up", limits(10), agentrt.StatusFailed); err != nil {
		return nil, err
	}
	return f, nil
}

// TestCrashHelper is the process that dies mid-step for the interrupted
// fixture. It runs only when buildFixtures starts it.
func TestCrashHelper(t *testing.T) {
	path := os.Getenv("AGENTRT_OTEL_CRASH_DB")
	if path == "" {
		t.Skip("not the crash helper")
	}
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	crash := tool{spec: agentrt.ToolSpec{Name: "write", Description: "write", InputSchema: []byte(numberSchema), SideEffect: agentrt.LocalMutation, Timeout: 5 * time.Second},
		fn: func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
			os.Exit(0) // the process dies with the lease held and the step executing
			return agentrt.ToolResult{}, nil
		}}
	d, err := driver(store, &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("write", `{"n":1}`, "do it"), scripted.Complete(`{}`)}}, []agentrt.Tool{crash}, func(c *agentrt.Config) {
		c.LeaseTTL = time.Second
		c.LeaseOwner = "crasher"
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.StartWithID(context.Background(), os.Getenv("AGENTRT_OTEL_CRASH_RUN"), "survive a crash", limits(10)); err != nil {
		t.Fatal(err)
	}
	t.Fatal("the crash tool returned")
}

// eventTypes is every event type the runtime writes.
var eventTypes = []string{
	agentrt.EventRunCreated, agentrt.EventRunStarted, agentrt.EventRunFinished, agentrt.EventStepStarted, agentrt.EventStepDecided,
	agentrt.EventStepPolicy, agentrt.EventStepToolStarted, agentrt.EventStepToolFinished, agentrt.EventStepFailed, agentrt.EventApprovalRequested,
	agentrt.EventLimitExceeded, agentrt.EventLoopDetected, agentrt.EventApprovalDecided, agentrt.EventRunResumed, agentrt.EventStepInterrupted,
	agentrt.EventLeaseTakenOver, agentrt.EventModelDispatched, agentrt.EventModelCompleted, agentrt.EventModelFailed,
}

func field(t *testing.T, e agentrt.Event, name string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return string(m[name])
}
