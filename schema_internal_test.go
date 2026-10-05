package agentrt

import (
	"encoding/json"
	"fmt"
	"strings"
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

// A panic inside the schema library is a refusal of the arguments, never
// an acceptance and never the end of the process. The boundary keeps out
// the numbers known to cause one, so validate is called directly with one.
func TestValidate_APanicInTheLibraryIsARefusal(t *testing.T) {
	for _, keyword := range []string{`"minimum":0`, `"maximum":0`, `"exclusiveMinimum":0`, `"exclusiveMaximum":0`, `"multipleOf":0.5`} {
		cs, err := compileSchema("t", json.RawMessage(`{"type":"object","properties":{"n":{"type":"number",`+keyword+`}}}`))
		if err != nil {
			t.Fatal(err)
		}
		err = cs.validate(json.RawMessage(`{"n":1e-10000000}`))
		if err == nil || !strings.Contains(err.Error(), "could not be checked against the schema, so they are refused") {
			t.Errorf("%s: %v", keyword, err)
		}
	}
	cs, err := compileSchema("t", json.RawMessage(`{"type":"array","uniqueItems":true}`))
	if err != nil {
		t.Fatal(err)
	}
	items := `[1e-10000000` + strings.Repeat(`,1`, 30) + `]`
	if err := cs.validate(json.RawMessage(items)); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("uniqueItems: %v", err)
	}
}
