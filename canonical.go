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
