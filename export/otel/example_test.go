package otel_test

import (
	"context"
	"fmt"
	"sort"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/export"
	"github.com/joeylking/agent-runtime/export/otel"
	"github.com/joeylking/agent-runtime/scripted"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A follower feeds an Exporter, which emits one span per run and one per
// step, all in the trace derived from the run id. The in-memory exporter
// stands in for the collector; Provider builds the real one from the
// environment. The run id and step ids are fixed here so the derived ids
// print the same every time.
func ExampleExporter() {
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()
	read := tool{spec: agentrt.ToolSpec{Name: "read", Description: "read", InputSchema: []byte(`{"type":"object"}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		fn: func(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
			return agentrt.ToolResult{Content: c.Args, Summary: "read it"}, nil
		}}
	n := 0
	driver, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look first"),
		scripted.Complete(`{"ok":true}`),
	}}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{read},
		NewID: func() string { n++; return fmt.Sprintf("step-%d", n) }})
	if err != nil {
		panic(err)
	}
	if _, err := driver.StartWithID(context.Background(), "run-1", "read once", agentrt.DefaultLimits()); err != nil {
		panic(err)
	}

	mem := tracetest.NewInMemoryExporter()
	tp := otel.NewTracerProvider(sdktrace.WithSyncer(mem))
	defer tp.Shutdown(context.Background())
	exporter := &otel.Exporter{TracerProvider: tp, Store: store}
	if err := (&export.Follower{Store: store, Once: true}).Follow(context.Background(), exporter.Handle); err != nil {
		panic(err)
	}

	spans := mem.GetSpans()
	sort.Slice(spans, func(i, j int) bool {
		return spans[i].StartTime.Before(spans[j].StartTime) || spans[i].StartTime.Equal(spans[j].StartTime) && spans[i].Name < spans[j].Name
	})
	for _, s := range spans {
		fmt.Printf("%s trace=%s span=%s parent=%s", s.Name, s.SpanContext.TraceID(), s.SpanContext.SpanID(), s.Parent.SpanID())
		for _, a := range s.Attributes {
			switch a.Key {
			case "agentrt.run.id", "agentrt.step.index", "agentrt.decision.tool", "agentrt.tool.summary", "agentrt.run.status", "agentrt.step.status":
				fmt.Printf(" %s=%s", a.Key, a.Value.String())
			}
		}
		fmt.Println()
	}
	// Output:
	// agentrt.run trace=93afafd5ba988f2a2bebf175c7cde8d6 span=d925db6dbbd9dfb1 parent=0000000000000000 agentrt.run.id=run-1 agentrt.run.status=COMPLETED
	// agentrt.step trace=93afafd5ba988f2a2bebf175c7cde8d6 span=a814125d85e59b99 parent=d925db6dbbd9dfb1 agentrt.run.id=run-1 agentrt.step.index=0 agentrt.decision.tool=read agentrt.tool.summary=read it agentrt.step.status=done
	// agentrt.step trace=93afafd5ba988f2a2bebf175c7cde8d6 span=02240d837b60d1a6 parent=d925db6dbbd9dfb1 agentrt.run.id=run-1 agentrt.step.index=1
}
