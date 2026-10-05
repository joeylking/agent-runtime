package agentrt_test

import (
	"context"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// exampleTool answers with the arguments it was given. The examples are about
// the controls around a call, not about what a tool does.
type exampleTool struct{ spec agentrt.ToolSpec }

func (t exampleTool) Spec() agentrt.ToolSpec { return t.spec }

func (t exampleTool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	return agentrt.ToolResult{Content: c.Args, Summary: "echoed " + t.spec.Name}, nil
}

func newExampleTool(name string, se agentrt.SideEffect) exampleTool {
	return exampleTool{spec: agentrt.ToolSpec{
		Name: name, Description: name, SideEffect: se, Timeout: 2 * time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}`),
	}}
}

// exampleIDs numbers ids in the order the driver asks for them, so an
// example's output does not change from run to run.
func exampleIDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// DefaultPolicy denies a destructive tool, and the denial is an observation
// the agent must work around rather than an error: the run continues until
// the consecutive-failure limit ends it.
func ExampleDefaultPolicy() {
	ctx := context.Background()
	store := must(agentrt.OpenStore(":memory:"))
	defer store.Close()

	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("wipe", `{"n":1}`, "clear the workspace"),
	}}
	d := must(agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(),
		Tools: []agentrt.Tool{newExampleTool("wipe", agentrt.Destructive)},
		NewID: exampleIDs(),
	}))
	run := must(d.Start(ctx, "clear the workspace", agentrt.Limits{
		MaxSteps: 5, MaxConsecutiveToolFailures: 1, LoopThreshold: 3,
	}))

	steps := must(store.ListSteps(ctx, run.ID))
	fmt.Println(run.Status, run.Reason)
	fmt.Println(steps[0].Policy.Outcome, steps[0].Policy.Reason)
	fmt.Println(steps[0].Observation.Kind)
	// Output:
	// FAILED repeated_tool_failures
	// deny side effect destructive is deny by policy
	// policy_denied
}

// A remote mutation pauses the run on an approval bound by hash to the exact
// request. Approve records the decision; Resume executes that request and
// nothing else, then continues the loop.
func ExampleDriver_Resume() {
	ctx := context.Background()
	store := must(agentrt.OpenStore(":memory:"))
	defer store.Close()

	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("push", `{"n":7}`, "publish the counter"),
		scripted.Complete(`{"pushed":7}`),
	}}
	d := must(agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: agentrt.DefaultPolicy(),
		Tools: []agentrt.Tool{newExampleTool("push", agentrt.RemoteMutation)},
		NewID: exampleIDs(),
	}))
	run := must(d.Start(ctx, "publish the counter", agentrt.DefaultLimits()))
	fmt.Println(run.Status)

	a := must(store.ListApprovals(ctx, run.ID))[0]
	fmt.Println(a.ID, a.Kind, a.Status, string(a.Presentation))

	if err := d.Approve(ctx, run.ID, a.ID, "joey", "looks right"); err != nil {
		panic(err)
	}
	run = must(d.Resume(ctx, run.ID))
	fmt.Println(run.Status, run.Reason, string(run.Result))
	// Output:
	// WAITING_FOR_APPROVAL
	// id-3 remote_mutation pending {"args":{"n":7},"tool":"push"}
	// COMPLETED goal_completed {"pushed":7}
}

// release is what this policy shows an operator. It is the presentation, so it
// enters the approval's hash and a changed presentation cannot inherit an old
// approval.
type release struct {
	Repo  string `json:"repo"`
	Files int    `json:"files"`
}

// NeedApproval builds the require_approval decision a policy returns, and
// DecodePresentation reads back what the operator was shown.
func ExampleNeedApproval() {
	ctx := context.Background()
	store := must(agentrt.OpenStore(":memory:"))
	defer store.Close()

	policy := agentrt.PolicyFunc(func(_ context.Context, req agentrt.ToolRequest, _ agentrt.RunView) (agentrt.PolicyDecision, error) {
		if req.Spec.SideEffect == agentrt.ReadOnly {
			return agentrt.PolicyDecision{Outcome: agentrt.Allow, Reason: "reads are allowed"}, nil
		}
		return agentrt.NeedApproval("release", "publishing is an operator's decision",
			map[string]string{"tool": req.Spec.Name},
			release{Repo: "joeylking/repo-steward", Files: 3})
	})
	agent := &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("push", `{"n":7}`, "publish the counter"),
	}}
	d := must(agentrt.NewDriver(agentrt.Config{
		Store: store, Agent: agent, Policy: policy,
		Tools: []agentrt.Tool{newExampleTool("push", agentrt.RemoteMutation)},
		NewID: exampleIDs(),
	}))
	run := must(d.Start(ctx, "publish the counter", agentrt.DefaultLimits()))

	a := must(store.ListApprovals(ctx, run.ID))[0]
	shown := must(agentrt.DecodePresentation[release](a))
	fmt.Println(run.Status, a.Kind, string(a.Capability))
	fmt.Println(shown.Repo, shown.Files)
	// Output:
	// WAITING_FOR_APPROVAL release {"tool":"push"}
	// joeylking/repo-steward 3
}

// A loop the runtime does not own proposes each action to a Gate, which
// records it, checks it, and runs what policy allows. A remote mutation
// pauses the run on an approval; once it is approved, Attach hands back
// exactly the approved request to execute, and the loop goes on.
func ExampleGate() {
	ctx := context.Background()
	store := must(agentrt.OpenStore(":memory:"))
	defer store.Close()

	g := must(agentrt.NewGate(agentrt.GateConfig{
		Store: store, Policy: agentrt.DefaultPolicy(),
		Tools: []agentrt.Tool{newExampleTool("read", agentrt.ReadOnly), newExampleTool("push", agentrt.RemoteMutation)},
		NewID: exampleIDs(),
	}))
	// The external loop's decisions, by step index.
	plan := []agentrt.Decision{
		scripted.ToolCall("read", `{"n":1}`, "look first"),
		scripted.ToolCall("push", `{"n":7}`, "publish the counter"),
		scripted.Complete(`{"pushed":7}`),
	}

	s := must(g.Begin(ctx, "run-1", "publish the counter", agentrt.DefaultLimits()))
	var v agentrt.Verdict
	for v.Outcome != agentrt.VerdictPending {
		step := must(s.Step(ctx))
		v = must(step.Propose(ctx, plan[step.Index()]))
		fmt.Println(step.Index(), v.Outcome)
		if v.Outcome == agentrt.VerdictAllowed {
			fmt.Println(must(step.Execute(ctx)).Summary)
		}
	}
	fmt.Println(v.Run.Status, v.Approval.Kind)

	if err := agentrt.Approve(ctx, store, nil, "run-1", v.Approval.ID, "joey", "looks right"); err != nil {
		panic(err)
	}
	s, v, err := g.Attach(ctx, "run-1")
	if err != nil {
		panic(err)
	}
	defer s.Close()
	fmt.Println(v.Outcome, v.Request.Spec.Name, string(v.Request.Args))
	fmt.Println(must(s.Current().Execute(ctx)).Summary)

	step := must(s.Step(ctx))
	v = must(step.Propose(ctx, plan[step.Index()]))
	fmt.Println(step.Index(), v.Outcome, v.Run.Status, string(v.Run.Result))
	// Output:
	// 0 allowed
	// echoed read
	// 1 pending
	// WAITING_FOR_APPROVAL remote_mutation
	// allowed push {"n":7}
	// echoed push
	// 2 ended COMPLETED {"pushed":7}
}
