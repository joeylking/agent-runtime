package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// ContentBlock is one part of a message. Types are text, tool_use, and
// tool_result, matching native tool-use APIs closely enough that a provider
// adapter is a mapping, not a redesign.
type ContentBlock struct {
	Type string `json:"type"`
	// Text applies to text blocks.
	Text string `json:"text,omitempty"`
	// ToolUseID, Name, and Input apply to tool_use blocks; ToolUseID also
	// links a tool_result block to the call it answers.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	// Content and IsError apply to tool_result blocks.
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// Message is one turn of the conversation the agent renders for the model.
type Message struct {
	Role    string         `json:"role"` // user or assistant
	Content []ContentBlock `json:"content"`
}

// ModelRequest is one generation request.
type ModelRequest struct {
	System          string     `json:"system,omitempty"`
	Messages        []Message  `json:"messages"`
	Tools           []ToolSpec `json:"tools,omitempty"`
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
}

// ToolUse is a tool call the model asked for.
type ToolUse struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Usage is what the provider reported.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
}

func (u Usage) add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens, CachedInputTokens: u.CachedInputTokens + o.CachedInputTokens}
}

// ModelResponse is one generation result.
type ModelResponse struct {
	Text       string          `json:"text,omitempty"`
	ToolUses   []ToolUse       `json:"tool_uses,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      Usage           `json:"usage"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// Model generates responses. Implementations are provider adapters; the
// runtime wraps every Model in a ModelCaller and agents only see that.
type Model interface {
	// Name identifies the model for accounting and pricing.
	Name() string
	Generate(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// TransientError marks a provider failure worth retrying, such as a rate
// limit or a 5xx response.
type TransientError struct {
	Err error
	// RetryAfter is how long the provider asked the caller to wait before
	// trying again, or zero when it did not say.
	RetryAfter time.Duration
}

func (e TransientError) Error() string { return "transient: " + e.Err.Error() }
func (e TransientError) Unwrap() error { return e.Err }

// ServedError is returned by a Model when the provider served the request,
// and so billed it, but the response could not be used: it would not
// decode, or carried a tool call whose arguments are not JSON. Usage is what
// the provider reported, and the accounting caller charges it; without this
// an unusable reply would cost nothing on the books.
type ServedError struct {
	Usage Usage
	Err   error
}

func (e ServedError) Error() string { return e.Err.Error() }
func (e ServedError) Unwrap() error { return e.Err }

// Micros is money in millionths of a currency unit.
type Micros int64

// Price is a model's price per million tokens.
type Price struct {
	InputPerMTok       Micros `json:"input_per_mtok"`
	OutputPerMTok      Micros `json:"output_per_mtok"`
	CachedInputPerMTok Micros `json:"cached_input_per_mtok"`
}

// PriceTable maps model names to prices. A model absent from the table
// costs nothing, which is the right answer for local models and the wrong
// one for paid ones, so consumers pass a table for every paid model.
type PriceTable map[string]Price

func (p PriceTable) cost(model string, u Usage) Micros {
	pr, ok := p[model]
	if !ok {
		return 0
	}
	return Micros(int64(u.InputTokens-u.CachedInputTokens)*int64(pr.InputPerMTok)/1e6 +
		int64(u.CachedInputTokens)*int64(pr.CachedInputPerMTok)/1e6 +
		int64(u.OutputTokens)*int64(pr.OutputPerMTok)/1e6)
}

// ModelCallStatus is the outcome of one attempt.
type ModelCallStatus string

const (
	CallDispatched ModelCallStatus = "dispatched"
	CallOK         ModelCallStatus = "ok"
	CallError      ModelCallStatus = "error"
	// CallAmbiguous means the request was sent and no answer came back, so
	// the provider may or may not have charged for it. It counts as a call
	// and is charged conservatively.
	CallAmbiguous ModelCallStatus = "ambiguous"
)

// ModelCall is one persisted attempt.
type ModelCall struct {
	ID           string
	RunID        string
	StepID       string
	Attempt      int
	Status       ModelCallStatus
	Model        string
	Usage        Usage
	Cost         Micros
	Latency      time.Duration
	Error        string
	DispatchedAt time.Time
	CompletedAt  time.Time
}

// ErrLimit is returned by a ModelCaller when a run limit would be exceeded.
type ErrLimit struct {
	Reason TerminalReason
	Detail string
}

func (e ErrLimit) Error() string { return string(e.Reason) + ": " + e.Detail }

// ErrModelUnavailable is returned when every attempt failed.
type ErrModelUnavailable struct {
	Attempts int
	Last     error
}

func (e ErrModelUnavailable) Error() string {
	return fmt.Sprintf("model unavailable after %d attempt(s): %v", e.Attempts, e.Last)
}

func (e ErrModelUnavailable) Unwrap() error { return e.Last }

// ModelCaller is the agent's only handle to the model. It enforces limits
// before every call, records every attempt, and applies prices.
type ModelCaller interface {
	Generate(ctx context.Context, req ModelRequest) (ModelResponse, error)
}

// ModelConfig configures the wrapper.
type ModelConfig struct {
	Model       Model
	Prices      PriceTable
	CallTimeout time.Duration // per attempt; default 2 minutes
	MaxRetries  int           // transient retries per Generate; default 2
	Backoff     time.Duration // base backoff; default 1 second
	// MaxRetryAfter is the longest TransientError.RetryAfter honoured;
	// default DefaultMaxRetryAfter. A provider that asks for longer ends
	// the retries as model_unavailable rather than being retried early.
	MaxRetryAfter time.Duration
}

// DefaultMaxRetryAfter is the longest wait a provider may ask for before a
// retry when ModelConfig.MaxRetryAfter is zero.
const DefaultMaxRetryAfter = time.Minute

type caller struct {
	d      *Driver
	cfg    ModelConfig
	runID  string
	stepID string
	// started is when the step began, by the driver's clock, and lease is
	// the loop's hold on the run: no attempt is dispatched once it is lost.
	started time.Time
	lease   *lease

	// mu serialises the limit check and the reservation, so concurrent
	// Generate calls within one step cannot all pass the check before any
	// of them is recorded. The reservation holds each in-flight call's
	// projection until its outcome is added to the run totals.
	mu            sync.Mutex
	reservedCalls int
	reserved      Usage
	reservedCost  Micros
}

// Generate implements ModelCaller.
func (c *caller) Generate(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	run, err := c.d.store.GetRun(ctx, c.runID)
	if err != nil {
		return ModelResponse{}, err
	}
	if run.Limits.MaxOutputTokensPerCall > 0 && (req.MaxOutputTokens <= 0 || req.MaxOutputTokens > run.Limits.MaxOutputTokensPerCall) {
		req.MaxOutputTokens = run.Limits.MaxOutputTokensPerCall
	}
	timeout := c.cfg.CallTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	retries := c.cfg.MaxRetries
	if retries < 0 {
		retries = 0
	}
	backoff := c.cfg.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	var last error
	for attempt := 1; attempt <= retries+1; attempt++ {
		if !c.lease.ok() {
			return ModelResponse{}, c.lease.err()
		}
		// A retry is a new call against the limits.
		est, estCost, err := c.reserve(ctx, req)
		if err != nil {
			return ModelResponse{}, err
		}
		resp, at, done, gerr := c.attempt(ctx, req, attempt, timeout)
		c.release(est, estCost)
		if done {
			return resp, gerr
		}
		last = gerr
		if attempt <= retries {
			wait, werr := c.retryWait(ctx, run, backoff*time.Duration(1<<(attempt-1)), gerr, at)
			if werr != nil {
				return ModelResponse{}, ErrModelUnavailable{Attempts: attempt, Last: werr}
			}
			if err := c.d.sleep(ctx, wait); err != nil {
				return ModelResponse{}, err
			}
		}
	}
	return ModelResponse{}, ErrModelUnavailable{Attempts: retries + 1, Last: last}
}

// retryWait is how long to wait before the next attempt: the backoff, or
// longer when the provider asked for it. The wait must fit: a request for
// more than MaxRetryAfter, or a wait that would pass the context's
// deadline or reach one of the run's time limits, fails instead of
// sleeping. at is the driver's clock reading for the failed attempt's
// completion, so the limits are judged without another reading.
func (c *caller) retryWait(ctx context.Context, run Run, backoff time.Duration, last error, at time.Time) (time.Duration, error) {
	wait := backoff
	var tr TransientError
	if errors.As(last, &tr) && tr.RetryAfter > 0 {
		limit := c.cfg.MaxRetryAfter
		if limit <= 0 {
			limit = DefaultMaxRetryAfter
		}
		if tr.RetryAfter > limit {
			return 0, fmt.Errorf("the provider asked to wait %s before a retry, more than the %s allowed: %w", tr.RetryAfter, limit, last)
		}
		wait = max(wait, tr.RetryAfter)
	}
	if dl, ok := ctx.Deadline(); ok && !c.d.wall().Add(wait).Before(dl) {
		return 0, fmt.Errorf("a retry after %s would pass the context's deadline: %w", wait, last)
	}
	l := run.Limits
	if l.MaxElapsedTime > 0 && at.Add(wait).Sub(run.CreatedAt) >= l.MaxElapsedTime {
		return 0, fmt.Errorf("a retry after %s would reach the elapsed time limit %s: %w", wait, l.MaxElapsedTime, last)
	}
	if l.MaxActiveTime > 0 && run.ActiveTime+at.Add(wait).Sub(c.started) >= l.MaxActiveTime {
		return 0, fmt.Errorf("a retry after %s would reach the active time limit %s: %w", wait, l.MaxActiveTime, last)
	}
	return wait, nil
}

// attempt dispatches one request and records its outcome. done reports
// that Generate returns the response and error as they are; otherwise err is a
// failure worth retrying, and at is the clock reading for its completion.
// Once the request is dispatched its outcome is recorded even if ctx is
// cancelled, because the provider may have served and billed it.
func (c *caller) attempt(ctx context.Context, req ModelRequest, attempt int, timeout time.Duration) (_ ModelResponse, at time.Time, done bool, _ error) {
	call := ModelCall{ID: c.d.newID(), RunID: c.runID, StepID: c.stepID, Attempt: attempt, Status: CallDispatched, Model: c.cfg.Model.Name(), DispatchedAt: c.d.now()}
	if err := c.d.store.tx(ctx, c.d.observer, func(t *txn) error {
		if err := t.insertModelCall(ctx, call); err != nil {
			return err
		}
		return t.appendEvent(ctx, Event{RunID: c.runID, StepID: c.stepID, At: call.DispatchedAt, Type: EventModelDispatched, Payload: toJSON(map[string]any{"call_id": call.ID, "attempt": attempt, "model": call.Model})})
	}); err != nil {
		return ModelResponse{}, time.Time{}, true, err
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	resp, gerr := c.cfg.Model.Generate(actx, req)
	cancel()
	rctx, rcancel := afterEffect(ctx)
	defer rcancel()
	call.CompletedAt = c.d.now()
	call.Latency = call.CompletedAt.Sub(call.DispatchedAt)
	ambiguous := func() {
		call.Status = CallAmbiguous
		call.Usage = Usage{InputTokens: EstimateInputTokens(req), OutputTokens: req.MaxOutputTokens}
		call.Cost = c.cfg.Prices.cost(call.Model, call.Usage)
	}
	if gerr == nil {
		call.Status, call.Usage = CallOK, resp.Usage
		call.Cost = c.cfg.Prices.cost(call.Model, resp.Usage)
		if err := c.record(rctx, call); err != nil {
			return ModelResponse{}, call.CompletedAt, true, err
		}
		if cerr := ctx.Err(); cerr != nil {
			return ModelResponse{}, call.CompletedAt, true, cerr
		}
		return resp, call.CompletedAt, true, nil
	}
	call.Error = gerr.Error()
	var served ServedError
	var tr TransientError
	retry := false
	var out error
	switch {
	case errors.As(gerr, &served):
		// Served and billed, but unusable: charge what the provider
		// reported. Asking again would be billed again for the same
		// reply, so it is not retried.
		call.Status, call.Usage = CallError, served.Usage
		call.Cost = c.cfg.Prices.cost(call.Model, served.Usage)
		out = ErrModelUnavailable{Attempts: attempt, Last: gerr}
	case ctx.Err() != nil:
		// The caller gave up with the request in flight.
		ambiguous()
		out = ctx.Err()
	case errors.Is(gerr, context.DeadlineExceeded) || errors.Is(actx.Err(), context.DeadlineExceeded) || isConnectionLost(gerr):
		// Sent, no answer: charge the conservative estimate.
		ambiguous()
		retry = true
	case errors.As(gerr, &tr):
		call.Status = CallError
		retry = true
	default:
		call.Status = CallError
		out = ErrModelUnavailable{Attempts: attempt, Last: gerr}
	}
	if err := c.record(rctx, call); err != nil {
		return ModelResponse{}, call.CompletedAt, true, err
	}
	if retry {
		return ModelResponse{}, call.CompletedAt, false, gerr
	}
	return ModelResponse{}, call.CompletedAt, true, out
}

// isConnectionLost reports a request that was sent and lost its connection
// before an answer: the provider may have served it.
func isConnectionLost(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "read" || op.Op == "write")
}

// reserve checks the limits against the recorded totals plus every call of
// this step still in flight, then holds this call's projection until
// release.
func (c *caller) reserve(ctx context.Context, req ModelRequest) (Usage, Micros, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, err := c.d.store.GetRun(ctx, c.runID)
	if err != nil {
		return Usage{}, 0, err
	}
	run.ModelCalls += c.reservedCalls
	run.Usage = run.Usage.add(c.reserved)
	run.EstimatedCost += c.reservedCost
	if err := c.checkLimits(run, req); err != nil {
		return Usage{}, 0, err
	}
	est := Usage{InputTokens: EstimateInputTokens(req), OutputTokens: req.MaxOutputTokens}
	cost := c.cfg.Prices.cost(c.cfg.Model.Name(), est)
	c.reservedCalls++
	c.reserved = c.reserved.add(est)
	c.reservedCost += cost
	return est, cost, nil
}

// release drops a reservation once its call is recorded in the totals.
func (c *caller) release(est Usage, cost Micros) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reservedCalls--
	c.reserved = c.reserved.add(Usage{InputTokens: -est.InputTokens, OutputTokens: -est.OutputTokens, CachedInputTokens: -est.CachedInputTokens})
	c.reservedCost -= cost
}

// checkLimits enforces the call, token, and cost limits before dispatch.
// Token and cost limits are checked against the totals so far plus a
// conservative estimate for this call, so an overrun is prevented rather
// than merely detected. The estimate counts the request's output cap, so
// under either limit a request with no cap is refused: its output, and so
// its cost, cannot be bounded.
func (c *caller) checkLimits(run Run, req ModelRequest) error {
	l := run.Limits
	if l.MaxModelCalls > 0 && run.ModelCalls >= l.MaxModelCalls {
		return ErrLimit{Reason: ReasonLimitModelCalls, Detail: fmt.Sprintf("%d model calls made, limit %d", run.ModelCalls, l.MaxModelCalls)}
	}
	if req.MaxOutputTokens <= 0 {
		const uncapped = "the request sets no output cap, so it cannot be projected; set MaxOutputTokens or Limits.MaxOutputTokensPerCall"
		if l.MaxTotalTokens > 0 {
			return ErrLimit{Reason: ReasonLimitTokens, Detail: uncapped}
		}
		if _, priced := c.cfg.Prices[c.cfg.Model.Name()]; l.MaxEstimatedCost > 0 && priced {
			return ErrLimit{Reason: ReasonLimitCost, Detail: uncapped}
		}
	}
	est := Usage{InputTokens: EstimateInputTokens(req), OutputTokens: req.MaxOutputTokens}
	if l.MaxTotalTokens > 0 {
		projected := run.Usage.InputTokens + run.Usage.OutputTokens + est.InputTokens + est.OutputTokens
		if projected > l.MaxTotalTokens {
			return ErrLimit{Reason: ReasonLimitTokens, Detail: fmt.Sprintf("projected %d tokens, limit %d", projected, l.MaxTotalTokens)}
		}
	}
	if l.MaxEstimatedCost > 0 {
		projected := run.EstimatedCost + c.cfg.Prices.cost(c.cfg.Model.Name(), est)
		if projected > l.MaxEstimatedCost {
			return ErrLimit{Reason: ReasonLimitCost, Detail: fmt.Sprintf("projected cost %d micros, limit %d", projected, l.MaxEstimatedCost)}
		}
	}
	return nil
}

// EstimateInputTokens is the conservative input estimate the limits use:
// one token per three bytes of the serialized request. Consumers can use
// it to budget before a run.
func EstimateInputTokens(req ModelRequest) int {
	b, _ := json.Marshal(req)
	return len(b)/3 + 1
}

// record persists the attempt's outcome and adds it to the run totals.
func (c *caller) record(ctx context.Context, call ModelCall) error {
	return c.d.store.tx(ctx, c.d.observer, func(t *txn) error {
		if err := t.updateModelCall(ctx, call); err != nil {
			return err
		}
		if err := t.addRunUsage(ctx, c.runID, call.Usage, call.Cost); err != nil {
			return err
		}
		typ := EventModelCompleted
		if call.Status != CallOK {
			typ = EventModelFailed
		}
		return t.appendEvent(ctx, Event{RunID: c.runID, StepID: c.stepID, At: call.CompletedAt, Type: typ, Payload: toJSON(map[string]any{
			"call_id": call.ID, "attempt": call.Attempt, "status": call.Status, "model": call.Model, "usage": call.Usage, "cost_micros": call.Cost, "latency_ms": call.Latency.Milliseconds(), "error": call.Error,
		})})
	})
}
