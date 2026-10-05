// Package otel maps a run's events onto OpenTelemetry spans, so a run's
// trace appears in whatever collector an operator already has, with no code
// in the consumer that owns the run: an Exporter is fed by export.Follower
// reading the database, read-only, from any process.
//
// # Spans
//
// Every span of a run is in one trace whose id is derived from the run id,
// and every span id is derived from the id of what it stands for, so an
// exporter restarted, or a run resumed days later, continues the trace
// instead of forking it. TraceID and SpanID are the derivations.
//
//	Span             Name                 Starts at            Ends at
//	run              agentrt.run          run.created          run.finished
//	step             agentrt.step         step.started         step.tool_finished (done), step.failed, the
//	                                                           next step.started of the run, or run.finished
//	model attempt    agentrt.model        model.dispatched     model.completed or model.failed
//	approval wait    agentrt.approval     approval.requested   approval.decided, or run.finished while pending
//	late tool        agentrt.tool.late    step.tool_finished   the same event: what a tool returned to a
//	                                      marked late          cancelled run, as a child of its step
//	unknown event    agentrt.event        an event type this   the same event: a zero-length span under the
//	                                      package does not     run, when the run span is not open; on the
//	                                      know                 run span as an event when it is
//
// Step, model, and approval spans are children of the run span, the step
// span, and the step span respectively. A span is emitted when it ends, so
// a live run's steps and model calls arrive as they complete and the run
// span arrives when the run finishes. step.interrupted does not end its
// step: an interrupted side effect may run again on the same step once an
// operator approves it, so the step ends with the rest of the run.
//
// Span names are the fixed strings above. Nothing a model chose, the goal,
// a reason, a summary, arguments, a message, or a provider's error text,
// ever names a span: it is carried in an attribute, cut at MaxText bytes
// (DefaultMaxText) on a rune boundary with a marker giving the full
// length. Everything else is carried whole, except that the follower caps
// a payload at agentrt.MaxPageText, which the span then records as
// agentrt.payload.truncated.
//
// # Span events
//
// Events of a run that are not a span of their own are span events, named
// as the runtime names them, with their payload fields as attributes:
// run.started, lease.taken_over, limit.exceeded, loop.detected, and
// run.resumed on the run span, and run.finished of a cancelled run as
// run.cancelled there; step.decided, step.policy, step.tool_started,
// step.interrupted, and approval.requested on the step span. step.started
// is a span start and has no event.
//
// # Attributes
//
// Names are stable; a release that changes one says so in the changelog.
// "text" marks a value cut at MaxText.
//
//	Attribute                                 On          From                       Value
//	agentrt.run.id                            every span  the event                  run id
//	agentrt.step.id                           step, model, approval, late            step id
//	agentrt.run.goal                          run         run.created                text
//	agentrt.limits.max_steps                  run         run.created                int
//	agentrt.limits.max_consecutive_tool_failures  run     run.created                int
//	agentrt.limits.loop_threshold             run         run.created                int
//	agentrt.limits.max_model_calls            run         run.created                int, when set
//	agentrt.limits.max_output_tokens_per_call run         run.created                int, when set
//	agentrt.limits.max_total_tokens           run         run.created                int, when set
//	agentrt.limits.max_estimated_cost_micros  run         run.created                int, when set
//	agentrt.limits.max_active_time_ms         run         run.created                int, when set
//	agentrt.limits.max_elapsed_time_ms        run         run.created                int, when set
//	agentrt.limits.approval_ttl_ms            run         run.created                int, when set
//	agentrt.limits.grant_ttl_ms               run         run.created                int, when set
//	agentrt.run.status                        run         run.finished               COMPLETED, FAILED, CANCELLED
//	agentrt.run.reason                        run         run.finished               terminal reason
//	agentrt.run.detail                        run         run.finished               text
//	agentrt.run.steps                         run         run.finished               int
//	agentrt.run.by                            run         run.finished               who cancelled, when an operator did
//	agentrt.step.index                        step        step.started               int
//	agentrt.decision.kind                     step        step.decided               tool_call, complete, fail, truncated, no_tool_call
//	agentrt.decision.tool                     step        step.decided               tool name as the agent gave it (text)
//	agentrt.decision.args                     step        step.decided               JSON, text
//	agentrt.decision.reason                   step        step.decided               text
//	agentrt.decision.result                   step        step.decided               JSON, text
//	agentrt.decision.message                  step        step.decided               text
//	agentrt.decision.invalid_args             step        step.decided               text, when the arguments were not JSON
//	agentrt.decision.invalid_result           step        step.decided               text, when the result was not JSON
//	agentrt.decision.invalid_base64           step        step.decided               bool, when set
//	agentrt.decision.origin                   step        step.decided               model, plan, operator, when set
//	agentrt.policy.outcome                    step        step.policy                allow, deny, abort, require_approval
//	agentrt.policy.reason                     step        step.policy                text
//	agentrt.policy.kind                       step        step.policy                approval kind, for require_approval
//	agentrt.tool.name                         step, late  step.tool_started/finished registered tool name
//	agentrt.tool.duration_ms                  step, late  step.tool_finished         int
//	agentrt.tool.summary                      step, late  step.tool_finished         text
//	agentrt.tool.content_hash                 step, late  step.tool_finished         SHA-256 of the observation
//	agentrt.tool.error                        step        step.tool_finished         text
//	agentrt.tool.observation                  late        step.tool_finished         JSON, text
//	agentrt.step.status                       step        the end event              done, failed, or interrupted
//	agentrt.step.end                          step        the end event              its type
//	agentrt.step.detail                       step        step.failed                text
//	agentrt.step.observation                  step        step.failed                observation kind
//	agentrt.step.content_hash                 step        step.failed                SHA-256 of the observation
//	agentrt.step.previous_status              step event  step.interrupted           deciding, awaiting_approval, executing
//	agentrt.approval.id                       approval, step event  approval.*       approval id
//	agentrt.approval.kind                     approval    approval.requested         kind
//	agentrt.approval.reason                   approval    approval.requested         text
//	agentrt.approval.hash                     approval    approval.requested         the binding hash
//	agentrt.approval.capability               approval    approval.requested         JSON, text
//	agentrt.approval.presentation             approval    approval.requested         JSON, text
//	agentrt.approval.status                   approval    approval.decided           approved, rejected, expired; pending at run end
//	agentrt.approval.by                       approval    approval.decided           who decided
//	agentrt.approval.note                     approval    approval.decided           text
//	agentrt.model.call_id                     model       model.dispatched           attempt id
//	agentrt.model.attempt                     model       model.dispatched           int, 1-based within the step
//	agentrt.model.name                        model       model.dispatched           model name
//	agentrt.model.status                      model       model.completed/failed     ok, error, ambiguous
//	agentrt.model.input_tokens                model       model.completed/failed     int
//	agentrt.model.output_tokens               model       model.completed/failed     int
//	agentrt.model.cached_input_tokens         model       model.completed/failed     int
//	agentrt.model.cost_micros                 model       model.completed/failed     int
//	agentrt.model.latency_ms                  model       model.completed/failed     int
//	agentrt.model.error                       model       model.failed               text
//	agentrt.lease.previous_owner              run event   lease.taken_over           owner
//	agentrt.lease.previous_expires_at         run event   lease.taken_over           RFC 3339
//	agentrt.lease.owner                       run event   lease.taken_over           owner
//	agentrt.limit.reason                      run event   limit.exceeded             terminal reason
//	agentrt.limit.detail                      run event   limit.exceeded             text
//	agentrt.loop.repeats                      run event   loop.detected              int
//	agentrt.loop.tool                         run event   loop.detected              tool name
//	agentrt.loop.args_hash                    run event   loop.detected              SHA-256
//	agentrt.loop.observation_hash             run event   loop.detected              SHA-256
//	agentrt.resumed.after                     run event   run.resumed                "interruption", when resumed after one
//	agentrt.event.type                        event       an unknown event           its type
//	agentrt.event.seq                         event       an unknown event           its Seq
//	agentrt.span.start_unknown                any         a missing start            true when neither the event nor Store gave the start
//	agentrt.payload.truncated                 any         any                        full length of a payload the follower capped
//
// The span status is Error for a FAILED run, a failed step, and a failed
// model attempt, Ok for a COMPLETED run, and unset otherwise.
//
// # Restarts
//
// An exporter that did not see a span's start, because it was started
// after the event, takes the start from Store when one is given: the run's
// creation time, the step's start time as stored (which, for a step
// resumed after an approval, is when it resumed), the approval's creation
// time. A model attempt's start is its end less its latency, and a step
// ended by step.tool_finished starts its duration before it, store or
// not. Otherwise the span starts at the first of its events this exporter
// saw and carries agentrt.span.start_unknown.
//
// # Wiring
//
// Provider builds a TracerProvider from the standard OTEL_* environment
// with the deterministic id generator installed; NewTracerProvider does
// the same for a caller that chooses its own exporter. A TracerProvider
// built any other way must use IDGenerator, or spans get random ids and a
// restart forks the trace. The example under example/ is the whole
// operator-side program.
package otel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	agentrt "github.com/joeylking/agent-runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// DefaultMaxText is the bound on model-chosen text in an attribute when
// Exporter.MaxText is zero.
const DefaultMaxText = 1024

// Scope is the instrumentation scope name of every span.
const Scope = "github.com/joeylking/agent-runtime/export/otel"

// Span names. Nothing else ever names a span.
const (
	SpanRun      = "agentrt.run"
	SpanStep     = "agentrt.step"
	SpanModel    = "agentrt.model"
	SpanApproval = "agentrt.approval"
	SpanLateTool = "agentrt.tool.late"
	// SpanEvent is a zero-length span standing in for a span event whose
	// span is not open: an event type this package does not know, for a
	// run that has finished.
	SpanEvent = "agentrt.event"
)

// lookupTimeout bounds a Store read for a span whose start was not seen.
const lookupTimeout = 5 * time.Second

// TraceID is the trace id of every span of a run: the first 16 bytes of
// the SHA-256 of "agentrt/trace", a zero byte, and the run id.
func TraceID(runID string) trace.TraceID {
	sum := sha256.Sum256([]byte("agentrt/trace\x00" + runID))
	var id trace.TraceID
	copy(id[:], sum[:16])
	if !id.IsValid() {
		id[15] = 1
	}
	return id
}

// SpanID is the span id of the span of kind ("run", "step", "model",
// "approval", "late") for the run, step, attempt, approval, or late step
// with that id: the first 8 bytes of the SHA-256 of "agentrt/span", a
// zero byte, the kind, a zero byte, and the id.
func SpanID(kind, id string) trace.SpanID {
	sum := sha256.Sum256([]byte("agentrt/span\x00" + kind + "\x00" + id))
	var sid trace.SpanID
	copy(sid[:], sum[:8])
	if !sid.IsValid() {
		sid[7] = 1
	}
	return sid
}

type idsKey struct{}

type ids struct {
	trace trace.TraceID
	span  trace.SpanID
}

// withIDs asks the generator for these ids for the next span started on
// the context.
func withIDs(ctx context.Context, tid trace.TraceID, sid trace.SpanID) context.Context {
	return context.WithValue(ctx, idsKey{}, ids{tid, sid})
}

// generator is the SDK id generator that honours withIDs and otherwise
// draws random ids, as the SDK's own does.
type generator struct{}

func (generator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if v, ok := ctx.Value(idsKey{}).(ids); ok {
		return v.trace, v.span
	}
	var tid trace.TraceID
	var sid trace.SpanID
	for !tid.IsValid() {
		rand.Read(tid[:])
	}
	for !sid.IsValid() {
		rand.Read(sid[:])
	}
	return tid, sid
}

func (generator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if v, ok := ctx.Value(idsKey{}).(ids); ok {
		return v.span
	}
	var sid trace.SpanID
	for !sid.IsValid() {
		rand.Read(sid[:])
	}
	return sid
}

// IDGenerator returns the id generator a TracerProvider must use for
// Exporter's spans to get their derived ids. Spans started by anything
// else on the same provider get random ids, as usual.
func IDGenerator() sdktrace.IDGenerator { return generator{} }

// NewTracerProvider is sdktrace.NewTracerProvider with IDGenerator
// installed first, so opts may still override everything else.
func NewTracerProvider(opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{sdktrace.WithIDGenerator(generator{})}, opts...)...)
}

// Exporter turns events into spans on a TracerProvider. Handle is an
// export.Sink. The zero value is usable: it traces on the global provider,
// has no Store, and cuts text at DefaultMaxText. One Exporter must see a
// run's events in order, as a Follower delivers them; it is safe for
// concurrent use.
type Exporter struct {
	// TracerProvider produces the spans. Nil means the global provider,
	// which must then have IDGenerator installed.
	TracerProvider trace.TracerProvider
	// Store, when set, is read for the start of a span whose start event
	// this Exporter did not see. It is only read: the follower's
	// read-only store serves.
	Store *agentrt.Store
	// MaxText bounds model-chosen text in attributes; zero is
	// DefaultMaxText.
	MaxText int

	once   sync.Once
	tracer trace.Tracer
	mu     sync.Mutex
	runs   map[string]*runState
}

type runState struct {
	id        string
	span      trace.Span
	steps     map[string]*stepState
	current   string
	calls     map[string]trace.Span
	approvals map[string]*approvalState
}

type stepState struct {
	span        trace.Span
	interrupted bool
}

type approvalState struct {
	span   trace.Span
	stepID string
}

func (r *runState) empty() bool {
	return r.span == nil && len(r.steps) == 0 && len(r.calls) == 0 && len(r.approvals) == 0
}

func (x *Exporter) init() {
	tp := x.TracerProvider
	if tp == nil {
		tp = globalProvider()
	}
	x.tracer = tp.Tracer(Scope)
	x.runs = map[string]*runState{}
}

func (x *Exporter) maxText() int {
	if x.MaxText > 0 {
		return x.MaxText
	}
	return DefaultMaxText
}

// Handle maps one event onto the spans of its run. It never returns an
// error: a payload that does not decode still ends the span it ends, with
// what could be read of it.
func (x *Exporter) Handle(e agentrt.Event) error {
	x.once.Do(x.init)
	x.mu.Lock()
	defer x.mu.Unlock()
	r := x.runs[e.RunID]
	if r == nil {
		r = &runState{id: e.RunID, steps: map[string]*stepState{}, calls: map[string]trace.Span{}, approvals: map[string]*approvalState{}}
		x.runs[e.RunID] = r
	}
	p := x.payload(e.Payload)
	switch e.Type {
	case agentrt.EventRunCreated:
		x.runCreated(r, e, p)
	case agentrt.EventRunStarted:
		x.runSpan(r, e.At).AddEvent(e.Type, trace.WithTimestamp(e.At))
	case agentrt.EventRunFinished:
		x.runFinished(r, e, p)
	case agentrt.EventRunResumed:
		x.runSpan(r, e.At).AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(append(p.attrs, p.str("agentrt.approval.id", "approval_id"), p.str("agentrt.resumed.after", "after")))...))
	case agentrt.EventLeaseTakenOver:
		x.runSpan(r, e.At).AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(append(p.attrs, p.str("agentrt.lease.previous_owner", "previous_owner"), p.str("agentrt.lease.previous_expires_at", "previous_expires_at"), p.str("agentrt.lease.owner", "owner")))...))
	case agentrt.EventLimitExceeded:
		x.runSpan(r, e.At).AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(append(p.attrs, p.str("agentrt.limit.reason", "reason"), p.text("agentrt.limit.detail", "detail", x.maxText())))...))
	case agentrt.EventLoopDetected:
		x.runSpan(r, e.At).AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(append(p.attrs, p.num("agentrt.loop.repeats", "repeats"), p.str("agentrt.loop.tool", "tool"), p.str("agentrt.loop.args_hash", "args_hash"), p.str("agentrt.loop.observation_hash", "observation_hash")))...))
	case agentrt.EventStepStarted:
		x.stepStarted(r, e, p)
	case agentrt.EventStepDecided:
		s := x.stepSpan(r, e)
		s.SetAttributes(valid(append(p.attrs,
			p.str("agentrt.decision.kind", "kind"), p.text("agentrt.decision.tool", "tool", x.maxText()), p.text("agentrt.decision.args", "args", x.maxText()),
			p.text("agentrt.decision.reason", "reason", x.maxText()), p.text("agentrt.decision.result", "result", x.maxText()), p.text("agentrt.decision.message", "message", x.maxText()),
			p.text("agentrt.decision.invalid_args", "invalid_args", x.maxText()), p.text("agentrt.decision.invalid_result", "invalid_result", x.maxText()), p.boolean("agentrt.decision.invalid_base64", "invalid_base64"), p.str("agentrt.decision.origin", "origin")))...)
		s.AddEvent(e.Type, trace.WithTimestamp(e.At))
	case agentrt.EventStepPolicy:
		s := x.stepSpan(r, e)
		s.SetAttributes(valid(append(p.attrs, p.str("agentrt.policy.outcome", "outcome"), p.text("agentrt.policy.reason", "reason", x.maxText()), p.str("agentrt.policy.kind", "kind")))...)
		s.AddEvent(e.Type, trace.WithTimestamp(e.At))
	case agentrt.EventStepToolStarted:
		s := x.stepSpan(r, e)
		s.SetAttributes(valid(append(p.attrs, p.str("agentrt.tool.name", "tool")))...)
		s.AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid([]attribute.KeyValue{p.str("agentrt.approval.id", "approval_id")})...))
	case agentrt.EventStepToolFinished:
		x.toolFinished(r, e, p)
	case agentrt.EventStepFailed:
		s := x.stepSpan(r, e)
		s.SetAttributes(valid(append(p.attrs, p.text("agentrt.step.detail", "detail", x.maxText()), p.str("agentrt.step.observation", "observation"), p.str("agentrt.step.content_hash", "content_hash")))...)
		s.SetStatus(codes.Error, p.string("observation"))
		x.endStep(r, e.StepID, e.At, e.Type, "failed")
	case agentrt.EventStepInterrupted:
		st := x.step(r, e, time.Time{})
		st.interrupted = true
		st.span.AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(append(p.attrs, attribute.String("agentrt.step.previous_status", p.previousStatus())))...))
	case agentrt.EventApprovalRequested:
		x.approvalRequested(r, e, p)
	case agentrt.EventApprovalDecided:
		x.approvalDecided(r, e, p)
	case agentrt.EventModelDispatched:
		x.modelDispatched(r, e, p)
	case agentrt.EventModelCompleted, agentrt.EventModelFailed:
		x.modelEnded(r, e, p)
	default:
		x.unknown(r, e, p)
	}
	if r.empty() {
		delete(x.runs, e.RunID)
	}
	return nil
}

// unknown records an event type this package does not know: on the run
// span when it is open, else as a zero-length span under the run, so a
// finished run is not reopened and never closed.
func (x *Exporter) unknown(r *runState, e agentrt.Event, p payload) {
	if r.span != nil {
		r.span.AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid(p.attrs)...))
		return
	}
	attrs := append(p.attrs, attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.event.type", e.Type), attribute.Int64("agentrt.event.seq", e.Seq))
	if e.StepID != "" {
		attrs = append(attrs, attribute.String("agentrt.step.id", e.StepID))
	}
	ctx := withIDs(runCtx(r.id), TraceID(r.id), SpanID("event", strconv.FormatInt(e.Seq, 10)))
	_, span := x.tracer.Start(ctx, SpanEvent, trace.WithTimestamp(e.At), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
	span.End(trace.WithTimestamp(e.At))
}

// runCtx is a context that makes the next span a child of the run span,
// whether or not that span is open here.
func runCtx(runID string) context.Context {
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: TraceID(runID), SpanID: SpanID("run", runID), TraceFlags: trace.FlagsSampled,
	}))
}

// stepCtx is runCtx for a child of a step span.
func stepCtx(runID, stepID string) context.Context {
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: TraceID(runID), SpanID: SpanID("step", stepID), TraceFlags: trace.FlagsSampled,
	}))
}

func (x *Exporter) runCreated(r *runState, e agentrt.Event, p payload) {
	if r.span != nil {
		return
	}
	attrs := append(p.attrs, attribute.String("agentrt.run.id", r.id), p.text("agentrt.run.goal", "goal", x.maxText()))
	var limits agentrt.Limits
	if raw, ok := p.fields["limits"]; ok && json.Unmarshal(raw, &limits) == nil {
		attrs = append(attrs,
			attribute.Int("agentrt.limits.max_steps", limits.MaxSteps),
			attribute.Int("agentrt.limits.max_consecutive_tool_failures", limits.MaxConsecutiveToolFailures),
			attribute.Int("agentrt.limits.loop_threshold", limits.LoopThreshold))
		attrs = optInt(attrs, "agentrt.limits.max_model_calls", int64(limits.MaxModelCalls))
		attrs = optInt(attrs, "agentrt.limits.max_output_tokens_per_call", int64(limits.MaxOutputTokensPerCall))
		attrs = optInt(attrs, "agentrt.limits.max_total_tokens", int64(limits.MaxTotalTokens))
		attrs = optInt(attrs, "agentrt.limits.max_estimated_cost_micros", int64(limits.MaxEstimatedCost))
		attrs = optInt(attrs, "agentrt.limits.max_active_time_ms", limits.MaxActiveTime.Milliseconds())
		attrs = optInt(attrs, "agentrt.limits.max_elapsed_time_ms", limits.MaxElapsedTime.Milliseconds())
		attrs = optInt(attrs, "agentrt.limits.approval_ttl_ms", limits.ApprovalTTL.Milliseconds())
		attrs = optInt(attrs, "agentrt.limits.grant_ttl_ms", limits.GrantTTL.Milliseconds())
	}
	ctx := withIDs(context.Background(), TraceID(r.id), SpanID("run", r.id))
	_, r.span = x.tracer.Start(ctx, SpanRun, trace.WithTimestamp(e.At), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
}

func optInt(attrs []attribute.KeyValue, key string, v int64) []attribute.KeyValue {
	if v == 0 {
		return attrs
	}
	return append(attrs, attribute.Int64(key, v))
}

// runSpan is the open run span, started from Store when run.created was
// not seen.
func (x *Exporter) runSpan(r *runState, at time.Time) trace.Span {
	if r.span != nil {
		return r.span
	}
	start, known := at, false
	if x.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		run, err := x.Store.GetRunCapped(ctx, r.id)
		cancel()
		if err == nil && !run.CreatedAt.IsZero() {
			start, known = run.CreatedAt, true
		}
	}
	attrs := []attribute.KeyValue{attribute.String("agentrt.run.id", r.id)}
	if !known {
		attrs = append(attrs, attribute.Bool("agentrt.span.start_unknown", true))
	}
	ctx := withIDs(context.Background(), TraceID(r.id), SpanID("run", r.id))
	_, r.span = x.tracer.Start(ctx, SpanRun, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
	return r.span
}

func (x *Exporter) runFinished(r *runState, e agentrt.Event, p payload) {
	span := x.runSpan(r, e.At)
	for id, a := range r.approvals {
		a.span.SetAttributes(attribute.String("agentrt.approval.status", string(agentrt.ApprovalPending)))
		a.span.End(trace.WithTimestamp(e.At))
		delete(r.approvals, id)
	}
	for id, c := range r.calls {
		c.SetAttributes(attribute.String("agentrt.model.status", "unknown"))
		c.End(trace.WithTimestamp(e.At))
		delete(r.calls, id)
	}
	for id := range r.steps {
		x.endStep(r, id, e.At, e.Type, "")
	}
	status := p.string("status")
	span.SetAttributes(valid(append(p.attrs, attribute.String("agentrt.run.status", status), p.str("agentrt.run.reason", "reason"), p.text("agentrt.run.detail", "detail", x.maxText()), p.num("agentrt.run.steps", "steps"), p.str("agentrt.run.by", "by")))...)
	switch agentrt.RunStatus(status) {
	case agentrt.StatusCompleted:
		span.SetStatus(codes.Ok, "")
	case agentrt.StatusFailed:
		span.SetStatus(codes.Error, p.string("reason"))
	case agentrt.StatusCancelled:
		span.AddEvent("run.cancelled", trace.WithTimestamp(e.At), trace.WithAttributes(valid([]attribute.KeyValue{p.str("agentrt.run.reason", "reason"), p.str("agentrt.run.by", "by"), p.text("agentrt.run.detail", "detail", x.maxText())})...))
	}
	span.End(trace.WithTimestamp(e.At))
	r.span = nil
	r.current = ""
}

func (x *Exporter) stepStarted(r *runState, e agentrt.Event, p payload) {
	if r.current != "" && r.current != e.StepID {
		x.endStep(r, r.current, e.At, e.Type, "")
	}
	r.current = e.StepID
	if _, open := r.steps[e.StepID]; open {
		return
	}
	attrs := append(p.attrs, attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), p.num("agentrt.step.index", "index"))
	ctx := withIDs(runCtx(r.id), TraceID(r.id), SpanID("step", e.StepID))
	_, span := x.tracer.Start(ctx, SpanStep, trace.WithTimestamp(e.At), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
	r.steps[e.StepID] = &stepState{span: span}
}

// step is the open step for the event, started from Store when
// step.started was not seen, else at fallback when the event implies a
// start, else at the event with the start marked unknown.
func (x *Exporter) step(r *runState, e agentrt.Event, fallback time.Time) *stepState {
	if st := r.steps[e.StepID]; st != nil {
		return st
	}
	start, known := fallback, !fallback.IsZero()
	attrs := []attribute.KeyValue{attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID)}
	if x.Store != nil {
		if s, ok := x.lookupStep(r.id, e.StepID); ok && !s.StartedAt.IsZero() {
			start, known = s.StartedAt, true
			attrs = append(attrs, attribute.Int("agentrt.step.index", s.Index))
		}
	}
	if !known {
		start = e.At
		attrs = append(attrs, attribute.Bool("agentrt.span.start_unknown", true))
	}
	ctx := withIDs(runCtx(r.id), TraceID(r.id), SpanID("step", e.StepID))
	_, span := x.tracer.Start(ctx, SpanStep, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
	st := &stepState{span: span}
	r.steps[e.StepID] = st
	if r.current == "" {
		r.current = e.StepID
	}
	return st
}

func (x *Exporter) stepSpan(r *runState, e agentrt.Event) trace.Span {
	return x.step(r, e, time.Time{}).span
}

// lookupStep reads the step from Store a page at a time.
func (x *Exporter) lookupStep(runID, stepID string) (agentrt.Step, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	const page = 200
	for offset := 0; ; offset += page {
		steps, total, err := x.Store.ListStepsPage(ctx, runID, page, offset)
		if err != nil {
			return agentrt.Step{}, false
		}
		for _, s := range steps {
			if s.ID == stepID {
				return s, true
			}
		}
		if offset+len(steps) >= total || len(steps) == 0 {
			return agentrt.Step{}, false
		}
	}
}

// endStep ends an open step at the event that ends it. status is what the
// end event says, or empty when the end is the next step or the run's end,
// in which case an interrupted step is reported as such.
func (x *Exporter) endStep(r *runState, stepID string, at time.Time, end, status string) {
	st := r.steps[stepID]
	if st == nil {
		return
	}
	if status == "" && st.interrupted {
		status = string(agentrt.StepInterrupted)
	}
	attrs := []attribute.KeyValue{attribute.String("agentrt.step.end", end)}
	if status != "" {
		attrs = append(attrs, attribute.String("agentrt.step.status", status))
	}
	st.span.SetAttributes(attrs...)
	st.span.End(trace.WithTimestamp(at))
	delete(r.steps, stepID)
	if r.current == stepID {
		r.current = ""
	}
}

func (x *Exporter) toolFinished(r *runState, e agentrt.Event, p payload) {
	attrs := append(p.attrs, p.str("agentrt.tool.name", "tool"), p.num("agentrt.tool.duration_ms", "duration_ms"), p.text("agentrt.tool.summary", "summary", x.maxText()), p.str("agentrt.tool.content_hash", "content_hash"))
	if late, _ := p.fields["late"]; len(late) > 0 && string(late) == "true" {
		// The run was cancelled while the tool ran; the step and the run
		// are over. The outcome is its own span under the step.
		start := e.At
		if ms, ok := p.int("duration_ms"); ok {
			start = e.At.Add(-time.Duration(ms) * time.Millisecond)
		}
		attrs = append(attrs, attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), p.text("agentrt.tool.observation", "observation", x.maxText()))
		ctx := withIDs(stepCtx(r.id, e.StepID), TraceID(r.id), SpanID("late", e.StepID))
		_, span := x.tracer.Start(ctx, SpanLateTool, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
		span.End(trace.WithTimestamp(e.At))
		return
	}
	// Not seen started: the tool's duration is measured from the step's
	// start, which for a resumed step is when it resumed.
	var implied time.Time
	if ms, ok := p.int("duration_ms"); ok {
		implied = e.At.Add(-time.Duration(ms) * time.Millisecond)
	}
	st := x.step(r, e, implied)
	st.span.SetAttributes(valid(attrs)...)
	if _, failed := p.fields["error"]; failed {
		st.span.SetAttributes(valid([]attribute.KeyValue{p.text("agentrt.tool.error", "error", x.maxText())})...)
		// step.failed follows in the same transaction and ends the step.
		return
	}
	x.endStep(r, e.StepID, e.At, e.Type, string(agentrt.StepDone))
}

func (x *Exporter) approvalRequested(r *runState, e agentrt.Event, p payload) {
	id := p.string("approval_id")
	x.stepSpan(r, e).AddEvent(e.Type, trace.WithTimestamp(e.At), trace.WithAttributes(valid([]attribute.KeyValue{attribute.String("agentrt.approval.id", id), p.str("agentrt.approval.kind", "kind")})...))
	if _, open := r.approvals[id]; open {
		return
	}
	attrs := append(p.attrs, attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), attribute.String("agentrt.approval.id", id),
		p.str("agentrt.approval.kind", "kind"), p.text("agentrt.approval.reason", "reason", x.maxText()), p.str("agentrt.approval.hash", "hash"),
		p.text("agentrt.approval.capability", "capability", x.maxText()), p.text("agentrt.approval.presentation", "presentation", x.maxText()))
	ctx := withIDs(stepCtx(r.id, e.StepID), TraceID(r.id), SpanID("approval", id))
	_, span := x.tracer.Start(ctx, SpanApproval, trace.WithTimestamp(e.At), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
	r.approvals[id] = &approvalState{span: span, stepID: e.StepID}
}

func (x *Exporter) approvalDecided(r *runState, e agentrt.Event, p payload) {
	id := p.string("approval_id")
	a := r.approvals[id]
	if a == nil {
		start, known := e.At, false
		attrs := []attribute.KeyValue{attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), attribute.String("agentrt.approval.id", id)}
		if x.Store != nil {
			ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
			ap, err := x.Store.GetApproval(ctx, r.id, id)
			cancel()
			if err == nil && !ap.CreatedAt.IsZero() {
				start, known = ap.CreatedAt, true
				attrs = append(attrs, attribute.String("agentrt.approval.kind", ap.Kind), attribute.String("agentrt.approval.hash", ap.Hash))
			}
		}
		if !known {
			attrs = append(attrs, attribute.Bool("agentrt.span.start_unknown", true))
		}
		ctx := withIDs(stepCtx(r.id, e.StepID), TraceID(r.id), SpanID("approval", id))
		_, span := x.tracer.Start(ctx, SpanApproval, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(valid(attrs)...))
		a = &approvalState{span: span, stepID: e.StepID}
	}
	a.span.SetAttributes(valid(append(p.attrs, p.str("agentrt.approval.status", "status"), p.str("agentrt.approval.by", "by"), p.text("agentrt.approval.note", "note", x.maxText())))...)
	a.span.End(trace.WithTimestamp(e.At))
	delete(r.approvals, id)
}

func (x *Exporter) modelDispatched(r *runState, e agentrt.Event, p payload) {
	id := p.string("call_id")
	if _, open := r.calls[id]; open {
		return
	}
	attrs := append(p.attrs, attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), attribute.String("agentrt.model.call_id", id), p.num("agentrt.model.attempt", "attempt"), p.str("agentrt.model.name", "model"))
	ctx := withIDs(stepCtx(r.id, e.StepID), TraceID(r.id), SpanID("model", id))
	_, span := x.tracer.Start(ctx, SpanModel, trace.WithTimestamp(e.At), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(valid(attrs)...))
	r.calls[id] = span
}

func (x *Exporter) modelEnded(r *runState, e agentrt.Event, p payload) {
	id := p.string("call_id")
	span := r.calls[id]
	if span == nil {
		start := e.At
		if ms, ok := p.int("latency_ms"); ok {
			start = e.At.Add(-time.Duration(ms) * time.Millisecond)
		}
		attrs := []attribute.KeyValue{attribute.String("agentrt.run.id", r.id), attribute.String("agentrt.step.id", e.StepID), attribute.String("agentrt.model.call_id", id), p.num("agentrt.model.attempt", "attempt"), p.str("agentrt.model.name", "model")}
		ctx := withIDs(stepCtx(r.id, e.StepID), TraceID(r.id), SpanID("model", id))
		_, span = x.tracer.Start(ctx, SpanModel, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(valid(attrs)...))
	}
	var usage agentrt.Usage
	if raw, ok := p.fields["usage"]; ok {
		json.Unmarshal(raw, &usage)
	}
	span.SetAttributes(valid(append(p.attrs, p.str("agentrt.model.status", "status"),
		attribute.Int("agentrt.model.input_tokens", usage.InputTokens), attribute.Int("agentrt.model.output_tokens", usage.OutputTokens), attribute.Int("agentrt.model.cached_input_tokens", usage.CachedInputTokens),
		p.num("agentrt.model.cost_micros", "cost_micros"), p.num("agentrt.model.latency_ms", "latency_ms")))...)
	if e.Type == agentrt.EventModelFailed {
		span.SetAttributes(valid([]attribute.KeyValue{p.text("agentrt.model.error", "error", x.maxText())})...)
		span.SetStatus(codes.Error, p.string("status"))
	}
	span.End(trace.WithTimestamp(e.At))
	delete(r.calls, id)
}

// valid drops the empty KeyValues the payload helpers return for absent
// fields: the SDK drops them from a span's attributes but not from an
// event's.
func valid(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := attrs[:0]
	for _, a := range attrs {
		if a.Valid() {
			out = append(out, a)
		}
	}
	return out
}

// payload is a decoded event payload: its top-level fields as raw JSON,
// and attributes every span or event it touches carries, which is
// agentrt.payload.truncated when the follower capped it.
type payload struct {
	fields map[string]json.RawMessage
	attrs  []attribute.KeyValue
}

func (x *Exporter) payload(raw json.RawMessage) payload {
	var p payload
	if json.Unmarshal(raw, &p.fields) != nil {
		p.fields = nil
		return p
	}
	if t, ok := p.fields["truncated"]; ok && string(t) == "true" {
		if n, ok := p.fields["length"]; ok {
			if v, err := strconv.ParseInt(string(n), 10, 64); err == nil {
				p.attrs = append(p.attrs, attribute.Int64("agentrt.payload.truncated", v))
			}
		}
	}
	return p
}

// string is the field as a string: a JSON string's value, or any other
// value's JSON.
func (p payload) string(field string) string {
	raw, ok := p.fields[field]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (p payload) int(field string) (int64, bool) {
	raw, ok := p.fields[field]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	return v, err == nil
}

// str, num, boolean, and text are attributes from a field, or an empty
// KeyValue, which the SDK drops, when the field is absent.
func (p payload) str(key, field string) attribute.KeyValue {
	s := p.string(field)
	if s == "" {
		return attribute.KeyValue{}
	}
	return attribute.String(key, s)
}

func (p payload) num(key, field string) attribute.KeyValue {
	v, ok := p.int(field)
	if !ok {
		return attribute.KeyValue{}
	}
	return attribute.Int64(key, v)
}

func (p payload) boolean(key, field string) attribute.KeyValue {
	raw, ok := p.fields[field]
	if !ok || string(raw) != "true" {
		return attribute.KeyValue{}
	}
	return attribute.Bool(key, true)
}

func (p payload) text(key, field string, max int) attribute.KeyValue {
	s := p.string(field)
	if s == "" {
		return attribute.KeyValue{}
	}
	return attribute.String(key, Truncate(s, max))
}

// previousStatus reads step.interrupted's previous_status, which the
// runtime writes as the interrupted observation's content, an object
// holding the status.
func (p payload) previousStatus() string {
	raw, ok := p.fields["previous_status"]
	if !ok {
		return ""
	}
	var obj struct {
		PreviousStatus string `json:"previous_status"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.PreviousStatus != "" {
		return obj.PreviousStatus
	}
	return p.string("previous_status")
}

// Truncate cuts s to at most max bytes on a rune boundary and appends a
// marker giving the full length, as the store's page reads mark a capped
// column. A string within max is returned as is.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated: " + strconv.Itoa(len(s)) + " bytes]"
}
