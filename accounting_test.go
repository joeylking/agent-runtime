package agentrt_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/replay"
)

// A reply the provider served and billed but that could not be used is
// charged at the usage the provider reported, recorded as an error, and
// not retried.
func TestModel_ServedErrorIsChargedAndNotRetried(t *testing.T) {
	h := newHarness(t, ":memory:")
	served := agentrt.ServedError{Usage: agentrt.Usage{InputTokens: 1000, OutputTokens: 200}, Err: errors.New("tool call arguments are not JSON")}
	m := &replay.Scripted{ModelName: "paid", Errors: []error{served, served, served}}
	prices := agentrt.PriceTable{"paid": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}
	run, _ := modelDriver(t, h, m, prices, 2).Start(context.Background(), "g", modelLimits())
	requireStatus(t, run, agentrt.StatusFailed, agentrt.ReasonModelUnavailable)
	calls, _ := h.store.ListModelCalls(context.Background(), run.ID)
	if len(calls) != 1 || len(m.Requests) != 1 {
		t.Fatalf("calls = %d requests = %d, want one: a served reply is not retried", len(calls), len(m.Requests))
	}
	want := agentrt.Micros(1000*1 + 200*5)
	if c := calls[0]; c.Status != agentrt.CallError || c.Usage != served.Usage || c.Cost != want {
		t.Fatalf("call = %+v", c)
	}
	if run.ModelCalls != 1 || run.Usage != served.Usage || run.EstimatedCost != want {
		t.Fatalf("run totals = %d calls %+v %d micros", run.ModelCalls, run.Usage, run.EstimatedCost)
	}
}

// concurrentAgent asks the model twice at once within one step.
type concurrentAgent struct {
	errs []error
}

func (a *concurrentAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	var wg sync.WaitGroup
	a.errs = make([]error, 2)
	for i := range a.errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, a.errs[i] = in.Model.Generate(ctx, agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}}}, MaxOutputTokens: 10})
		}(i)
	}
	wg.Wait()
	return agentrt.Decision{Kind: agentrt.DecideComplete}, nil
}

// heldModel answers only after its first call has waited for a second
// one to have been refused or dispatched.
type heldModel struct {
	mu    sync.Mutex
	calls int
}

func (m *heldModel) Name() string { return "held" }
func (m *heldModel) Generate(context.Context, agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	return agentrt.ModelResponse{Text: "ok", StopReason: "end_turn", Usage: agentrt.Usage{InputTokens: 10, OutputTokens: 10}}, nil
}

// Two concurrent calls in one step cannot both pass the call limit
// before either is recorded: the check and the reservation are one step.
func TestModel_ConcurrentCallsReserveAgainstTheLimits(t *testing.T) {
	h := newHarness(t, ":memory:")
	m := &heldModel{}
	agent := &concurrentAgent{}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: agent, Policy: agentrt.DefaultPolicy(), Model: &agentrt.ModelConfig{Model: m}})
	if err != nil {
		t.Fatal(err)
	}
	l := limits(5, 3)
	l.MaxModelCalls = 1
	run, err := d.Start(context.Background(), "g", l)
	if err != nil {
		t.Fatal(err)
	}
	var lim agentrt.ErrLimit
	refused := 0
	for _, err := range agent.errs {
		if errors.As(err, &lim) && lim.Reason == agentrt.ReasonLimitModelCalls {
			refused++
		}
	}
	if m.calls != 1 || refused != 1 || run.ModelCalls != 1 {
		t.Fatalf("dispatched %d, refused %d, recorded %d; want 1, 1, 1 (errs %v)", m.calls, refused, run.ModelCalls, agent.errs)
	}
}

// uncappedAgent asks without an output cap.
type uncappedAgent struct{}

func (uncappedAgent) Decide(ctx context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	if _, err := in.Model.Generate(ctx, agentrt.ModelRequest{Messages: []agentrt.Message{{Role: "user", Content: []agentrt.ContentBlock{{Type: "text", Text: "go"}}}}}); err != nil {
		return agentrt.Decision{}, err
	}
	return agentrt.Decision{Kind: agentrt.DecideComplete}, nil
}

// Under a token or cost limit a request with no output cap cannot be
// projected, so it is refused before dispatch rather than counted as
// producing no output.
func TestModel_UncappedRequestRefusedUnderAProjectedLimit(t *testing.T) {
	prices := agentrt.PriceTable{"paid": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}
	cases := map[string]struct {
		limits func(*agentrt.Limits)
		reason agentrt.TerminalReason
	}{
		"tokens":        {func(l *agentrt.Limits) { l.MaxTotalTokens = 1_000_000 }, agentrt.ReasonLimitTokens},
		"cost":          {func(l *agentrt.Limits) { l.MaxEstimatedCost = 1_000_000 }, agentrt.ReasonLimitCost},
		"capped by run": {func(l *agentrt.Limits) { l.MaxTotalTokens, l.MaxOutputTokensPerCall = 1_000_000, 100 }, agentrt.ReasonGoalCompleted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ":memory:")
			m := &replay.Scripted{ModelName: "paid", Responses: []agentrt.ModelResponse{done}}
			d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: uncappedAgent{}, Policy: agentrt.DefaultPolicy(), Model: &agentrt.ModelConfig{Model: m, Prices: prices}})
			if err != nil {
				t.Fatal(err)
			}
			l := limits(5, 3)
			tc.limits(&l)
			run, _ := d.Start(context.Background(), "g", l)
			if run.Reason != tc.reason {
				t.Fatalf("reason = %s (%s), want %s", run.Reason, run.ReasonDetail, tc.reason)
			}
			if tc.reason != agentrt.ReasonGoalCompleted && len(m.Requests) != 0 {
				t.Fatal("an uncapped request was dispatched")
			}
		})
	}
}

// cancellingModel cancels its caller while the provider is serving the
// request, then answers.
type cancellingModel struct{ cancel context.CancelFunc }

func (m cancellingModel) Name() string { return "paid" }
func (m cancellingModel) Generate(context.Context, agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	m.cancel()
	return agentrt.ModelResponse{Text: "late", StopReason: "end_turn", Usage: agentrt.Usage{InputTokens: 300, OutputTokens: 40}}, nil
}

// A dispatched request is recorded and charged even when the caller's
// context is cancelled meanwhile; the cancellation is then returned.
func TestModel_CancelledContextStillRecordsTheCall(t *testing.T) {
	h := newHarness(t, ":memory:")
	ctx, cancel := context.WithCancel(context.Background())
	prices := agentrt.PriceTable{"paid": {InputPerMTok: 1_000_000, OutputPerMTok: 5_000_000}}
	d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: modelAgent{}, Policy: agentrt.DefaultPolicy(), Model: &agentrt.ModelConfig{Model: cancellingModel{cancel}, Prices: prices}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Start(ctx, "g", modelLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	runs, _ := h.store.ListRuns(context.Background())
	calls, _ := h.store.ListModelCalls(context.Background(), runs[0].ID)
	if len(calls) != 1 || calls[0].Status != agentrt.CallOK || calls[0].Cost != 300+40*5 {
		t.Fatalf("calls = %+v", calls)
	}
	if runs[0].ModelCalls != 1 || runs[0].EstimatedCost != 300+40*5 {
		t.Fatalf("run totals = %d calls, %d micros", runs[0].ModelCalls, runs[0].EstimatedCost)
	}
}
