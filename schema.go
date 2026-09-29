package agentrt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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
