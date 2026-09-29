package view_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
	"github.com/joeylking/agent-runtime/view"
)

// benchStore holds runs runs, every other one waiting for an approval and
// the rest completed.
func benchStore(b *testing.B, runs int) *agentrt.Store {
	b.Helper()
	store, err := agentrt.OpenStore(filepath.Join(b.TempDir(), "runs.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { store.Close() })
	tools := []agentrt.Tool{newTool("push", agentrt.RemoteMutation)}
	pause, _ := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.ToolCall("push", `{"n":7}`, "")}}, Policy: agentrt.DefaultPolicy(), Tools: tools})
	done, _ := agentrt.NewDriver(agentrt.Config{Store: store, Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.Complete(`{}`)}}, Policy: agentrt.DefaultPolicy(), Tools: tools})
	l := agentrt.Limits{MaxSteps: 5, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}
	for i := range runs {
		d := done
		if i%2 == 0 {
			d = pause
		}
		if _, err := d.Start(context.Background(), fmt.Sprintf("goal %d", i), l); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

func BenchmarkRuns(b *testing.B) {
	store := benchStore(b, 1000)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		rows, err := view.Runs(ctx, store)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 1000 || rows[0].PendingApprovalID == "" && rows[1].PendingApprovalID == "" {
			b.Fatalf("rows = %d", len(rows))
		}
	}
}
