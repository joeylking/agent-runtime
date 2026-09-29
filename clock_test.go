package agentrt_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// countingClock hands out one second per reading and counts them.
type countingClock struct {
	at    time.Time
	reads int
}

func (c *countingClock) Now() time.Time { c.reads++; c.at = c.at.Add(time.Second); return c.at }

// A consumer with a deterministic clock keys its own records on the
// sequence of the runtime's clock readings, so every path takes exactly
// the readings it always has, one per recorded state change. The counts
// were taken from the runtime before the writes of a step and its run
// were merged into one transaction.
func TestDriver_ClockReadingsAreStable(t *testing.T) {
	wipe := newTool("wipe", agentrt.Destructive, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{}`)}, nil
	})
	fin := newTool("finish", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{Content: []byte(`{"done":true}`)}, nil
	})
	fin.spec.Terminal = true
	abort := newTool("abort", agentrt.LocalMutation, emptySchema, func(context.Context, agentrt.ToolCall) (agentrt.ToolResult, error) {
		return agentrt.ToolResult{}, agentrt.ErrAbortRun{Detail: "no"}
	})
	cases := map[string][]agentrt.Decision{
		"read, deny, invalid, complete": {scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("wipe", `{}`, ""), scripted.ToolCall("nope", `{}`, ""), scripted.Complete(`{}`)},
		"terminal tool":                 {scripted.ToolCall("finish", `{}`, "")},
		"tool abort":                    {scripted.ToolCall("abort", `{}`, "")},
		"fail":                          {scripted.Fail("gave up")},
		"agent error":                   {},
		"pause, approve, resume":        {scripted.ToolCall("push", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":2}`, ""), scripted.Complete(`{}`)},
		"loop":                          {scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, ""), scripted.ToolCall("read", `{"n":1}`, "")},
	}
	want := map[string]string{
		"agent error":                   "4",
		"fail":                          "5",
		"loop":                          "11",
		"pause, approve, resume":        "5 16",
		"read, deny, invalid, complete": "16",
		"terminal tool":                 "6",
		"tool abort":                    "6",
	}
	for name, decisions := range cases {
		h := newHarness(t, ":memory:")
		c := &countingClock{at: time.Unix(1700000000, 0)}
		d, err := agentrt.NewDriver(agentrt.Config{Store: h.store, Agent: &scripted.Agent{Decisions: decisions}, Policy: agentrt.DefaultPolicy(),
			Tools: []agentrt.Tool{echoTool("read", agentrt.ReadOnly), echoTool("push", agentrt.RemoteMutation), wipe, fin, abort}, Now: c.Now})
		if err != nil {
			t.Fatal(err)
		}
		l := limits(10, 5)
		l.LoopThreshold = 2
		run, _ := d.Start(context.Background(), "g", l)
		trail := fmt.Sprint(c.reads)
		if run.Status == agentrt.StatusWaitingForApproval {
			approvals, _ := h.store.ListApprovals(context.Background(), run.ID)
			agentrt.Approve(context.Background(), h.store, nil, run.ID, approvals[0].ID, "joey", "")
			d.Resume(context.Background(), run.ID)
			trail += fmt.Sprint(" ", c.reads)
		}
		if trail != want[name] {
			t.Errorf("%s: %s clock readings, want %s", name, trail, want[name])
		}
	}
}
