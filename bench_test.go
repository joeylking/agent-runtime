package agentrt_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

// benchAgent calls read with a fresh argument until the run has n-1 steps,
// then completes, so a run is n steps with no loop or failure in it.
type benchAgent struct{ n int }

func (a benchAgent) Decide(_ context.Context, in agentrt.StepInput) (agentrt.Decision, error) {
	if len(in.Steps) >= a.n-1 {
		return scripted.Complete(`{}`), nil
	}
	return scripted.ToolCall("read", fmt.Sprintf(`{"n":%d}`, len(in.Steps)), ""), nil
}

// padTool answers with its arguments, padded to about size bytes. It
// keeps no record of its calls, so a long benchmark does not grow it.
type padTool int

func (padTool) Spec() agentrt.ToolSpec {
	return agentrt.ToolSpec{Name: "read", Description: "read", InputSchema: []byte(numberSchema), SideEffect: agentrt.ReadOnly, Timeout: 2 * time.Second}
}

func (p padTool) Call(_ context.Context, c agentrt.ToolCall) (agentrt.ToolResult, error) {
	if p == 0 {
		return agentrt.ToolResult{Content: c.Args, Summary: "echoed"}, nil
	}
	body := strings.TrimSuffix(string(c.Args), "}")
	pad := int(p) - len(body) - len(`,"pad":""}`)
	return agentrt.ToolResult{Content: []byte(body + `,"pad":"` + strings.Repeat("x", max(pad, 0)) + `"}`), Summary: "echoed"}, nil
}

// BenchmarkDriver runs whole runs of a scripted agent against a file-backed
// store, which is what a consumer's run costs the runtime.
func BenchmarkDriver(b *testing.B) {
	for _, steps := range []int{10, 100, 400} {
		for _, obs := range []struct {
			name string
			size int
		}{{"small", 0}, {"8KiB", 8 << 10}} {
			b.Run(fmt.Sprintf("steps=%d/obs=%s", steps, obs.name), func(b *testing.B) {
				store, err := agentrt.OpenStore(filepath.Join(b.TempDir(), "bench.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer store.Close()
				d, err := agentrt.NewDriver(agentrt.Config{Store: store, Agent: benchAgent{n: steps}, Policy: agentrt.DefaultPolicy(), Tools: []agentrt.Tool{padTool(obs.size)}})
				if err != nil {
					b.Fatal(err)
				}
				l := agentrt.Limits{MaxSteps: steps, MaxConsecutiveToolFailures: 3, LoopThreshold: steps + 1, MaxActiveTime: time.Hour}
				ctx := context.Background()
				b.ReportAllocs()
				for b.Loop() {
					run, err := d.Start(ctx, "bench", l)
					if err != nil {
						b.Fatal(err)
					}
					if run.Status != agentrt.StatusCompleted || run.StepCount != steps {
						b.Fatalf("run %s after %d steps: %s", run.Status, run.StepCount, run.ReasonDetail)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*steps), "ns/step")
			})
		}
	}
}
