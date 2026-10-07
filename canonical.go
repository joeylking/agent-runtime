package agentrt

import (
	"encoding/json"
	"fmt"
)

// CanonicalJSON returns the form of a JSON value the runtime hashes:
// object keys in byte order, no insignificant whitespace, every number as
// its literal was written, and every string by its exact contents in one
// fixed escaping. Documents that differ only in key order, whitespace, or
// how a string's characters are escaped have the same form; documents that
// differ in any number's literal or any string's contents do not, so 10,
// 10.0, and 1e1 are three values. It refuses, with the reason, what the
// runtime refuses where consumer JSON enters: malformed JSON, nesting past
// 256 levels, invalid UTF-8, an escaped surrogate outside a pair, a
// repeated key, and a number past the bound. Everything it accepts it
// encodes without loss. Content and approval hashes are computed over
// this form, which TestContentHash_Golden pins.
func CanonicalJSON(raw json.RawMessage) ([]byte, error) {
	if err := checkJSON(raw); err != nil {
		return nil, fmt.Errorf("agentrt: canonical JSON: %w", err)
	}
	return canonicalJSON(raw)
}

// PolicyJSON returns the form of a JSON value a policy is handed: object
// keys in byte order, no insignificant whitespace, every number as its
// literal was written, and every string by its exact contents, escaped
// only as JSON requires: '"', '\', and control characters. Unlike
// CanonicalJSON it leaves '<', '>', '&', U+2028, and U+2029 as they are,
// so a policy matching "&&" in an argument finds it. It refuses what
// CanonicalJSON refuses. Two documents with the same CanonicalJSON have
// the same PolicyJSON, so the form is the same whether it is made from
// what an agent wrote or from what the runtime stored, which is compact
// and HTML-escaped. Hashes are never computed over this form.
func PolicyJSON(raw json.RawMessage) (json.RawMessage, error) {
	if err := checkJSON(raw); err != nil {
		return nil, fmt.Errorf("agentrt: policy JSON: %w", err)
	}
	return policyForm(raw)
}

// policyRequest is req as a policy is handed it: its arguments in
// PolicyJSON form and its spec's input schema in policyForm, fresh bytes
// the policy may keep. The request the runtime hashes and executes is req.
func policyRequest(req ToolRequest) (ToolRequest, error) {
	args, err := PolicyJSON(req.Args)
	if err != nil {
		return ToolRequest{}, fmt.Errorf("%w: arguments: %w", errEncode, err)
	}
	schema, err := policyForm(req.Spec.InputSchema)
	if err != nil {
		return ToolRequest{}, fmt.Errorf("%w: input schema of %q: %w", errEncode, req.Spec.Name, err)
	}
	out := req
	out.Args, out.Spec.InputSchema = args, schema
	return out, nil
}
