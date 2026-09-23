// Package replay provides Model implementations that need no provider: a
// scripted model for unit tests, and a recorder and replayer pair so that a
// real model's responses can be captured once and served deterministically
// afterwards. Tests and CI never call a provider.
package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	agentrt "github.com/joeylking/agent-runtime"
)

// Scripted returns canned responses in order.
type Scripted struct {
	ModelName string
	Responses []agentrt.ModelResponse
	// Errors, when set, is consulted first: a non-nil entry is returned
	// instead of the response at that index.
	Errors []error
	mu     sync.Mutex
	next   int
	// Requests records every request received.
	Requests []agentrt.ModelRequest
}

// Name implements agentrt.Model.
func (s *Scripted) Name() string {
	if s.ModelName == "" {
		return "scripted"
	}
	return s.ModelName
}

// Generate implements agentrt.Model.
func (s *Scripted) Generate(_ context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, req)
	i := s.next
	s.next++
	if i < len(s.Errors) && s.Errors[i] != nil {
		return agentrt.ModelResponse{}, s.Errors[i]
	}
	if i >= len(s.Responses) {
		return agentrt.ModelResponse{}, fmt.Errorf("scripted model: no response for call %d", i)
	}
	return s.Responses[i], nil
}

// Key is the replay key of a request: the SHA-256 of its canonical JSON
// with the model name.
func Key(model string, req agentrt.ModelRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	c, err := json.Marshal(map[string]any{"model": model, "request": v})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// record is one stored exchange. The provider's raw bytes are stored as a
// JSON string rather than embedded JSON so that re-encoding cannot touch
// them: an embedded json.RawMessage would be re-indented and, with the
// default encoder, HTML-escaped, and the replayed bytes would differ from
// what the provider sent. Files written before this field existed carry
// the raw bytes embedded in response.raw and are still read.
type record struct {
	Model    string                `json:"model"`
	Request  agentrt.ModelRequest  `json:"request"`
	Response agentrt.ModelResponse `json:"response"`
	Raw      string                `json:"raw_bytes,omitempty"`
}

// Recorder wraps a model and stores every successful exchange in Dir.
//
// Exchanges with identical requests are stored in sequence: the first at
// <key>.json, the second at <key>.2.json, and so on, so a replay serves
// them in the order they were recorded rather than the last one for every
// call. The first exchange for a key in a Recorder's lifetime replaces
// whatever an earlier session recorded under that key, including any
// higher-numbered files, so re-recording a run leaves nothing stale.
type Recorder struct {
	Inner agentrt.Model
	Dir   string
	mu    sync.Mutex
	seen  map[string]int
}

// Name implements agentrt.Model.
func (r *Recorder) Name() string { return r.Inner.Name() }

// Generate implements agentrt.Model.
func (r *Recorder) Generate(ctx context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	resp, err := r.Inner.Generate(ctx, req)
	if err != nil {
		return resp, err
	}
	key, err := Key(r.Inner.Name(), req)
	if err != nil {
		return resp, err
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return resp, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	n := r.seen[key] + 1
	r.seen[key] = n
	if n == 1 {
		if err := removeSequence(r.Dir, key); err != nil {
			return resp, err
		}
	}
	stored := resp
	stored.Raw = nil
	rec := record{Model: r.Inner.Name(), Request: req, Response: stored, Raw: string(resp.Raw)}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		return resp, err
	}
	tmp := filepath.Join(r.Dir, key+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return resp, err
	}
	return resp, os.Rename(tmp, sequencePath(r.Dir, key, n))
}

// sequencePath names the nth exchange for a key. The first keeps the
// original <key>.json name so earlier recordings stay valid.
func sequencePath(dir, key string, n int) string {
	if n == 1 {
		return filepath.Join(dir, key+".json")
	}
	return filepath.Join(dir, key+"."+strconv.Itoa(n)+".json")
}

// removeSequence deletes the higher-numbered files of a key left by an
// earlier recording session. The first file is overwritten by rename.
func removeSequence(dir, key string) error {
	matches, err := filepath.Glob(filepath.Join(dir, key+".*.json"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotRecorded means the replayer has no response for the request, or
// the request has been made more times than it was recorded.
var ErrNotRecorded = errors.New("replay: no recorded response for this request")

// Replayer serves recorded responses and fails on anything unrecorded.
// Identical requests are served in the order they were recorded; a
// request made more times than it was recorded is unrecorded, because the
// replayed session has diverged from the recorded one.
type Replayer struct {
	ModelName string
	Dir       string
	mu        sync.Mutex
	served    map[string]int
}

// Name implements agentrt.Model.
func (p *Replayer) Name() string { return p.ModelName }

// Generate implements agentrt.Model.
func (p *Replayer) Generate(_ context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	key, err := Key(p.ModelName, req)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.served == nil {
		p.served = map[string]int{}
	}
	n := p.served[key] + 1
	b, err := os.ReadFile(sequencePath(p.Dir, key, n))
	if errors.Is(err, os.ErrNotExist) {
		if n == 1 {
			return agentrt.ModelResponse{}, fmt.Errorf("%w (key %s)", ErrNotRecorded, key[:12])
		}
		return agentrt.ModelResponse{}, fmt.Errorf("%w (key %s: recorded %d times, requested %d)", ErrNotRecorded, key[:12], n-1, n)
	}
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return agentrt.ModelResponse{}, err
	}
	p.served[key] = n
	resp := rec.Response
	if rec.Raw != "" {
		resp.Raw = json.RawMessage(rec.Raw)
	}
	return resp, nil
}
