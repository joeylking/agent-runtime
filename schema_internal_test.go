package agentrt

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// CheckArgs compiles a spec once however often it is called with it, and
// its cache stays bounded.
func TestCheckArgs_CompilesOnceForASpec(t *testing.T) {
	n := 0
	compiles = func() { n++ }
	t.Cleanup(func() { compiles = func() {} })
	spec := ToolSpec{Name: "once", SideEffect: ReadOnly, Timeout: time.Second, InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)}
	for i := range 1000 {
		if err := CheckArgs(spec, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatalf("compiled %d times for one spec", n)
	}
	spec.InputSchema = json.RawMessage(`{"type":"object"}`)
	CheckArgs(spec, nil)
	if n != 2 {
		t.Fatalf("a changed schema compiled %d times in all, want 2", n)
	}
	for i := range 3 * toolCacheSize {
		spec.Name = fmt.Sprint("t", i)
		CheckArgs(spec, nil)
	}
	toolCache.Lock()
	size := len(toolCache.m)
	toolCache.Unlock()
	if size > toolCacheSize {
		t.Fatalf("cache holds %d specs", size)
	}
}
