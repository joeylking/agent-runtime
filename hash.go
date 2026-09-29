package agentrt

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// canonicalJSON re-encodes raw JSON with sorted object keys and no
// insignificant whitespace so that equal documents hash equally.
//
// Stored approval hashes and both consumers' data depend on these bytes,
// so the encoder is written out here rather than left to encoding/json,
// whose escaping and number formatting a Go release may change. It is
// byte-identical to what json.Marshal of the decoded value produced when
// the hashes were first stored, and TestCanonicalJSON_MatchesEncodingJSON
// and TestContentHash_Golden pin that.
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
	return appendCanonical(nil, v)
}

// appendCanonical writes a decoded value: objects with keys in byte order,
// numbers as their literal text, strings escaped as encoding/json escapes
// them with HTML escaping on.
func appendCanonical(dst []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if x {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case json.Number:
		if x == "" {
			return append(dst, '0'), nil
		}
		return append(dst, x...), nil
	case string:
		return appendCanonicalString(dst, x), nil
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			if dst, err = appendCanonical(dst, e); err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCanonicalString(dst, k)
			dst = append(dst, ':')
			var err error
			if dst, err = appendCanonical(dst, x[k]); err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	return nil, fmt.Errorf("agentrt: canonical JSON: unexpected %T", v)
}

const lowerHex = "0123456789abcdef"

// appendCanonicalString quotes s: '"' and '\' escaped with a backslash;
// \b, \f, \n, \r, \t by their short escapes; other control bytes, '<',
// '>', and '&' as \u00XX; U+2028 and U+2029 as   and  ; an
// invalid UTF-8 byte as �; everything else as is.
func appendCanonicalString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', lowerHex[b>>4], lowerHex[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `�`...)
			i += size
			start = i
			continue
		}
		if c == ' ' || c == ' ' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', lowerHex[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
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

// checkJSON is the boundary check for JSON a consumer hands the runtime:
// decision arguments and results, tool content, and approval capabilities
// and presentations. It requires exactly one well-formed value, valid
// UTF-8 throughout, and no object with a repeated key. The last two are
// accepted by encoding/json but decoded lossily, which would let two
// different documents share a hash.
func checkJSON(raw json.RawMessage) error {
	if !utf8.Valid(raw) {
		return errors.New("not valid UTF-8")
	}
	if !json.Valid(raw) {
		return errors.New("not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A stack of the keys seen in each open object; nil marks an array.
	var stack []map[string]bool
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if stack == nil {
				return nil // io.EOF after the single value json.Valid vouched for
			}
			return err
		}
		switch x := tok.(type) {
		case json.Delim:
			switch x {
			case '{':
				stack = append(stack, map[string]bool{})
				expectKey = true
				continue
			case '[':
				stack = append(stack, nil)
			case '}', ']':
				stack = stack[:len(stack)-1]
			}
		case string:
			if expectKey {
				keys := stack[len(stack)-1]
				if keys[x] {
					return fmt.Errorf("duplicate key %q", x)
				}
				keys[x] = true
				expectKey = false
				continue
			}
		}
		if len(stack) == 0 {
			stack = nil
			continue
		}
		// After a value inside an object the next string is a key.
		expectKey = stack[len(stack)-1] != nil
	}
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agentrt: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
