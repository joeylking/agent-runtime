package otel_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/export"
	"github.com/joeylking/agent-runtime/export/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// exportAll runs every fixture event through one Exporter and returns the
// spans, in end order.
func exportAll(t *testing.T, f *fixtures, cfg func(*otel.Exporter)) tracetest.SpanStubs {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := otel.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	x := &otel.Exporter{TracerProvider: tp, Store: f.store}
	if cfg != nil {
		cfg(x)
	}
	if err := (&export.Follower{Store: f.store, Once: true}).Follow(context.Background(), x.Handle); err != nil {
		t.Fatal(err)
	}
	return mem.GetSpans()
}

func ofRun(spans tracetest.SpanStubs, runID string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.SpanContext.TraceID() == otel.TraceID(runID) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartTime.Before(out[j].StartTime) })
	return out
}

func named(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func str(t *testing.T, s tracetest.SpanStub, key string) string {
	t.Helper()
	v, ok := attr(s, key)
	if !ok {
		t.Fatalf("%s span %s has no %s; has %v", s.Name, s.SpanContext.SpanID(), key, s.Attributes)
	}
	return v.String()
}

func noAttr(t *testing.T, s tracetest.SpanStub, key string) {
	t.Helper()
	if v, ok := attr(s, key); ok {
		t.Fatalf("%s span has %s=%s", s.Name, key, v.String())
	}
}

func event(s tracetest.SpanStub, name string) (sdktrace.Event, bool) {
	for _, e := range s.Events {
		if e.Name == name {
			return e, true
		}
	}
	return sdktrace.Event{}, false
}

func eventAttr(t *testing.T, s tracetest.SpanStub, name, key string) string {
	t.Helper()
	e, ok := event(s, name)
	if !ok {
		t.Fatalf("%s span has no event %s; has %v", s.Name, name, s.Events)
	}
	for _, a := range e.Attributes {
		if string(a.Key) == key {
			return a.Value.String()
		}
	}
	t.Fatalf("event %s has no %s; has %v", name, key, e.Attributes)
	return ""
}

func eventsOf(t *testing.T, f *fixtures, runID string) []agentrt.Event {
	t.Helper()
	events, err := f.store.ListEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func stepIDs(events []agentrt.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type == agentrt.EventStepStarted {
			out = append(out, e.StepID)
		}
	}
	return out
}

// The fixtures emit every event type the runtime has, so the mapping of
// each is exercised below.
func TestFixtures_EmitEveryEventType(t *testing.T) {
	f := loadFixtures(t)
	seen := map[string]bool{}
	var all []agentrt.Event
	if err := (&export.Follower{Store: f.store, Once: true}).Follow(context.Background(), func(e agentrt.Event) error {
		seen[e.Type] = true
		all = append(all, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range eventTypes {
		if !seen[typ] {
			t.Errorf("no fixture emits %s", typ)
		}
	}
	late := false
	for _, e := range all {
		if e.Type == agentrt.EventStepToolFinished && field(t, e, "late") == "true" {
			late = true
		}
	}
	if !late {
		t.Error("no fixture emits a late step.tool_finished")
	}
}

// A completed run is one run span with a step span per step, parented to
// it, in the trace derived from the run id; the decision, policy, and tool
// outcome are attributes of the step; a failed step carries its status.
func TestExporter_RunAndStepSpans(t *testing.T) {
	f := loadFixtures(t)
	spans := ofRun(exportAll(t, f, nil), f.completed)
	events := eventsOf(t, f, f.completed)
	runs, steps := named(spans, otel.SpanRun), named(spans, otel.SpanStep)
	if len(runs) != 1 || len(steps) != len(stepIDs(events)) || len(spans) != 1+len(steps) {
		t.Fatalf("got %d run, %d step spans of %d; want 1 and %d", len(runs), len(steps), len(spans), len(stepIDs(events)))
	}
	run := runs[0]
	if run.SpanContext.SpanID() != otel.SpanID("run", f.completed) || run.Parent.IsValid() {
		t.Fatalf("run span id %s parent %v", run.SpanContext.SpanID(), run.Parent)
	}
	if !run.StartTime.Equal(events[0].At) || !run.EndTime.Equal(events[len(events)-1].At) {
		t.Fatalf("run span %v-%v, events %v-%v", run.StartTime, run.EndTime, events[0].At, events[len(events)-1].At)
	}
	if str(t, run, "agentrt.run.status") != "COMPLETED" || str(t, run, "agentrt.run.reason") != "goal_completed" || str(t, run, "agentrt.run.steps") != "5" || run.Status.Code != codes.Ok {
		t.Fatalf("run attributes %v status %v", run.Attributes, run.Status)
	}
	if str(t, run, "agentrt.limits.max_steps") != "10" || str(t, run, "agentrt.limits.loop_threshold") != "3" {
		t.Fatalf("limits %v", run.Attributes)
	}
	noAttr(t, run, "agentrt.limits.max_model_calls")
	for i, s := range steps {
		if s.Parent.SpanID() != run.SpanContext.SpanID() || s.SpanContext.SpanID() != otel.SpanID("step", stepIDs(events)[i]) {
			t.Fatalf("step %d: span %s parent %s", i, s.SpanContext.SpanID(), s.Parent.SpanID())
		}
		if str(t, s, "agentrt.step.index") != fmt.Sprint(i) || str(t, s, "agentrt.run.id") != f.completed {
			t.Fatalf("step %d attributes %v", i, s.Attributes)
		}
	}
	// Step 0: read, done.
	s := steps[0]
	if str(t, s, "agentrt.decision.kind") != "tool_call" || str(t, s, "agentrt.decision.tool") != "read" || str(t, s, "agentrt.decision.args") != `{"n":1}` || str(t, s, "agentrt.policy.outcome") != "allow" || str(t, s, "agentrt.tool.name") != "read" || str(t, s, "agentrt.tool.summary") != "read done" || str(t, s, "agentrt.step.status") != "done" || str(t, s, "agentrt.step.end") != agentrt.EventStepToolFinished {
		t.Fatalf("step 0 attributes %v", s.Attributes)
	}
	if _, ok := attr(s, "agentrt.tool.duration_ms"); !ok {
		t.Fatal("step 0 has no duration")
	}
	if len(str(t, s, "agentrt.tool.content_hash")) != 64 {
		t.Fatalf("content hash %q", str(t, s, "agentrt.tool.content_hash"))
	}
	for _, name := range []string{agentrt.EventStepDecided, agentrt.EventStepPolicy, agentrt.EventStepToolStarted} {
		if _, ok := event(s, name); !ok {
			t.Fatalf("step 0 has no %s event; has %v", name, s.Events)
		}
	}
	// Step 1: malformed arguments are an invalid decision; no policy, no tool.
	s = steps[1]
	if str(t, s, "agentrt.step.status") != "failed" || str(t, s, "agentrt.step.observation") != "invalid_decision" || str(t, s, "agentrt.step.end") != agentrt.EventStepFailed || s.Status.Code != codes.Error {
		t.Fatalf("step 1 attributes %v status %v", s.Attributes, s.Status)
	}
	if _, ok := attr(s, "agentrt.step.detail"); !ok {
		t.Fatal("step 1 has no detail")
	}
	noAttr(t, s, "agentrt.policy.outcome")
	noAttr(t, s, "agentrt.tool.name")
	// Step 2: denied by policy.
	s = steps[2]
	if str(t, s, "agentrt.policy.outcome") != "deny" || str(t, s, "agentrt.step.observation") != "policy_denied" || str(t, s, "agentrt.step.status") != "failed" {
		t.Fatalf("step 2 attributes %v", s.Attributes)
	}
	// Step 4: the completion has no end event of its own; the run's end ends it.
	s = steps[4]
	if str(t, s, "agentrt.decision.kind") != "complete" || str(t, s, "agentrt.decision.result") != `{"done":true}` || str(t, s, "agentrt.step.end") != agentrt.EventRunFinished || !s.EndTime.Equal(run.EndTime) {
		t.Fatalf("step 4 attributes %v end %v", s.Attributes, s.EndTime)
	}
	noAttr(t, s, "agentrt.step.status")
}

// Each model attempt is a client span under its step, from dispatch to
// completion, with usage and cost; a failed attempt has an error status.
func TestExporter_ModelAttemptSpans(t *testing.T) {
	f := loadFixtures(t)
	spans := ofRun(exportAll(t, f, nil), f.model)
	events := eventsOf(t, f, f.model)
	models := named(spans, otel.SpanModel)
	if len(models) != 3 {
		t.Fatalf("%d model spans, want 3 (a failed attempt, a retry, a final call)", len(models))
	}
	step0 := otel.SpanID("step", stepIDs(events)[0])
	failed, retry, final := models[0], models[1], models[2]
	if failed.Parent.SpanID() != step0 || retry.Parent.SpanID() != step0 || final.Parent.SpanID() == step0 {
		t.Fatalf("parents %s %s %s, step 0 is %s", failed.Parent.SpanID(), retry.Parent.SpanID(), final.Parent.SpanID(), step0)
	}
	if failed.SpanKind != trace.SpanKindClient || failed.Status.Code != codes.Error || str(t, failed, "agentrt.model.status") != "error" || str(t, failed, "agentrt.model.attempt") != "1" || !strings.Contains(str(t, failed, "agentrt.model.error"), "429") || str(t, failed, "agentrt.model.name") != "scripted-model" {
		t.Fatalf("failed attempt %v %v", failed.Attributes, failed.Status)
	}
	if retry.Status.Code != codes.Unset || str(t, retry, "agentrt.model.status") != "ok" || str(t, retry, "agentrt.model.attempt") != "2" || str(t, retry, "agentrt.model.input_tokens") != "100" || str(t, retry, "agentrt.model.output_tokens") != "20" || str(t, retry, "agentrt.model.cost_micros") != "200" {
		t.Fatalf("retry %v", retry.Attributes)
	}
	if str(t, final, "agentrt.model.cached_input_tokens") != "50" {
		t.Fatalf("final %v", final.Attributes)
	}
	var dispatched, completed []agentrt.Event
	for _, e := range events {
		switch e.Type {
		case agentrt.EventModelDispatched:
			dispatched = append(dispatched, e)
		case agentrt.EventModelCompleted, agentrt.EventModelFailed:
			completed = append(completed, e)
		}
	}
	for i, m := range models {
		if m.SpanContext.SpanID() != otel.SpanID("model", strings.Trim(field(t, dispatched[i], "call_id"), `"`)) || !m.StartTime.Equal(dispatched[i].At) || !m.EndTime.Equal(completed[i].At) {
			t.Fatalf("attempt %d: span %s %v-%v, events %v-%v", i, m.SpanContext.SpanID(), m.StartTime, m.EndTime, dispatched[i].At, completed[i].At)
		}
		if _, ok := attr(m, "agentrt.model.latency_ms"); !ok {
			t.Fatalf("attempt %d has no latency", i)
		}
	}
	if len(named(spans, otel.SpanStep)) != 2 || len(named(spans, otel.SpanRun)) != 1 || len(spans) != 6 {
		t.Fatalf("spans %d", len(spans))
	}
}

// The approval wait is a span under the step, from the request to the
// decision, and the resume is an event on the run span naming the grant.
func TestExporter_ApprovalWaitIsASpan(t *testing.T) {
	f := loadFixtures(t)
	spans := ofRun(exportAll(t, f, nil), f.paused)
	events := eventsOf(t, f, f.paused)
	approvals := named(spans, otel.SpanApproval)
	if len(approvals) != 1 {
		t.Fatalf("%d approval spans", len(approvals))
	}
	a := approvals[0]
	publish := stepIDs(events)[1]
	if a.Parent.SpanID() != otel.SpanID("step", publish) {
		t.Fatalf("approval parent %s, step is %s", a.Parent.SpanID(), otel.SpanID("step", publish))
	}
	var requested, decided agentrt.Event
	for _, e := range events {
		switch e.Type {
		case agentrt.EventApprovalRequested:
			requested = e
		case agentrt.EventApprovalDecided:
			decided = e
		}
	}
	id := strings.Trim(field(t, requested, "approval_id"), `"`)
	if a.SpanContext.SpanID() != otel.SpanID("approval", id) || !a.StartTime.Equal(requested.At) || !a.EndTime.Equal(decided.At) {
		t.Fatalf("approval span %s %v-%v", a.SpanContext.SpanID(), a.StartTime, a.EndTime)
	}
	if str(t, a, "agentrt.approval.id") != id || str(t, a, "agentrt.approval.status") != "approved" || str(t, a, "agentrt.approval.by") != "operator" || str(t, a, "agentrt.approval.note") != "looks right" || len(str(t, a, "agentrt.approval.hash")) != 64 || str(t, a, "agentrt.approval.kind") == "" {
		t.Fatalf("approval attributes %v", a.Attributes)
	}
	step := named(spans, otel.SpanStep)[1]
	if str(t, step, "agentrt.policy.outcome") != "require_approval" || str(t, step, "agentrt.step.status") != "done" || eventAttr(t, step, agentrt.EventApprovalRequested, "agentrt.approval.id") != id || eventAttr(t, step, agentrt.EventStepToolStarted, "agentrt.approval.id") != id {
		t.Fatalf("step attributes %v events %v", step.Attributes, step.Events)
	}
	if a.EndTime.After(step.EndTime) || step.EndTime.Before(a.EndTime) {
		t.Fatalf("approval ended %v, step %v", a.EndTime, step.EndTime)
	}
	run := named(spans, otel.SpanRun)[0]
	if eventAttr(t, run, agentrt.EventRunResumed, "agentrt.approval.id") != id {
		t.Fatalf("run events %v", run.Events)
	}
}

// A run interrupted by a dead process and resumed by another stays one
// trace: the takeover and the interruption are events, the pause on the
// unknown side effect is an approval span of kind interrupted_side_effect,
// and the step that ran twice is one span, done.
func TestExporter_InterruptedRunContinuesTheTrace(t *testing.T) {
	f := loadFixtures(t)
	spans := ofRun(exportAll(t, f, nil), f.interrupted)
	events := eventsOf(t, f, f.interrupted)
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	want := "run.created run.started step.started step.decided step.policy step.tool_started lease.taken_over step.interrupted step.policy approval.requested approval.decided run.resumed step.policy step.tool_started step.tool_finished step.started step.decided run.finished"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("fixture events:\n%s\nwant\n%s", got, want)
	}
	run := named(spans, otel.SpanRun)[0]
	if eventAttr(t, run, agentrt.EventLeaseTakenOver, "agentrt.lease.previous_owner") == "" || !strings.HasPrefix(eventAttr(t, run, agentrt.EventLeaseTakenOver, "agentrt.lease.owner"), "resumer") || eventAttr(t, run, agentrt.EventLeaseTakenOver, "agentrt.lease.previous_expires_at") == "" {
		t.Fatalf("takeover %v", run.Events)
	}
	if !strings.HasPrefix(eventAttr(t, run, agentrt.EventLeaseTakenOver, "agentrt.lease.previous_owner"), "crasher") {
		t.Fatalf("previous owner %s", eventAttr(t, run, agentrt.EventLeaseTakenOver, "agentrt.lease.previous_owner"))
	}
	if eventAttr(t, run, agentrt.EventRunResumed, "agentrt.approval.id") == "" {
		t.Fatalf("resume %v", run.Events)
	}
	steps := named(spans, otel.SpanStep)
	if len(steps) != 2 {
		t.Fatalf("%d step spans, want 2: the step ran twice but is one step", len(steps))
	}
	s := steps[0]
	if eventAttr(t, s, agentrt.EventStepInterrupted, "agentrt.step.previous_status") != "executing" || str(t, s, "agentrt.step.status") != "done" || str(t, s, "agentrt.tool.summary") != "write done" || str(t, s, "agentrt.policy.kind") != agentrt.InterruptedSideEffect {
		t.Fatalf("interrupted step %v %v", s.Attributes, s.Events)
	}
	a := named(spans, otel.SpanApproval)
	if len(a) != 1 || str(t, a[0], "agentrt.approval.kind") != agentrt.InterruptedSideEffect || str(t, a[0], "agentrt.approval.status") != "approved" || !strings.Contains(str(t, a[0], "agentrt.approval.presentation"), "write") {
		t.Fatalf("approval %v", a)
	}
	if str(t, run, "agentrt.run.status") != "COMPLETED" {
		t.Fatalf("run %v", run.Attributes)
	}
}

// An operator's cancel is an event on the run span, the step in flight
// ends with the run, and what the tool returned afterwards is a span of
// its own under the step.
func TestExporter_CancelledRunAndLateTool(t *testing.T) {
	f := loadFixtures(t)
	spans := ofRun(exportAll(t, f, nil), f.cancelled)
	events := eventsOf(t, f, f.cancelled)
	run := named(spans, otel.SpanRun)[0]
	if str(t, run, "agentrt.run.status") != "CANCELLED" || str(t, run, "agentrt.run.reason") != "operator_cancelled" || str(t, run, "agentrt.run.by") != "operator" || str(t, run, "agentrt.run.detail") != "changed my mind" || run.Status.Code != codes.Unset {
		t.Fatalf("run %v %v", run.Attributes, run.Status)
	}
	if eventAttr(t, run, "run.cancelled", "agentrt.run.by") != "operator" {
		t.Fatalf("run events %v", run.Events)
	}
	steps := named(spans, otel.SpanStep)
	if len(steps) != 1 || str(t, steps[0], "agentrt.step.end") != agentrt.EventRunFinished || !steps[0].EndTime.Equal(run.EndTime) {
		t.Fatalf("steps %v", steps)
	}
	late := named(spans, otel.SpanLateTool)
	if len(late) != 1 {
		t.Fatalf("%d late spans", len(late))
	}
	l := late[0]
	stepID := stepIDs(events)[0]
	if l.Parent.SpanID() != otel.SpanID("step", stepID) || l.SpanContext.SpanID() != otel.SpanID("late", stepID) || str(t, l, "agentrt.tool.name") != "block" || str(t, l, "agentrt.tool.summary") != "finished after cancel" || !strings.Contains(str(t, l, "agentrt.tool.observation"), `"late":true`) {
		t.Fatalf("late span %s parent %s %v", l.SpanContext.SpanID(), l.Parent.SpanID(), l.Attributes)
	}
	finished := events[len(events)-1]
	if finished.Type != agentrt.EventStepToolFinished || !l.EndTime.Equal(finished.At) || l.StartTime.After(l.EndTime) || !l.EndTime.After(run.EndTime) {
		t.Fatalf("late span %v-%v, event %v, run ended %v", l.StartTime, l.EndTime, finished.At, run.EndTime)
	}
}

// Limits, loops, and a fail decision: events on the run span with their
// payload fields, and a run status that says why.
func TestExporter_LimitLoopAndFailure(t *testing.T) {
	f := loadFixtures(t)
	all := exportAll(t, f, nil)

	run := named(ofRun(all, f.limit), otel.SpanRun)[0]
	if eventAttr(t, run, agentrt.EventLimitExceeded, "agentrt.limit.reason") != "limit_steps" || str(t, run, "agentrt.run.status") != "FAILED" || str(t, run, "agentrt.run.reason") != "limit_steps" || run.Status.Code != codes.Error || run.Status.Description != "limit_steps" {
		t.Fatalf("limit run %v %v %v", run.Attributes, run.Events, run.Status)
	}
	if _, ok := event(run, agentrt.EventRunStarted); !ok {
		t.Fatalf("no run.started event: %v", run.Events)
	}

	run = named(ofRun(all, f.loop), otel.SpanRun)[0]
	if eventAttr(t, run, agentrt.EventLoopDetected, "agentrt.loop.repeats") != "2" || eventAttr(t, run, agentrt.EventLoopDetected, "agentrt.loop.tool") != "read" || len(eventAttr(t, run, agentrt.EventLoopDetected, "agentrt.loop.args_hash")) != 64 || len(eventAttr(t, run, agentrt.EventLoopDetected, "agentrt.loop.observation_hash")) != 64 || str(t, run, "agentrt.run.reason") != "loop_detected" {
		t.Fatalf("loop run %v %v", run.Attributes, run.Events)
	}

	spans := ofRun(all, f.failed)
	run = named(spans, otel.SpanRun)[0]
	step := named(spans, otel.SpanStep)[0]
	if str(t, run, "agentrt.run.reason") != "goal_failed" || str(t, step, "agentrt.decision.kind") != "fail" || !strings.HasPrefix(str(t, step, "agentrt.decision.message"), "no no") {
		t.Fatalf("failed run %v step %v", run.Attributes, step.Attributes)
	}
}

// Two exporters that between them saw the stream once, split anywhere,
// emit the same spans with the same ids and parents as one exporter that
// saw it all; the second takes the starts it missed from the store.
func TestExporter_DeterministicAcrossRestarts(t *testing.T) {
	f := loadFixtures(t)
	whole := exportAll(t, f, nil)
	var all []agentrt.Event
	if err := (&export.Follower{Store: f.store, Once: true}).Follow(context.Background(), func(e agentrt.Event) error { all = append(all, e); return nil }); err != nil {
		t.Fatal(err)
	}
	type key struct {
		trace  trace.TraceID
		span   trace.SpanID
		parent trace.SpanID
		name   string
	}
	index := func(spans tracetest.SpanStubs) map[key]tracetest.SpanStub {
		m := map[key]tracetest.SpanStub{}
		for _, s := range spans {
			k := key{s.SpanContext.TraceID(), s.SpanContext.SpanID(), s.Parent.SpanID(), s.Name}
			if _, dup := m[k]; dup {
				t.Fatalf("duplicate span %+v", k)
			}
			m[k] = s
		}
		return m
	}
	wantSpans := index(whole)
	for _, cut := range []int{1, len(all) / 3, len(all) / 2, 2 * len(all) / 3, len(all) - 2} {
		mem := tracetest.NewInMemoryExporter()
		tp := otel.NewTracerProvider(sdktrace.WithSyncer(mem))
		first := &otel.Exporter{TracerProvider: tp, Store: f.store}
		for _, e := range all[:cut] {
			first.Handle(e)
		}
		second := &otel.Exporter{TracerProvider: tp, Store: f.store} // a new process: nothing remembered
		for _, e := range all[cut:] {
			second.Handle(e)
		}
		got := index(mem.GetSpans())
		tp.Shutdown(context.Background())
		if len(got) != len(wantSpans) {
			t.Fatalf("cut at %d: %d spans, want %d", cut, len(got), len(wantSpans))
		}
		for k, w := range wantSpans {
			g, ok := got[k]
			if !ok {
				t.Fatalf("cut at %d: missing span %+v", cut, k)
			}
			if _, unknown := attr(g, "agentrt.span.start_unknown"); unknown {
				t.Fatalf("cut at %d: span %+v started without a time though the store has it", cut, k)
			}
			if !g.EndTime.Equal(w.EndTime) {
				t.Fatalf("cut at %d: span %+v ends %v, want %v", cut, k, g.EndTime, w.EndTime)
			}
			if g.Name == otel.SpanModel && !g.StartTime.Equal(w.StartTime) {
				t.Fatalf("cut at %d: model span starts %v, want %v", cut, g.StartTime, w.StartTime)
			}
			if g.StartTime.After(g.EndTime) {
				t.Fatalf("cut at %d: span %+v starts after it ends", cut, k)
			}
		}
	}
	// Without a store, a span whose start was not seen says so.
	mem := tracetest.NewInMemoryExporter()
	tp := otel.NewTracerProvider(sdktrace.WithSyncer(mem))
	x := &otel.Exporter{TracerProvider: tp}
	for _, e := range all {
		if e.Type != agentrt.EventRunCreated && e.Type != agentrt.EventStepStarted && e.Type != agentrt.EventApprovalRequested {
			x.Handle(e)
		}
	}
	unknownStarts := mem.GetSpans()
	tp.Shutdown(context.Background())
	unknown := 0
	for _, s := range unknownStarts {
		if _, ok := attr(s, "agentrt.span.start_unknown"); ok {
			unknown++
			// It starts at the first of its events this exporter saw.
			if s.StartTime.After(s.EndTime) {
				t.Fatalf("%s span with an unknown start spans %v-%v", s.Name, s.StartTime, s.EndTime)
			}
		} else if s.Name != otel.SpanModel && s.Name != otel.SpanLateTool {
			t.Fatalf("%s span %s has a start without having seen one", s.Name, s.SpanContext.SpanID())
		}
	}
	if unknown == 0 {
		t.Fatal("no span reported an unknown start")
	}
}

// Model-chosen text is cut at MaxText with a marker, and never names a
// span.
func TestExporter_TruncatesModelChosenText(t *testing.T) {
	f := loadFixtures(t)
	names := map[string]bool{otel.SpanRun: true, otel.SpanStep: true, otel.SpanModel: true, otel.SpanApproval: true, otel.SpanLateTool: true, otel.SpanEvent: true}
	check := func(spans tracetest.SpanStubs, max int) {
		t.Helper()
		run := named(ofRun(spans, f.completed), otel.SpanRun)[0]
		goal := str(t, run, "agentrt.run.goal")
		if !strings.HasPrefix(goal, strings.Repeat("g", max)) || !strings.HasSuffix(goal, fmt.Sprintf("…[truncated: %d bytes]", longGoal)) || len(goal) > max+40 {
			t.Fatalf("goal at max %d: %d bytes, ends %q", max, len(goal), goal[len(goal)-40:])
		}
		reason := str(t, named(ofRun(spans, f.completed), otel.SpanStep)[0], "agentrt.decision.reason")
		if len(reason) > max+40 || !strings.Contains(reason, "…[truncated: 3200 bytes]") {
			t.Fatalf("reason at max %d: %d bytes", max, len(reason))
		}
		message := str(t, named(ofRun(spans, f.failed), otel.SpanStep)[0], "agentrt.decision.message")
		if len(message) > max+40 {
			t.Fatalf("message at max %d: %d bytes", max, len(message))
		}
		for _, s := range spans {
			if !names[s.Name] {
				t.Fatalf("span named %q", s.Name)
			}
			for _, a := range s.Attributes {
				if !a.Valid() {
					t.Fatalf("%s span carries an empty attribute", s.Name)
				}
			}
			for _, e := range s.Events {
				if !strings.Contains(e.Name, ".") || strings.Contains(e.Name, " ") {
					t.Fatalf("event named %q", e.Name)
				}
				for _, a := range e.Attributes {
					if !a.Valid() {
						t.Fatalf("event %s on %s carries an empty attribute", e.Name, s.Name)
					}
				}
			}
		}
	}
	check(exportAll(t, f, nil), otel.DefaultMaxText)
	check(exportAll(t, f, func(x *otel.Exporter) { x.MaxText = 100 }), 100)
	// Short text is left alone.
	run := named(ofRun(exportAll(t, f, nil), f.paused), otel.SpanRun)[0]
	if str(t, run, "agentrt.run.goal") != "publish with approval" {
		t.Fatalf("goal %q", str(t, run, "agentrt.run.goal"))
	}
}

func TestTruncate_CutsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 20 bytes
	got := otel.Truncate(s, 5)
	if !strings.HasPrefix(got, "éé…[truncated: 20 bytes]") || strings.ContainsRune(got, '�') {
		t.Fatalf("got %q", got)
	}
	if otel.Truncate("abc", 3) != "abc" || otel.Truncate("", 0) != "" {
		t.Fatal("short text changed")
	}
}

func TestIDs_DerivedAndValid(t *testing.T) {
	if otel.TraceID("r") == otel.TraceID("s") || !otel.TraceID("").IsValid() {
		t.Fatal("trace ids")
	}
	if otel.SpanID("step", "a") == otel.SpanID("run", "a") || otel.SpanID("step", "a") == otel.SpanID("step", "b") || !otel.SpanID("", "").IsValid() {
		t.Fatal("span ids")
	}
	if otel.TraceID("run-1").String() != "93afafd5ba988f2a2bebf175c7cde8d6" || otel.SpanID("run", "run-1").String() != "d925db6dbbd9dfb1" {
		t.Fatalf("derivation changed: %s %s", otel.TraceID("run-1"), otel.SpanID("run", "run-1"))
	}
}

// A payload the follower capped is still mapped, and the span says the
// payload was cut.
func TestExporter_RecordsACappedPayload(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := otel.NewTracerProvider(sdktrace.WithSyncer(mem))
	defer tp.Shutdown(context.Background())
	x := &otel.Exporter{TracerProvider: tp}
	now := time.Now()
	x.Handle(agentrt.Event{Seq: 1, RunID: "r", At: now, Type: agentrt.EventRunCreated, Payload: []byte(`{"truncated":true,"length":99999,"head":"{\"goal\":\"..."}`)})
	x.Handle(agentrt.Event{Seq: 2, RunID: "r", At: now.Add(time.Second), Type: agentrt.EventRunFinished, Payload: []byte(`{"status":"COMPLETED","reason":"goal_completed","detail":"","steps":1}`)})
	x.Handle(agentrt.Event{Seq: 3, RunID: "r", At: now.Add(time.Second), Type: "future.event", Payload: []byte(`not json`)})
	spans := mem.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	if str(t, spans[0], "agentrt.payload.truncated") != "99999" || str(t, spans[0], "agentrt.run.status") != "COMPLETED" {
		t.Fatalf("attributes %v", spans[0].Attributes)
	}
	noAttr(t, spans[0], "agentrt.run.goal")
	// An unknown event after the run ended is a zero-length span under the run.
	if spans[1].Name != otel.SpanEvent || str(t, spans[1], "agentrt.event.type") != "future.event" || str(t, spans[1], "agentrt.event.seq") != "3" || spans[1].Parent.SpanID() != otel.SpanID("run", "r") || !spans[1].StartTime.Equal(spans[1].EndTime) {
		t.Fatalf("span %s %v", spans[1].Name, spans[1].Attributes)
	}
}

// Provider reads the standard environment: none and console build, the
// deterministic ids are installed, and a protocol this module does not
// carry is refused by name.
func TestProvider_FromEnvironment(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_SERVICE_NAME", "svc")
	ctx := context.Background()
	mem := tracetest.NewInMemoryExporter()
	tp, err := otel.Provider(ctx, sdktrace.WithSyncer(mem))
	if err != nil {
		t.Fatal(err)
	}
	x := &otel.Exporter{TracerProvider: tp}
	now := time.Now()
	x.Handle(agentrt.Event{Seq: 1, RunID: "run-1", At: now, Type: agentrt.EventRunCreated, Payload: []byte(`{"goal":"g","limits":{"max_steps":1,"max_consecutive_tool_failures":1,"loop_threshold":1,"max_model_calls":4,"approval_ttl":60000000000}}`)})
	x.Handle(agentrt.Event{Seq: 2, RunID: "run-1", At: now, Type: agentrt.EventRunFinished, Payload: []byte(`{"status":"FAILED","reason":"limit_steps","detail":"d","steps":1}`)})
	spans := mem.GetSpans()
	tp.Shutdown(ctx)
	if len(spans) != 1 || spans[0].SpanContext.TraceID() != otel.TraceID("run-1") || spans[0].SpanContext.SpanID() != otel.SpanID("run", "run-1") {
		t.Fatalf("spans %v", spans)
	}
	if !strings.Contains(fmt.Sprint(spans[0].Resource.Attributes()), "svc") {
		t.Fatalf("resource %v", spans[0].Resource.Attributes())
	}
	if str(t, spans[0], "agentrt.limits.max_model_calls") != "4" || str(t, spans[0], "agentrt.limits.approval_ttl_ms") != "60000" {
		t.Fatalf("limits %v", spans[0].Attributes)
	}
	noAttr(t, spans[0], "agentrt.limits.grant_ttl_ms")

	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	tp, err = otel.Provider(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tp.Shutdown(ctx)

	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	if _, err := otel.Provider(ctx); err == nil || !strings.Contains(err.Error(), "http/json") {
		t.Fatalf("http/json accepted: %v", err)
	}
	for _, proto := range []string{"http/protobuf", "grpc", ""} {
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", proto)
		tp, err = otel.Provider(ctx)
		if err != nil {
			t.Fatalf("%q: %v", proto, err)
		}
		// Nothing is sent before Shutdown; a short deadline bounds the flush.
		sctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		tp.Shutdown(sctx)
		cancel()
	}

	t.Setenv("OTEL_TRACES_EXPORTER", "zipkin")
	if _, err := otel.Provider(ctx); err == nil {
		t.Fatal("unknown exporter accepted")
	}
	// Without a service name the resource says agentrt.
	os.Unsetenv("OTEL_SERVICE_NAME")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	tp, err = otel.Provider(ctx, sdktrace.WithSyncer(mem))
	if err != nil {
		t.Fatal(err)
	}
	mem.Reset()
	x = &otel.Exporter{TracerProvider: tp}
	x.Handle(agentrt.Event{Seq: 1, RunID: "run-2", At: now, Type: agentrt.EventRunCreated, Payload: []byte(`{}`)})
	x.Handle(agentrt.Event{Seq: 2, RunID: "run-2", At: now, Type: agentrt.EventRunFinished, Payload: []byte(`{}`)})
	spans = mem.GetSpans()
	tp.Shutdown(ctx)
	if len(spans) != 1 || !strings.Contains(fmt.Sprint(spans[0].Resource.Attributes()), "agentrt") {
		t.Fatalf("resource %v", spans)
	}
}
