package testkit_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/testkit"
)

// exampleTool records its calls; the examples are about what the runtime
// does around a call, not what the tool does.
type exampleTool struct {
	spec  agentrt.ToolSpec
	calls int
}

func (t *exampleTool) Spec() agentrt.ToolSpec { return t.spec }
func (t *exampleTool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	t.calls++
	return agentrt.ToolResult{Content: c.Args, Summary: "sent"}, nil
}

func newExampleTool(name string, se agentrt.SideEffect) *exampleTool {
	return &exampleTool{spec: agentrt.ToolSpec{Name: name, Description: name, SideEffect: se, Timeout: time.Second,
		InputSchema: []byte(`{"type":"object","properties":{"to":{"type":"string","minLength":1}},"required":["to"],"additionalProperties":false}`)}}
}

// The examples run the kit under a testing.TB they create, as a test
// would pass its own; nothing here is expected to fail.
func exampleTB() testing.TB {
	return &exampleT{}
}

type exampleT struct{ testing.TB }

func mustTempDir() string {
	dir, err := os.MkdirTemp("", "testkit-example")
	if err != nil {
		panic(err)
	}
	return dir
}

func (exampleT) Helper()                           {}
func (exampleT) TempDir() string                   { return mustTempDir() }
func (exampleT) Cleanup(func())                    {}
func (exampleT) Errorf(format string, args ...any) { fmt.Printf("FAIL: "+format+"\n", args...) }
func (exampleT) Fatalf(format string, args ...any) { panic(fmt.Sprintf(format, args...)) }
func (exampleT) Logf(string, ...any)               {}
func (exampleT) Fatal(args ...any)                 { panic(fmt.Sprint(args...)) }
func (exampleT) Error(args ...any)                 { fmt.Println("FAIL:", fmt.Sprint(args...)) }
func (exampleT) Name() string                      { return "example" }
func (exampleT) FailNow()                          { panic("FailNow") }
func (exampleT) Failed() bool                      { return false }
func (exampleT) Fail()                             {}
func (exampleT) Log(...any)                        {}
func (exampleT) Skip(...any)                       {}
func (exampleT) SkipNow()                          {}
func (exampleT) Skipf(string, ...any)              {}
func (exampleT) Skipped() bool                     { return false }
func (exampleT) Setenv(string, string)             {}
func (exampleT) Chdir(string)                      {}
func (exampleT) Context() context.Context          { return context.Background() }
func (exampleT) Attr(string, string)               {}
func (exampleT) Output() io.Writer                 { return io.Discard }

// A side effect interrupted after it took effect and before the runtime
// recorded it is not run again by the runtime: the run waits for an
// operator, and approving runs the same step once more under that
// approval. The result carries every call the tool received.
func ExampleRun() {
	send := newExampleTool("send", agentrt.RemoteMutation)
	ids := 0
	res := testkit.Run(exampleTB(), testkit.Scenario{
		Tools:  []agentrt.Tool{send},
		Policy: agentrt.SideEffectPolicy{agentrt.RemoteMutation: agentrt.Allow},
		Agent:  &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("send", `{"to":"ops"}`, ""), scripted.Complete(`{}`)}},
		NewID:  func() string { ids++; return fmt.Sprintf("id-%d", ids) },
	}, testkit.Crash{Step: 0, At: testkit.AfterToolEffect})

	fmt.Println(res.Run.Status, "after", res.Resumes, "resumes")
	for _, c := range res.Calls {
		fmt.Println("call step", c.Step, c.Tool, string(c.Args), "returned:", c.Returned)
	}
	fmt.Println("approval", res.Approvals[0].Kind, res.Approvals[0].Status)
	// Output:
	// COMPLETED after 2 resumes
	// call step 0 send {"to":"ops"} returned: false
	// call step 0 send {"to":"ops"} returned: true
	// approval interrupted_side_effect approved
}

// A conformance table is decided through the runtime first, so a case the
// schema refuses is Refused before any policy sees it, and a disagreement
// prints as one table.
func ExampleCheckPolicy() {
	tools := []agentrt.Tool{newExampleTool("send", agentrt.RemoteMutation), newExampleTool("wipe", agentrt.Destructive)}
	testkit.CheckPolicy(exampleTB(), agentrt.DefaultPolicy(), tools, []testkit.PolicyCase{
		{Name: "send asks", Tool: "send", Args: `{"to":"ops"}`, Want: agentrt.RequireApproval, Kind: "remote_mutation"},
		{Name: "wipe denied", Tool: "wipe", Args: `{"to":"all"}`, Want: agentrt.Deny},
		{Name: "no recipient", Tool: "send", Args: `{}`, Want: testkit.Refused},
		{Name: "wrong expectation", Tool: "wipe", Args: `{"to":"all"}`, Want: agentrt.Allow},
	})
	// Output:
	// FAIL: testkit: 1 of 4 policy cases disagree:
	// case               request            want   got   detail
	// wrong expectation  wipe {"to":"all"}  allow  deny  side effect destructive is deny by policy
}

// Valid arguments come from the schema with a fixed seed; the near misses
// each break it in one way, after the cases the runtime refuses before
// any schema.
func ExampleFuzz() {
	f := testkit.Fuzz{Seed: 3, Count: 3, MaxLength: 6}
	schema := []byte(`{"type":"object","properties":{"to":{"type":"string","minLength":1,"maxLength":8},"urgent":{"type":"boolean"}},"required":["to"],"additionalProperties":false}`)
	valid, _ := f.Valid(schema)
	for _, v := range valid {
		fmt.Println("valid  ", string(v))
	}
	invalid, _ := f.Invalid(schema)
	fmt.Println("invalid", len(invalid), "cases; the first:", string(invalid[0][:12]), "...")
	// Output:
	// valid   {"to":"YSe4ß","urgent":true}
	// valid   {"to":"Z\"A1a","urgent":false}
	// valid   {"to":"Tzfr"}
	// invalid 6 cases; the first: {"_":[[[[[[[ ...
}
