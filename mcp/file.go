package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// FileRule is one Rule as an operator writes it in a JSON file, under the
// name the server gives the tool: the side-effect class, a timeout as a Go
// duration string, and the rest of Rule's fields in snake case. It is the
// operator's format rather than Rule itself because a rule is configuration
// an operator edits, and a duration reads better as "5s" than as
// nanoseconds.
type FileRule struct {
	SideEffect        agentrt.SideEffect         `json:"side_effect"`
	Timeout           string                     `json:"timeout,omitempty"`
	Terminal          bool                       `json:"terminal,omitempty"`
	Description       string                     `json:"description,omitempty"`
	Deny              []string                   `json:"deny,omitempty"`
	Fixed             map[string]json.RawMessage `json:"fixed,omitempty"`
	Rename            string                     `json:"rename,omitempty"`
	AllowHintMismatch bool                       `json:"allow_hint_mismatch,omitempty"`
}

// Rule converts the operator's line to a Rule. Only the timeout can fail to
// convert; everything else Load checks against the pinned tool.
func (r FileRule) Rule() (Rule, error) {
	var timeout time.Duration
	if r.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return Rule{}, fmt.Errorf("timeout: %w", err)
		}
	}
	return Rule{
		SideEffect:        r.SideEffect,
		Timeout:           timeout,
		Terminal:          r.Terminal,
		Description:       r.Description,
		Deny:              r.Deny,
		Fixed:             r.Fixed,
		Rename:            r.Rename,
		AllowHintMismatch: r.AllowHintMismatch,
	}, nil
}

// ReadRules reads an operator's rules file, a JSON object mapping each
// server tool name to its FileRule, through ReadOwned. A field the format
// does not know is refused rather than ignored, because a misspelled "deny"
// would otherwise silently let a parameter through. A tool that is not in
// the file has no rule and is not registered, which is how the file says no.
func ReadRules(path string) (Rules, error) {
	raw, err := ReadOwned(path)
	if err != nil {
		return nil, err
	}
	var file map[string]FileRule
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(Rules, len(file))
	for name, r := range file {
		rule, err := r.Rule()
		if err != nil {
			return nil, fmt.Errorf("%s: tool %q: %w", path, name, err)
		}
		out[name] = rule
	}
	return out, nil
}

// ReadManifest reads a manifest Pin produced, through ReadOwned.
func ReadManifest(path string) (*Manifest, error) {
	raw, err := ReadOwned(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// ReadOwned reads a file that decides what tools may do, such as a
// manifest, a rules file, or a configuration naming them, only when the
// current user owns it and neither its group nor anyone else can write it:
// whoever can write it chooses how every tool is classified. The checks are
// made on the open file, so the file read is the file checked. Where file
// modes do not describe an owner, a group, and everyone else, as on
// Windows, only the regular-file check applies.
func ReadOwned(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if err := OwnedPrivately(path, info); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
