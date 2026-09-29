package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// canonicalSchema re-encodes a schema from the bytes the server sent, with
// sorted keys, no insignificant whitespace, and no HTML escaping, so that
// equal documents hash equally. It works from the raw bytes rather than the
// SDK's decoded value because that value holds every number as a float64,
// and a pin that cannot see past float64 precision cannot see a server move
// a bound from 9007199254740993 to 9007199254740992.
//
// A number is written as float64 formatting writes it whenever that loses
// nothing: when the shortest float64 text of the parsed value denotes the
// same decimal as the literal the server sent. So 0.1, 1.0, 1e2, and -0 are
// written 0.1, 1, 100, and -0, exactly as before this read raw bytes, and
// every manifest pinned from such a schema still loads. Only a number float64
// cannot hold is written from its literal, normalised by normalNumber, so a
// schema that carries one hashes differently from a pin taken before, which
// saw the rounded value.
func canonicalSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("null")
	}
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the schema")
	}
	doc, err := canonicalNumbers(doc)
	if err != nil {
		return nil, err
	}
	return marshalCanonical(doc)
}

func canonicalNumbers(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		text, err := numberText(string(x))
		return json.Number(text), err
	case map[string]any:
		for k, e := range x {
			c, err := canonicalNumbers(e)
			if err != nil {
				return nil, err
			}
			x[k] = c
		}
	case []any:
		for i, e := range x {
			c, err := canonicalNumbers(e)
			if err != nil {
				return nil, err
			}
			x[i] = c
		}
	}
	return v, nil
}

// numberText is how one JSON number literal is written: as encoding/json
// writes the float64 it parses to when that float64 text denotes the same
// decimal, and as normalNumber otherwise.
func numberText(lit string) (string, error) {
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return "", fmt.Errorf("number %s: %w", lit, err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	float := string(b)
	litDigits, litExp, litNeg, err := decimal(lit)
	if err != nil {
		return "", err
	}
	fDigits, fExp, fNeg, err := decimal(float)
	if err != nil {
		return "", err
	}
	if litDigits == fDigits && litExp == fExp && (litNeg == fNeg || litDigits == "") {
		return float, nil
	}
	return normalNumber(litDigits, litExp, litNeg), nil
}

// decimal splits a JSON number into significant digits and an exponent, the
// value being digits × 10^exp with no leading or trailing zero in digits.
// Zero is empty digits.
func decimal(s string) (digits string, exp int, neg bool, err error) {
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	mant, expText := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, expText = s[:i], s[i+1:]
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits = strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return "", 0, neg, nil
	}
	e := 0
	if expText != "" {
		if e, err = strconv.Atoi(strings.TrimPrefix(expText, "+")); err != nil {
			return "", 0, false, fmt.Errorf("number %s: exponent: %w", s, err)
		}
	}
	exp = e - len(frac)
	trimmed := strings.TrimRight(digits, "0")
	exp += len(digits) - len(trimmed)
	return trimmed, exp, neg, nil
}

// normalNumber writes a decimal in the shape encoding/json gives a float64,
// with every digit the literal carried: plain notation, or d.ddde±x when the
// magnitude is below 1e-6 or at least 1e21.
func normalNumber(digits string, exp int, neg bool) string {
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	sci := len(digits) - 1 + exp
	switch {
	case sci < -6 || sci >= 21:
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if sci >= 0 {
			b.WriteByte('+')
		}
		b.WriteString(strconv.Itoa(sci))
	case exp >= 0:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", exp))
	default:
		point := len(digits) + exp
		if point > 0 {
			b.WriteString(digits[:point])
			b.WriteByte('.')
			b.WriteString(digits[point:])
		} else {
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -point))
			b.WriteString(digits)
		}
	}
	return b.String()
}

func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func hashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// hashString hashes a string through its JSON form, so descriptions and
// schemas are hashed over the same representation.
func hashString(s string) string {
	b, err := marshalCanonical(s)
	if err != nil {
		b = []byte(s)
	}
	return hashBytes(b)
}
