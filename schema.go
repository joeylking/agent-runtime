package agentrt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compiledSchema validates tool arguments against a tool's input schema.
type compiledSchema struct {
	schema *jsonschema.Schema
}

func compileSchema(toolName string, raw json.RawMessage) (*compiledSchema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("tool %q: input schema is required", toolName)
	}
	// The schema is part of every approval request the tool is named in,
	// so it is held to the depth that request's hash must encode.
	if err := checkDepth(raw); err != nil {
		return nil, fmt.Errorf("tool %q: input schema is %w", toolName, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("tool %q: input schema is not valid JSON: %w", toolName, err)
	}
	c := jsonschema.NewCompiler()
	// A schema is compiled from itself and the standard metaschemas, which
	// the library embeds; the default loader would read any file: URL, so
	// a $ref, $schema, or $id naming anything else could read the
	// operator's files or block on a pipe. No URL is loaded.
	c.UseLoader(jsonschema.SchemeURLLoader{})
	url := "agentrt://tools/" + toolName + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("tool %q: %w", toolName, err)
	}
	s, err := c.Compile(url)
	if err != nil {
		var ext *jsonschema.LoadURLError
		if errors.As(err, &ext) {
			return nil, fmt.Errorf("tool %q: input schema refers to %s, outside itself; only references within the schema are resolved: %w", toolName, ext.URL, err)
		}
		return nil, fmt.Errorf("tool %q: input schema does not compile: %w", toolName, err)
	}
	return &compiledSchema{schema: s}, nil
}

// ToolSchema is a tool's spec as NewDriver accepts it, with its input
// schema compiled once: what a test, a linter, or a channel needs to judge
// arguments as the runtime would, without a driver or a store. It is safe
// for concurrent use.
type ToolSchema struct {
	schema *compiledSchema
}

// CompileTool checks spec as NewDriver checks each tool, a name, a valid
// side effect, a timeout, and an input schema that compiles from itself
// alone, and returns it compiled. Its error is the one NewDriver returns
// for the same spec.
func CompileTool(spec ToolSpec) (*ToolSchema, error) {
	if spec.Name == "" {
		return nil, errors.New("agentrt: tool with empty name")
	}
	if !spec.SideEffect.valid() {
		return nil, fmt.Errorf("agentrt: tool %q: invalid side effect %q", spec.Name, spec.SideEffect)
	}
	if spec.Timeout <= 0 {
		return nil, fmt.Errorf("agentrt: tool %q: timeout is required", spec.Name)
	}
	cs, err := compileSchema(spec.Name, spec.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("agentrt: %w", err)
	}
	return &ToolSchema{schema: cs}, nil
}

// Check reports whether the runtime accepts args for the tool: the JSON
// boundary checks a decision's arguments meet (depth, UTF-8, escaped
// surrogates, repeated keys), then the input schema. Empty arguments mean
// {}. Its error text is the one the driver records in an invalid_decision
// observation for the same arguments.
func (s *ToolSchema) Check(args json.RawMessage) error { return checkArgs(s.schema, args) }

// checkArgs is the loop's acceptance of a tool call's arguments.
func checkArgs(cs *compiledSchema, args json.RawMessage) error {
	if len(bytes.TrimSpace(args)) > 0 {
		if err := checkJSON(args); err != nil {
			return fmt.Errorf("arguments are not usable JSON: %w", err)
		}
	}
	return cs.validate(args)
}

// CheckArgs is CompileTool(spec) then Check(args). The compiled schemas of
// the last few specs it was given are kept, so a loop over one spec does
// not recompile; a caller holding many specs keeps CompileTool's results.
// A spec NewDriver would refuse is an error, CompileTool's.
func CheckArgs(spec ToolSpec, args json.RawMessage) error {
	ts, err := compileCached(spec)
	if err != nil {
		return err
	}
	return ts.Check(args)
}

// toolCacheSize bounds CheckArgs's cache; the cache is emptied when full.
const toolCacheSize = 32

type toolKey struct {
	name, schema string
	side         SideEffect
	timeout      time.Duration
}

var toolCache = struct {
	sync.Mutex
	m map[toolKey]*ToolSchema
}{m: map[toolKey]*ToolSchema{}}

// compiles counts compilations by CompileTool. Tests read it.
var compiles = func() {}

func compileCached(spec ToolSpec) (*ToolSchema, error) {
	k := toolKey{spec.Name, string(spec.InputSchema), spec.SideEffect, spec.Timeout}
	toolCache.Lock()
	ts := toolCache.m[k]
	toolCache.Unlock()
	if ts != nil {
		return ts, nil
	}
	compiles()
	ts, err := CompileTool(spec)
	if err != nil {
		return nil, err
	}
	toolCache.Lock()
	if len(toolCache.m) >= toolCacheSize {
		clear(toolCache.m)
	}
	toolCache.m[k] = ts
	toolCache.Unlock()
	return ts, nil
}

// validate checks raw against the schema. Empty arguments are treated as an
// empty object.
func (c *compiledSchema) validate(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if err := c.schema.Validate(v); err != nil {
		return fmt.Errorf("arguments do not match schema: %w", err)
	}
	return nil
}
