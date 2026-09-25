package view_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/view"
)

// PendingApproval is the rule for choosing the approval an operator means:
// without an id the run must have exactly one pending approval, and a named
// approval must still be pending. Guessing is not the runtime's decision.
func ExamplePendingApproval() {
	ctx := context.Background()
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()

	push := tool{spec: agentrt.ToolSpec{
		Name: "push", Description: "push", SideEffect: agentrt.RemoteMutation, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`),
	}}
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":7}`, "publish")}}
	d, err := agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(),
		Tools: []agentrt.Tool{push},
		NewID: func() func() string {
			n := 0
			return func() string { n++; return fmt.Sprintf("id-%d", n) }
		}(),
	})
	if err != nil {
		panic(err)
	}
	run, err := d.Start(ctx, "publish the counter", agentrt.DefaultLimits())
	if err != nil {
		panic(err)
	}

	// One pending approval, so the operator need not name it.
	a, err := view.PendingApproval(ctx, store, run.ID, "")
	fmt.Println(a.ID, a.Kind, a.Status, err)

	if err := d.Approve(ctx, run.ID, a.ID, "joey", ""); err != nil {
		panic(err)
	}

	// Once decided it is no longer a candidate, by id or without one.
	_, err = view.PendingApproval(ctx, store, run.ID, a.ID)
	fmt.Println(errors.Is(err, view.ErrNotPending))
	_, err = view.PendingApproval(ctx, store, run.ID, "")
	fmt.Println(errors.Is(err, view.ErrNoPendingApproval))
	// Output:
	// id-3 remote_mutation pending <nil>
	// true
	// true
}
