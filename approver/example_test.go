package approver_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/approver"
	"github.com/joeylking/agent-runtime/scripted"
)

// A channel shows a run's pending approval and decides it bound to the
// hash it showed; a hash that is not the stored one decides nothing, and
// a decision without an identity is refused before the runtime sees it.
func ExampleLocal() {
	ctx := context.Background()
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()
	push := tool("push", agentrt.RemoteMutation)
	agent := &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":7}`, "publish"), scripted.Complete(`{}`)}}
	n := 0
	d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{push},
		NewID: func() string { n++; return fmt.Sprintf("id-%d", n) }, Now: func() time.Time { return time.Unix(1700000000, 0) }})
	if err != nil {
		panic(err)
	}
	run, err := d.Start(ctx, "publish the counter", agentrt.DefaultLimits())
	if err != nil {
		panic(err)
	}

	ap := approver.New(store, nil)
	pending, err := ap.Pending(ctx, run.ID, 10)
	if err != nil {
		panic(err)
	}
	a := pending[0]
	fmt.Println(a.ID, a.Kind, a.Request.Spec.Name, string(a.Request.Args), len(a.Hash), a.Expired)

	err = ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: "not what was shown", By: "chat:joey"})
	fmt.Println(errors.Is(err, agentrt.ErrApprovalChanged))
	err = ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash})
	fmt.Println(errors.Is(err, approver.ErrDecision))
	err = ap.Approve(ctx, approver.Decision{RunID: run.ID, ApprovalID: a.ID, Hash: a.Hash, By: "chat:joey", Note: "ship it"})
	fmt.Println(err)

	// The grant waits for the consumer's own process to resume the run.
	got, err := d.Resume(ctx, run.ID)
	fmt.Println(got.Status, len(push.calls), err)
	// Output:
	// id-3 remote_mutation push {"n":7} 64 false
	// true
	// true
	// <nil>
	// COMPLETED 1 <nil>
}
