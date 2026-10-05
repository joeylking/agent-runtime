package agentrt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// CanonicalJSON is the encoder the hashes use, for every value the
// boundary accepts: hashed, it gives the pinned content hash. It refuses
// what the boundary refuses, so it never collapses two documents.
func TestCanonicalJSON_IsTheHashedForm(t *testing.T) {
	accepted := 0
	for _, raw := range canonicalCorpus {
		if checkJSON(json.RawMessage(raw)) != nil {
			if _, err := CanonicalJSON(json.RawMessage(raw)); err == nil {
				t.Errorf("%q accepted", raw)
			}
			continue
		}
		accepted++
		c, err := CanonicalJSON(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		sum := sha256.Sum256(c)
		if got := hex.EncodeToString(sum[:]); got != contentHash(json.RawMessage(raw)) {
			t.Errorf("%q: canonical form hashes to %s, content hash %s", raw, got, contentHash(json.RawMessage(raw)))
		}
	}
	if accepted < 30 {
		t.Fatalf("only %d corpus values accepted", accepted)
	}
	same := [][2]string{{`{"b":1,"a":"x"}`, ` { "a" : "x", "b" : 1 } `}, {`{"s":"é"}`, `{"s":"é"}`}, {`{"s":"<"}`, `{"s":"<"}`}}
	for _, p := range same {
		a, _ := CanonicalJSON(json.RawMessage(p[0]))
		b, _ := CanonicalJSON(json.RawMessage(p[1]))
		if string(a) != string(b) {
			t.Errorf("%s and %s differ: %s, %s", p[0], p[1], a, b)
		}
	}
	for _, p := range [][2]string{{`{"n":10}`, `{"n":10.0}`}, {`{"n":10}`, `{"n":1e1}`}, {`{"n":1250.5}`, `{"n":1250.50}`}, {`{"n":0}`, `{"n":-0}`}, {`{"s":"A"}`, `{"s":"a"}`}} {
		a, _ := CanonicalJSON(json.RawMessage(p[0]))
		b, _ := CanonicalJSON(json.RawMessage(p[1]))
		if string(a) == string(b) {
			t.Errorf("%s and %s share the form %s", p[0], p[1], a)
		}
	}
	for _, bad := range []string{``, `{"a":1,"a":2}`, `{"s":"\ud800"}`, "{\"s\":\"\xff\"}", `{"n":1e-10000000}`, `{} {}`, strings.Repeat("[", 300) + strings.Repeat("]", 300)} {
		if _, err := CanonicalJSON(json.RawMessage(bad)); err == nil || !strings.HasPrefix(err.Error(), "agentrt: canonical JSON: ") {
			t.Errorf("%.30q: %v", bad, err)
		}
	}
}
