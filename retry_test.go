package agentrt

import (
	"context"
	"errors"
	"testing"
	"time"
)

// flakyModel fails with each of errs in turn, then answers.
type flakyModel struct {
	errs  []error
	calls int
}

func (m *flakyModel) Name() string { return "flaky" }
func (m *flakyModel) Generate(context.Context, ModelRequest) (ModelResponse, error) {
	m.calls++
	if m.calls <= len(m.errs) {
		return ModelResponse{}, m.errs[m.calls-1]
	}
	return ModelResponse{Text: "ok"}, nil
}

type generateOnce struct{}

func (generateOnce) Decide(ctx context.Context, in StepInput) (Decision, error) {
	if _, err := in.Model.Generate(ctx, ModelRequest{MaxOutputTokens: 10}); err != nil {
		return Decision{}, err
	}
	return Decision{Kind: DecideComplete}, nil
}

// retryRun runs one model call under limits with a driver clock that
// moves a second per reading, and records each wait instead of sleeping.
func retryRun(t *testing.T, ctx context.Context, m *flakyModel, cfg ModelConfig, l Limits) (Run, []time.Duration) {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clock := time.Unix(1700000000, 0)
	cfg.Model = m
	d, err := NewDriver(Config{Store: store, Agent: generateOnce{}, Policy: DefaultPolicy(), Model: &cfg,
		Now: func() time.Time { clock = clock.Add(time.Second); return clock }})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	d.sleep = func(_ context.Context, w time.Duration) error { waits = append(waits, w); return nil }
	run, err := d.Start(ctx, "g", l)
	if err != nil {
		t.Fatal(err)
	}
	return run, waits
}

func transient(after time.Duration) error {
	return TransientError{Err: errors.New("429"), RetryAfter: after}
}

// A retry waits at least as long as the provider asked, and at least the
// backoff.
func TestModel_RetryAfterIsHonoured(t *testing.T) {
	m := &flakyModel{errs: []error{transient(7 * time.Second), transient(time.Millisecond), transient(0)}}
	run, waits := retryRun(t, context.Background(), m, ModelConfig{MaxRetries: 3, Backoff: time.Second}, leaseLimits())
	if run.Status != StatusCompleted || m.calls != 4 {
		t.Fatalf("run %s %s after %d calls", run.Status, run.ReasonDetail, m.calls)
	}
	if want := []time.Duration{7 * time.Second, 2 * time.Second, 4 * time.Second}; len(waits) != 3 || waits[0] != want[0] || waits[1] != want[1] || waits[2] != want[2] {
		t.Fatalf("waits %v, want %v", waits, want)
	}
}

// A wait that cannot be honoured is not slept: a request beyond
// MaxRetryAfter, one past the context's deadline, and one that would reach
// the elapsed or active time limit end the run as model_unavailable after
// the one attempt.
func TestModel_RetryWaitThatCannotFitIsUnavailable(t *testing.T) {
	deadline, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	elapsed, active := leaseLimits(), leaseLimits()
	elapsed.MaxElapsedTime, active.MaxActiveTime = 40*time.Second, 40*time.Second
	cases := map[string]struct {
		ctx    context.Context
		after  time.Duration
		cfg    ModelConfig
		limits Limits
	}{
		"beyond the default maximum":    {context.Background(), 2 * time.Minute, ModelConfig{}, leaseLimits()},
		"beyond the configured maximum": {context.Background(), 20 * time.Second, ModelConfig{MaxRetryAfter: 10 * time.Second}, leaseLimits()},
		"past the context's deadline":   {deadline, 2 * time.Hour, ModelConfig{MaxRetryAfter: 3 * time.Hour}, leaseLimits()},
		"reaching the elapsed limit":    {context.Background(), 45 * time.Second, ModelConfig{}, elapsed},
		"reaching the active limit":     {context.Background(), 45 * time.Second, ModelConfig{}, active},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &flakyModel{errs: []error{transient(tc.after)}}
			tc.cfg.MaxRetries = 2
			run, waits := retryRun(t, tc.ctx, m, tc.cfg, tc.limits)
			if run.Status != StatusFailed || run.Reason != ReasonModelUnavailable || m.calls != 1 || len(waits) != 0 {
				t.Fatalf("run %s/%s %q, %d calls, waits %v", run.Status, run.Reason, run.ReasonDetail, m.calls, waits)
			}
		})
	}
	// Within every bound the same request is honoured.
	m := &flakyModel{errs: []error{transient(30 * time.Second)}}
	run, waits := retryRun(t, deadline, m, ModelConfig{MaxRetries: 2}, elapsed)
	if run.Status != StatusCompleted || len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("run %s %q, waits %v", run.Status, run.ReasonDetail, waits)
	}
}
