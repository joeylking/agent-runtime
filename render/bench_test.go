package render_test

import (
	"fmt"
	"strings"
	"testing"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/render"
)

// benchSteps is a run of n tool calls whose results are about size bytes.
func benchSteps(n, size int) []agentrt.Step {
	steps := make([]agentrt.Step, n)
	for i := range steps {
		content := fmt.Sprintf(`{"n":%d}`, i)
		if size > 0 {
			content = fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("x", size-20))
		}
		steps[i] = toolStep(i, "read", fmt.Sprintf(`{"n":%d}`, i), "look", result(content, "echoed"))
	}
	return steps
}

var benchSizes = []struct {
	name string
	size int
}{{"small", 0}, {"8KiB", 8 << 10}}

// BenchmarkMessages renders the last decision of a run.
func BenchmarkMessages(b *testing.B) {
	opts := render.Options{Opening: fixedOpening("Goal: g")}
	for _, n := range []int{10, 100, 400} {
		for _, sz := range benchSizes {
			in := input(benchSteps(n, sz.size)...)
			b.Run(fmt.Sprintf("steps=%d/obs=%s", n, sz.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					render.Messages(in, opts)
				}
			})
		}
	}
}

// BenchmarkMessagesWholeRun renders every decision of a run, as a model
// agent does once per step.
func BenchmarkMessagesWholeRun(b *testing.B) {
	opts := render.Options{Opening: fixedOpening("Goal: g")}
	for _, n := range []int{10, 100, 400} {
		for _, sz := range benchSizes {
			steps := benchSteps(n, sz.size)
			b.Run(fmt.Sprintf("steps=%d/obs=%s", n, sz.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for k := 0; k <= n; k++ {
						render.Messages(input(steps[:k]...), opts)
					}
				}
			})
		}
	}
}
