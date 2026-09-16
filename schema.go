package agentrt

import (
	"bytes"
	"encoding/json"
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
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("tool %q: input schema is not valid JSON: %w", toolName, err)
	}
	c := jsonschema.NewCompiler()
	url := "agentrt://tools/" + toolName + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("tool %q: %w", toolName, err)
	}
	s, err := c.Compile(url)
	if err != nil {
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
