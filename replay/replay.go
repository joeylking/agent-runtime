// Package replay provides Model implementations that need no provider: a
// scripted model for unit tests, and a recorder and replayer pair so that a
// real model's responses can be captured once and served deterministically
// afterwards. Tests and CI never call a provider.
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// record is one stored exchange.
type record struct {
	Model    string                `json:"model"`
	Request  agentrt.ModelRequest  `json:"request"`
	Response agentrt.ModelResponse `json:"response"`
}

// Recorder wraps a model and stores every successful exchange in Dir.
type Recorder struct {
	Inner agentrt.Model
	Dir   string
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
	b, err := json.MarshalIndent(record{Model: r.Inner.Name(), Request: req, Response: resp}, "", "  ")
	if err != nil {
		return resp, err
	}
	tmp := filepath.Join(r.Dir, key+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return resp, err
	}
	return resp, os.Rename(tmp, filepath.Join(r.Dir, key+".json"))
}

// ErrNotRecorded means the replayer has no response for the request.
var ErrNotRecorded = errors.New("replay: no recorded response for this request")

// Replayer serves recorded responses and fails on anything unrecorded.
type Replayer struct {
	ModelName string
	Dir       string
}

// Name implements agentrt.Model.
func (p *Replayer) Name() string { return p.ModelName }

// Generate implements agentrt.Model.
func (p *Replayer) Generate(_ context.Context, req agentrt.ModelRequest) (agentrt.ModelResponse, error) {
	key, err := Key(p.ModelName, req)
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return agentrt.ModelResponse{}, fmt.Errorf("%w (key %s)", ErrNotRecorded, key[:12])
	}
	if err != nil {
		return agentrt.ModelResponse{}, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return agentrt.ModelResponse{}, err
	}
	return rec.Response, nil
}
