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
	"strings"
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

// maxJSONDepth is the deepest nesting of arrays and objects the runtime
// accepts in JSON a consumer hands it. encoding/json refuses to decode, and
// on Go 1.27 to encode, past 10000 levels, and the runtime stores and
// hashes a consumer's value inside objects of its own, so a value that
// passed a check at 10000 would fail once wrapped: unencodable on one Go
// release and undecodable on another. 256 leaves that margin many times
// over, is far past any tool's arguments or result, and bounds the
// recursion of the canonical encoder and the schema validator.
const maxJSONDepth = 256

// checkDepth rejects JSON nested deeper than maxJSONDepth. It counts
// brackets outside strings and needs no valid input, so it runs before
// anything that recurses.
func checkDepth(raw []byte) error {
	depth, inString := 0, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			if depth++; depth > maxJSONDepth {
				return fmt.Errorf("nested deeper than %d levels", maxJSONDepth)
			}
		case ']', '}':
			depth--
		}
	}
	return nil
}

// maxNumberLength and maxNumberExponent bound a JSON number the runtime
// accepts from a consumer: its literal at most 10000 bytes, and the
// exponent written after its e or E at most 10000 in absolute value. The
// schema validator reads a number as a big.Rat, which refuses a decimal
// exponent, written or implied by the digits after the point, past a
// million and leaves nil for it, and validation against minimum, maximum,
// the exclusive bounds, multipleOf, or uniqueItems then dereferences that
// nil and panics: a single argument such as 1e-10000000 brought the
// process down. Below the library's limit a number still costs time in
// proportion to its exponent. float64 reaches 1e308, and IEEE decimal128,
// the widest exponent range in common use, 1e6144, so no number a tool
// legitimately takes comes near either bound, and together they keep the
// implied exponent fifty times inside the library's limit.
const (
	maxNumberLength   = 10000
	maxNumberExponent = 10000
)

// checkNumbers rejects a number past maxNumberLength or maxNumberExponent.
// It reads number literals outside strings and needs no valid input.
func checkNumbers(raw []byte) error {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch c {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		if c != '-' && (c < '0' || c > '9') {
			continue
		}
		start := i
		for i < len(raw) && strings.IndexByte("0123456789+-.eE", raw[i]) >= 0 {
			i++
		}
		if err := checkNumber(raw[start:i]); err != nil {
			return err
		}
		i--
	}
	return nil
}

func checkNumber(lit []byte) error {
	quoted := lit
	if len(quoted) > 24 {
		quoted = append(append([]byte(nil), lit[:24]...), "..."...)
	}
	if len(lit) > maxNumberLength {
		return fmt.Errorf("number %s is longer than %d characters", quoted, maxNumberLength)
	}
	e := bytes.IndexAny(lit, "eE")
	if e < 0 {
		return nil
	}
	exp := bytes.TrimLeft(bytes.TrimLeft(lit[e+1:], "+-"), "0")
	// Five digits hold every exponent up to the bound, and more cannot.
	if len(exp) > 5 {
		return fmt.Errorf("number %s has an exponent beyond ±%d", quoted, maxNumberExponent)
	}
	n := 0
	for _, d := range exp {
		n = n*10 + int(d-'0')
	}
	if n > maxNumberExponent {
		return fmt.Errorf("number %s has an exponent beyond ±%d", quoted, maxNumberExponent)
	}
	return nil
}

// checkJSON is the boundary check for JSON a consumer hands the runtime:
// decision arguments and results, tool content, approval capabilities and
// presentations, and a reconciliation's result. It requires exactly one
// well-formed value nested no deeper than maxJSONDepth, valid UTF-8
// throughout, no escaped surrogate outside a pair, no object with a
// repeated key, and no number past maxNumberLength or maxNumberExponent.
// Invalid UTF-8, lone surrogates, and repeated keys are accepted by
// encoding/json but decoded lossily, which would let two different
// documents share a hash; such a number makes the schema validator panic.
// The number bound is checked last, so a value refused before it existed
// is refused with the same reason.
func checkJSON(raw json.RawMessage) error {
	if err := checkDepth(raw); err != nil {
		return err
	}
	if !utf8.Valid(raw) {
		return errors.New("not valid UTF-8")
	}
	if !json.Valid(raw) {
		return errors.New("not valid JSON")
	}
	if err := checkSurrogates(raw); err != nil {
		return err
	}
	if err := checkKeys(raw); err != nil {
		return err
	}
	return checkNumbers(raw)
}

// checkKeys rejects an object with a repeated key in valid JSON. Its
// tokens decode numbers as float64, so a number past float64's range
// nested in an array or object is refused here too, as it always was.
func checkKeys(raw json.RawMessage) error {
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

// checkSurrogates rejects a \u escape of a UTF-16 surrogate that is not a
// high surrogate followed at once by an escaped low one. encoding/json
// decodes such an escape as U+FFFD, so "\ud800" and "\ufffd" would hash
// alike. raw is valid JSON, so a backslash is inside a string and starts
// an escape.
func checkSurrogates(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		r := hex4(raw[i+2 : i+6])
		switch {
		case r >= 0xd800 && r < 0xdc00:
			if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
				return fmt.Errorf("unpaired surrogate %s", raw[i:i+6])
			}
			if lo := hex4(raw[i+8 : i+12]); lo < 0xdc00 || lo >= 0xe000 {
				return fmt.Errorf("unpaired surrogate %s", raw[i:i+6])
			}
			i += 11
		case r >= 0xdc00 && r < 0xe000:
			return fmt.Errorf("unpaired surrogate %s", raw[i:i+6])
		default:
			i += 5
		}
	}
	return nil
}

// hex4 decodes four hex digits that json.Valid has vouched for.
func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		default:
			c -= 'A' - 10
		}
		r = r<<4 | rune(c)
	}
	return r
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("agentrt: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
