package agentrt

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// canonicalJSON re-encodes raw JSON with sorted object keys and no
// insignificant whitespace so that equal documents hash equally.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null"), nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // encoding/json sorts map keys
}

// contentHash returns the hex SHA-256 of the canonical form of raw.
func contentHash(raw json.RawMessage) string {
	c, err := canonicalJSON(raw)
	if err != nil {
		c = raw
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:])
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agentrt: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
