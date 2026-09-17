package agentrt_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"
)

// modelAgent asks the model once per step and turns the first tool use into
// a decision, completing when the model returns text only.
type modelAgent struct{}

func (modelAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	resp, err := in.Model.Generate(ctx, agentrt.ModelRequest{System: "s", Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: in.Run.Goal}}}}, Tools: in.Tools, MaxOutputTokens: 100})
	if err != nil {
		return agentrt.Decision{}, err
	}
	if len(resp.ToolUses) == 0 {
		return agentrt.Decision{Kind: agentrt.DecideComplete, Result: []byte(`{}`)}, nil
	}
	return agentrt.Decision{Kind: agentrt.DecideToolCall, Tool: resp.ToolUses[0].Name, Args: resp.ToolUses[0].Args}, nil
}

func toolUse(name, args string) agentrt.ModelResponse {
	return agentrt.ModelResponse{ToolUses: []agentrt.ToolUse{{ID: "t1", Name: name, Args: []byte(args)}}, StopReason: "tool_use", Usage: agentrt.Usage{InputTokens: 100, OutputTokens: 20}}
}

var done = agentrt.ModelResponse{Text: "done", StopReason: "end_turn", Usage: agentrt.Usage{InputTokens: 120, OutputTokens: 5}}

func modelDriver(t *testing.T, h *harness, m agentrt.Model, prices agentrt.PriceTable, retries int) *agentrt.Driver {
	t.Helper()
	d, err := agentrt.NewDriver(agentrt.Config{
		Store: h.store, Agent: modelAgent{}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{echoTool("read", agentrt.ReadOnly)},
		Model:    &agentrt.ModelConfig{Model: m, Prices: prices, MaxRetries: retries, Backoff: time.Millisecond, CallTimeout: 200 * time.Millisecond},
		Observer: func(e agentrt.Event) { h.events = append(h.events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func modelLimits() agentrt.Limits {
	l := limits(10, 3)
	l.MaxModelCalls = 10
	return l
}

func TestModel_UsageAndCostRecorded(t *testing.T) {
	h := newHarness(t, ":memory:")
	m := &replay.Scripted{ModelName: "test-model", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	prices := agentrt.PriceTable{"test-model": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}} // $1 and $5 per million
	run, err := modelDriver(t, h, m, prices, 0).Start(context.Background(), "g", modelLimits())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if run.ModelCalls != 2 || run.Usage.InputTokens != 220 || run.Usage.OutputTokens != 25 {
		t.Fatalf("totals = calls %d usage %+v", run.ModelCalls, run.Usage)
	}
	// 220 input at $1/M = 220 micros; 25 output at $5/M = 125 micros.
	if run.EstimatedCost != 345 {
		t.Fatalf("cost = %d micros", run.EstimatedCost)
	}
	calls, _ := h.store.ListModelCalls(context.Background(), run.ID)
	if len(calls) != 2 || calls[0].Status != agentrt.CallOK || calls[0].StepID == "" || calls[0].Attempt != 1 || calls[0].Cost != 200 {
		t.Fatalf("calls = %+v", calls)
	}
	if m.Requests[0].MaxOutputTokens != 100 || len(m.Requests[0].Tools) != 1 {
		t.Fatalf("request = %+v", m.Requests[0])
	}
	types := eventTypes(h.events)
	seen := map[string]int{}
	for _, tp := range types {
		seen[tp]++
	}
	if seen[agentrt.EventModelDispatched] != 2 || seen[agentrt.EventModelCompleted] != 2 {
		t.Fatalf("model events = %v", seen)
	}
}

func TestModel_CallLimitStopsBeforeDispatch(t *testing.T) {
	h := newHarness(t, ":memory:")
	m := &replay.Scripted{Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), toolUse("read", `{"n":2}`), toolUse("read", `{"n":3}`), done}}
	l := modelLimits()
	l.MaxModelCalls = 2
	run, err := modelDriver(t, h, m, nil, 0).Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitModelCalls)
	if len(m.Requests) != 2 || run.ModelCalls != 2 {
		t.Fatalf("requests = %d calls = %d", len(m.Requests), run.ModelCalls)
	}
}

func TestModel_TokenAndCostLimitsProjected(t *testing.T) {
	h := newHarness(t, ":memory:")
	m := &replay.Scripted{Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	l := modelLimits()
	l.MaxTotalTokens = 150 // the first call alone projects more than this
	run, _ := modelDriver(t, h, m, nil, 0).Start(context.Background(), "g", l)
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitTokens)
	if len(m.Requests) != 0 {
		t.Fatal("a call was dispatched despite the token projection")
	}

	// Cost: learn the projection for this agent's request, then set a cap
	// that admits the first call and refuses the second.
	h2 := newHarness(t, ":memory:")
	m2 := &replay.Scripted{ModelName: "paid", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	prices := agentrt.PriceTable{"paid": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}
	l = modelLimits()
	l.MaxEstimatedCost = 1_000_000
	run, _ = modelDriver(t, h2, m2, prices, 0).Start(context.Background(), "g", l)
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	projected := agentrt.Micros(agentrt.EstimateInputTokens(m2.Requests[0])*1 + 100*5) // micros at these prices
	firstCost := agentrt.Micros(100*1 + 20*5)
	h3 := newHarness(t, ":memory:")
	m3 := &replay.Scripted{ModelName: "paid", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	l.MaxEstimatedCost = firstCost + projected - 1
	run, _ = modelDriver(t, h3, m3, prices, 0).Start(context.Background(), "g", l)
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonLimitCost)
	if len(m3.Requests) != 1 || run.EstimatedCost != firstCost {
		t.Fatalf("requests = %d cost = %d", len(m3.Requests), run.EstimatedCost)
	}
	// Local models have no price and no cost limit exposure.
	h4 := newHarness(t, ":memory:")
	m4 := &replay.Scripted{ModelName: "local", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	l.MaxTotalTokens = 0
	run, _ = modelDriver(t, h4, m4, nil, 0).Start(context.Background(), "g", l)
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	if run.EstimatedCost != 0 {
		t.Fatalf("local cost = %d", run.EstimatedCost)
	}
}

func TestModel_TransientRetryThenSuccess(t *testing.T) {
	h := newHarness(t, ":memory:")
	m := &replay.Scripted{Responses: []agentrt.ModelResponse{{}, toolUse("read", `{"n":1}`), done}, Errors: []error{agentrt.TransientError{Err: errors.New("429")}}}
	run, err := modelDriver(t, h, m, nil, 2).Start(context.Background(), "g", modelLimits())
	if err != nil {
		t.Fatal(err)
	}
	requireStatus(t, run, agentrt.StatusCompleted, agentrt.ReasonGoalCompleted)
	calls, _ := h.store.ListModelCalls(context.Background(), run.ID)
	if len(calls) != 3 || calls[0].Status != agentrt.CallError || calls[0].Attempt != 1 || calls[1].Status != agentrt.CallOK || calls[1].Attempt != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if run.ModelCalls != 3 {
		t.Fatalf("retries must count as calls: %d", run.ModelCalls)
	}
}

func TestModel_ExhaustedRetriesIsUnavailable(t *testing.T) {
	h := newHarness(t, ":memory:")
	tr := agentrt.TransientError{Err: errors.New("503")}
	m := &replay.Scripted{Errors: []error{tr, tr, tr}}
	run, _ := modelDriver(t, h, m, nil, 2).Start(context.Background(), "g", modelLimits())
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonModelUnavailable)
	if run.ModelCalls != 3 {
		t.Fatalf("calls = %d", run.ModelCalls)
	}
	// A non-transient error is not retried.
	h2 := newHarness(t, ":memory:")
	m2 := &replay.Scripted{Errors: []error{errors.New("400 bad request")}}
	run, _ = modelDriver(t, h2, m2, nil, 2).Start(context.Background(), "g", modelLimits())
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonModelUnavailable)
	if run.ModelCalls != 1 {
		t.Fatalf("non-transient error retried: %d calls", run.ModelCalls)
	}
}

// slowModel never answers within the call timeout.
type slowModel struct{}

func (slowModel) Name() string { return "slow" }
func (slowModel) Generate(ctx context.Context, _ agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	<-ctx.Done()
	return agentrt.ModelResponse{}, ctx.Err()
}

func TestModel_AmbiguousAttemptChargedConservatively(t *testing.T) {
	h := newHarness(t, ":memory:")
	prices := agentrt.PriceTable{"slow": {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}
	run, _ := modelDriver(t, h, slowModel{}, prices, 1).Start(context.Background(), "g", modelLimits())
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonModelUnavailable)
	calls, _ := h.store.ListModelCalls(context.Background(), run.ID)
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	for _, c := range calls {
		if c.Status != agentrt.CallAmbiguous || c.Usage.OutputTokens != 100 || c.Usage.InputTokens == 0 || c.Cost == 0 {
			t.Fatalf("ambiguous call = %+v", c)
		}
	}
	if run.ModelCalls != 2 || run.EstimatedCost == 0 {
		t.Fatalf("run totals = %d calls, %d micros", run.ModelCalls, run.EstimatedCost)
	}
}

func TestReplay_RecordThenReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	inner := &replay.Scripted{ModelName: "m", Responses: []agentrt.ModelResponse{toolUse("read", `{"n":1}`), done}}
	rec := &replay.Recorder{Inner: inner, Dir: dir}
	req := agentrt.ModelRequest{System: "s", Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "hi"}}}}}
	first, err := rec.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	rep := &replay.Replayer{ModelName: "m", Dir: dir}
	again, err := rep.Generate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if again.ToolUses[0].Name != first.ToolUses[0].Name || again.Usage != first.Usage {
		t.Fatalf("replayed %+v != recorded %+v", again, first)
	}
	req.System = "different"
	if _, err := rep.Generate(context.Background(), req); !errors.Is(err, replay.ErrNotRecorded) {
		t.Fatalf("unrecorded request: %v", err)
	}
	other := &replay.Replayer{ModelName: "other-model", Dir: dir}
	req.System = "s"
	if _, err := other.Generate(context.Background(), req); !errors.Is(err, replay.ErrNotRecorded) {
		t.Fatal("recording served for a different model name")
	}
}
