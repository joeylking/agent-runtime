package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
}

func (e TransientError) Error() string { return "transient: " + e.Err.Error() }
func (e TransientError) Unwrap() error { return e.Err }

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
}

type caller struct {
	d      *Driver
	cfg    ModelConfig
	runID  string
	stepID string
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
	if err := c.checkLimits(run, req); err != nil {
		return ModelResponse{}, err
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
		// A retry is a new call against the limits.
		if attempt > 1 {
			run, err = c.d.store.GetRun(ctx, c.runID)
			if err != nil {
				return ModelResponse{}, err
			}
			if err := c.checkLimits(run, req); err != nil {
				return ModelResponse{}, err
			}
		}
		call := ModelCall{ID: c.d.newID(), RunID: c.runID, StepID: c.stepID, Attempt: attempt, Status: CallDispatched, Model: c.cfg.Model.Name(), DispatchedAt: c.d.now()}
		if err := c.d.store.tx(ctx, c.d.observer, func(t *txn) error {
			if err := t.insertModelCall(ctx, call); err != nil {
				return err
			}
			return t.appendEvent(ctx, Event{RunID: c.runID, StepID: c.stepID, At: call.DispatchedAt, Type: EventModelDispatched, Payload: mustJSON(map[string]any{"call_id": call.ID, "attempt": attempt, "model": call.Model})})
		}); err != nil {
			return ModelResponse{}, err
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		resp, gerr := c.cfg.Model.Generate(actx, req)
		cancel()
		call.CompletedAt = c.d.now()
		call.Latency = call.CompletedAt.Sub(call.DispatchedAt)
		if gerr == nil {
			call.Status, call.Usage = CallOK, resp.Usage
			call.Cost = c.cfg.Prices.cost(call.Model, resp.Usage)
			if err := c.record(ctx, call, run); err != nil {
				return ModelResponse{}, err
			}
			return resp, nil
		}
		last = gerr
		call.Error = gerr.Error()
		var tr TransientError
		switch {
		case errors.Is(gerr, context.DeadlineExceeded) || errors.Is(actx.Err(), context.DeadlineExceeded) || isConnectionLost(gerr):
			// Sent, no answer: charge the conservative estimate.
			call.Status = CallAmbiguous
			call.Usage = Usage{InputTokens: EstimateInputTokens(req), OutputTokens: req.MaxOutputTokens}
			call.Cost = c.cfg.Prices.cost(call.Model, call.Usage)
		case errors.As(gerr, &tr):
			call.Status = CallError
		default:
			call.Status = CallError
			if err := c.record(ctx, call, run); err != nil {
				return ModelResponse{}, err
			}
			return ModelResponse{}, ErrModelUnavailable{Attempts: attempt, Last: gerr}
		}
		if err := c.record(ctx, call, run); err != nil {
			return ModelResponse{}, err
		}
		if attempt <= retries {
			select {
			case <-time.After(backoff * time.Duration(1<<(attempt-1))):
			case <-ctx.Done():
				return ModelResponse{}, ctx.Err()
			}
		}
	}
	return ModelResponse{}, ErrModelUnavailable{Attempts: retries + 1, Last: last}
}

func isConnectionLost(err error) bool {
	s := err.Error()
	return strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") || strings.Contains(s, "unexpected EOF") || strings.Contains(s, "EOF")
}

// checkLimits enforces the call, token, and cost limits before dispatch.
// Token and cost limits are checked against the totals so far plus a
// conservative estimate for this call, so an overrun is prevented rather
// than merely detected.
func (c *caller) checkLimits(run Run, req ModelRequest) error {
	l := run.Limits
	if l.MaxModelCalls > 0 && run.ModelCalls >= l.MaxModelCalls {
		return ErrLimit{Reason: ReasonLimitModelCalls, Detail: fmt.Sprintf("%d model calls made, limit %d", run.ModelCalls, l.MaxModelCalls)}
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
func (c *caller) record(ctx context.Context, call ModelCall, run Run) error {
	return c.d.store.tx(ctx, c.d.observer, func(t *txn) error {
		if err := t.updateModelCall(ctx, call); err != nil {
			return err
		}
		if err := t.addRunUsage(ctx, run.ID, call.Usage, call.Cost); err != nil {
			return err
		}
		typ := EventModelCompleted
		if call.Status != CallOK {
			typ = EventModelFailed
		}
		return t.appendEvent(ctx, Event{RunID: c.runID, StepID: c.stepID, At: call.CompletedAt, Type: typ, Payload: mustJSON(map[string]any{
			"call_id": call.ID, "attempt": call.Attempt, "status": call.Status, "model": call.Model, "usage": call.Usage, "cost_micros": call.Cost, "latency_ms": call.Latency.Milliseconds(), "error": call.Error,
		})})
	})
}
