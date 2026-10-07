package agentrt_test

import (
	"context"
	"fmt"
	"os"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/trace"
)

// versioned names a SideEffectPolicy, which has no identity of its own, by
// the version of the configuration it was built from.
type versioned struct {
	agentrt.SideEffectPolicy
	id string
}

func (p versioned) PolicyID() string { return p.id }

// Recheck hands each decision a finished run recorded to another policy,
// rebuilt as the first policy saw it, and reports which it would have
// decided otherwise.
func ExampleRecheck() {
	ctx := context.Background()
	store := must(agentrt.OpenStore(":memory:"))
	defer store.Close()

	lenient := versioned{agentrt.DefaultPolicy(), "policy v1"}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, ""),
		scripted.ToolCall("write", `{"n":2}`, ""),
		scripted.Complete(`{}`),
	}}
	d := must(agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: lenient, NewID: exampleIDs(),
		Tools: []agentrt.Tool{newExampleTool("read", agentrt.ReadOnly), newExampleTool("write", agentrt.LocalMutation)},
	}))
	run := must(d.Start(ctx, "read, then write", agentrt.DefaultLimits()))

	stricter := versioned{agentrt.SideEffectPolicy{agentrt.ReadOnly: agentrt.Allow}, "policy v2"}
	rep := must(agentrt.Recheck(ctx, store, run.ID, stricter, agentrt.RecheckOptions{}))
	fmt.Println(rep.Same())
	if err := trace.WriteRecheck(os.Stdout, rep); err != nil {
		panic(err)
	}
	// Output:
	// false
	// run id-1 COMPLETED: 1 of 2 re-checked evaluations differ; policy changed: recorded "policy v1", given "policy v2"; recorded specs
	// DIFFERENT step 1 seq 10 write: allow -> deny: no policy for side effect "local_mutation"
	// same      step 0 seq 5 read: allow
}
