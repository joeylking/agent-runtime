package agentrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// What a step records so its policy decision can be checked again later:
// the spec of the tool it named, by hash, the identity of the policy that
// decided, and in its step.policy event what the policy saw of the run.

// specEntry is a tool spec as it is recorded: its hash and the canonical
// JSON the hash is taken over.
type specEntry struct {
	spec ToolSpec
	hash string
	json string
}

// newSpecEntry encodes spec as tool_specs stores it. The hash is the hex
// SHA-256 of the canonical JSON of the whole spec, Timeout and Terminal
// included, because the policy is handed all of it.
func newSpecEntry(spec ToolSpec) (specEntry, error) {
	b, err := toJSON(spec)
	if err != nil {
		return specEntry{}, err
	}
	c, err := canonicalJSON(b)
	if err != nil {
		return specEntry{}, fmt.Errorf("%w: %w", errEncode, err)
	}
	sum := sha256.Sum256(c)
	return specEntry{spec: spec, hash: hex.EncodeToString(sum[:]), json: string(c)}, nil
}

// sameSpec reports whether two specs encode alike.
func sameSpec(a, b ToolSpec) bool {
	return a.Name == b.Name && a.Description == b.Description && a.SideEffect == b.SideEffect && a.Timeout == b.Timeout && a.Terminal == b.Terminal &&
		bytes.Equal(a.InputSchema, b.InputSchema)
}

// recordedSpec returns the recorded form of spec, encoded once per distinct
// spec of each tool: a tool's Spec is called at every step, and usually
// returns what it returned at registration.
func (d *Driver) recordedSpec(spec ToolSpec) (*specEntry, error) {
	d.specMu.Lock()
	e, ok := d.specCache[spec.Name]
	d.specMu.Unlock()
	if ok && sameSpec(e.spec, spec) {
		return e, nil
	}
	fresh, err := newSpecEntry(spec)
	if err != nil {
		return nil, err
	}
	// The cache keeps bytes of its own, which nobody it hands a spec to
	// can write into.
	fresh.spec.InputSchema = cloneBytes(spec.InputSchema)
	d.specMu.Lock()
	d.specCache[spec.Name] = &fresh
	d.specMu.Unlock()
	return &fresh, nil
}

// putSpec stores a spec under its hash unless it is stored already.
func (t *txn) putSpec(ctx context.Context, e *specEntry) error {
	_, err := t.exec(ctx, sqlPutSpec, e.hash, e.json)
	return err
}

// maxPolicyID bounds a policy's identity.
const maxPolicyID = 256

// policyID is the identity a policy records, refused when it could not be
// shown to an operator as it is.
func policyID(p Policy) (string, error) {
	ip, ok := p.(IdentifiedPolicy)
	if !ok {
		return "", nil
	}
	id := ip.PolicyID()
	switch {
	case len(id) > maxPolicyID:
		return "", fmt.Errorf("agentrt: policy identity is %d bytes, more than %d", len(id), maxPolicyID)
	case !utf8.ValidString(id):
		return "", errors.New("agentrt: policy identity is not valid UTF-8")
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("agentrt: policy identity holds the control character %U", r)
		}
	}
	return id, nil
}

// policyView is what a policy saw of the run when it decided, beyond what
// the record already holds: the run's status, step count, model
// accounting, and active time as the RunView carried them, which the
// stored run row no longer says once the run moves on, and how many steps
// and approvals the view held, which are the run's first ones in order.
// The id, goal, limits, and creation and start times are run.created's;
// the reason, detail, finish time, and result are empty while a policy
// can be asked, because the run is RUNNING, or WAITING on resume.
type policyView struct {
	Status            RunStatus `json:"status"`
	StepCount         int       `json:"step_count"`
	ModelCalls        int       `json:"model_calls"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	CachedInputTokens int       `json:"cached_input_tokens"`
	EstimatedCost     Micros    `json:"estimated_cost_micros"`
	ActiveMS          int64     `json:"active_ms"`
	Steps             int       `json:"steps"`
	Approvals         int       `json:"approvals"`
}

func seenBy(v RunView) *policyView {
	r := v.Run
	return &policyView{Status: r.Status, StepCount: r.StepCount, ModelCalls: r.ModelCalls, InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens,
		CachedInputTokens: r.Usage.CachedInputTokens, EstimatedCost: r.EstimatedCost, ActiveMS: r.ActiveTime.Milliseconds(), Steps: len(v.Steps), Approvals: len(v.Approvals)}
}

// policyRecord is a step.policy payload: the decision's fields as they
// always were, then the policy's identity, the hash of the spec it was
// handed, and what it saw. A decision the policy did not make, a
// reconciliation's or an interrupted side effect's pause, has no view.
type policyRecord struct {
	*PolicyDecision
	PolicyID string      `json:"policy_id,omitempty"`
	SpecHash string      `json:"spec_hash,omitempty"`
	View     *policyView `json:"view,omitempty"`
}

// policyEvent appends step.policy for the step's recorded policy decision.
// spec is the spec the request carried, stored here when it is not the
// one recorded with the decision, as on a resume after the tool changed.
func (t *txn) policyEvent(ctx context.Context, step *Step, at time.Time, spec *specEntry, view *policyView) error {
	rec := policyRecord{PolicyDecision: step.Policy, PolicyID: step.PolicyID, View: view}
	if spec != nil {
		rec.SpecHash = spec.hash
		if spec.hash != step.SpecHash {
			if err := t.putSpec(ctx, spec); err != nil {
				return err
			}
		}
	}
	return t.emit(ctx, Event{RunID: step.RunID, StepID: step.ID, At: at, Type: EventStepPolicy}, rec)
}

// ToolSpec returns the tool spec recorded under hash, as Step.SpecHash,
// run.created's tools, and step.policy's spec_hash name it. It reads the
// spec whole, as GetApproval reads an approval, because a decision is
// checked against all of it, and refuses one whose stored JSON no longer
// hashes to hash. ErrNotFound when no spec has that hash.
func (s *Store) ToolSpec(ctx context.Context, hash string) (ToolSpec, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT spec_json FROM tool_specs WHERE hash = ?`, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ToolSpec{}, ErrNotFound
	}
	if err != nil {
		return ToolSpec{}, err
	}
	var spec ToolSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return ToolSpec{}, fmt.Errorf("agentrt: tool spec %s: %w", hash, err)
	}
	if e, err := newSpecEntry(spec); err != nil || e.hash != hash {
		return ToolSpec{}, fmt.Errorf("agentrt: tool spec %s does not match its hash", hash)
	}
	return spec, nil
}
