package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/export"
	"github.com/joeylking/agent-runtime/scripted"
)

// A follower delivers a run's events in commit order to a sink; here the
// JSON Lines sink, into a buffer, and Once returns when the committed
// events are delivered. An operator's process would open the database
// with agentrt.OpenExisting(path, true) and leave Once unset.
func ExampleFollower() {
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		panic(err)
	}
	defer store.Close()
	echo := tool{spec: agentrt.ToolSpec{Name: "echo", Description: "echo", InputSchema: []byte(`{"type":"object"}`), SideEffect: agentrt.ReadOnly, Timeout: time.Second},
		fn: func(c agentrt.ToolCall) (agentrt.ToolResult, error) {
			return agentrt.ToolResult{Content: c.Args, Summary: "echoed"}, nil
		}}
	driver, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: []agentrt.Decision{
		scripted.ToolCall("echo", `{"n":1}`, "look"),
		scripted.Complete(`{"ok":true}`),
	}}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{echo}})
	if err != nil {
		panic(err)
	}
	if _, err := driver.Start(context.Background(), "echo once", agentrt.DefaultLimits()); err != nil {
		panic(err)
	}

	var out bytes.Buffer
	follower := &export.Follower{Store: store, Cursor: &export.MemCursor{}, Once: true}
	if err := follower.Follow(context.Background(), export.JSONL(&out)); err != nil {
		panic(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var rec struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			panic(err)
		}
		fmt.Println(rec.Seq, rec.Type)
	}
	// Output:
	// 1 run.created
	// 2 run.started
	// 3 step.started
	// 4 step.decided
	// 5 step.policy
	// 6 step.tool_started
	// 7 step.tool_finished
	// 8 step.started
	// 9 step.decided
	// 10 run.finished
}
