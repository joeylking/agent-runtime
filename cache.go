package agentrt

import (
	"context"
	"slices"
)

// runCache is one loop's copy of its run's steps and approvals. The loop
// loads it once when it starts; after each write commits, the step and
// approval rows written are decoded from the very columns stored, the way
// ListSteps and ListApprovals decode them, so it equals a fresh load after
// every write and a step reads O(1) rows instead of every prior step. A
// write that fails leaves it as it was. It is never shared between loops:
// Start and Resume each load their own.
//
// The agent and the policy each get a mirror of it with bytes of their
// own, and every handout is a fresh slice of fresh structs over that
// mirror. Neither can change the driver's copy or the other's view; the
// one thing that persists is a write into the bytes of a RawMessage a
// consumer was handed, and only into that consumer's own later views.
type runCache struct {
	steps     []Step
	approvals []Approval
	// last is the row last written, which lends the text of fields a later
	// write of the same step leaves unchanged.
	last stepRow
	// version counts the changes to each step, and to the approvals, so a
	// mirror copies only what changed since its last handout.
	version       []int
	apprVersion   int
	agent, policy mirror
	// stale means a row could not be applied; the loop loads afresh.
	stale bool
}

func (d *Driver) load(ctx context.Context, runID string) (*runCache, error) {
	rows, err := d.store.listStepRows(ctx, runID)
	if err != nil {
		return nil, err
	}
	approvals, err := d.store.ListApprovals(ctx, runID)
	if err != nil {
		return nil, err
	}
	c := &runCache{approvals: approvals, version: make([]int, len(rows)), apprVersion: 1}
	for i := range rows {
		c.version[i] = 1
		c.steps = append(c.steps, rows[i].decode(nil, nil))
	}
	return c, nil
}

// write runs fn in a transaction under the driver's lease terms and, once
// it has committed, applies the rows it wrote to c, which may be nil
// outside a loop.
func (d *Driver) write(ctx context.Context, c *runCache, fn func(t *txn) error) error {
	var done *txn
	err := d.store.tx(ctx, d.observer, func(t *txn) error {
		t.lease = &d.terms
		if done = t; !d.reload {
			t.cache = c
		}
		return fn(t)
	})
	if err != nil || c == nil {
		return err
	}
	for _, r := range done.steps {
		c.putStep(r)
	}
	for _, r := range done.approvals {
		c.putApproval(r)
	}
	if d.written != nil {
		d.written(c)
	}
	return nil
}

func (c *runCache) putStep(r stepRow) {
	i := r.index
	var prev *Step
	switch {
	case i == len(c.steps) && i == len(c.version):
	case i < len(c.steps) && c.steps[i].ID == r.id:
		prev = &c.steps[i]
	default:
		c.stale = true
		return
	}
	st := r.decode(&c.last, prev)
	if st.DecodeError != "" {
		c.stale = true
		return
	}
	if prev == nil {
		c.steps, c.version = append(c.steps, st), append(c.version, 0)
	} else {
		c.steps[i] = st
	}
	c.version[i]++
	c.last = r
}

func (c *runCache) putApproval(r approvalRow) {
	a := r.decode()
	if a.DecodeError != "" {
		c.stale = true
		return
	}
	c.approvals = append(c.approvals, a)
	c.apprVersion++
}

// mirror is one consumer's copy of the cache, bytes included.
type mirror struct {
	steps     []Step
	seen      []int
	approvals []Approval
	apprSeen  int
}

// view hands out the first n steps and every approval as a fresh load
// would return them: nil when there are none.
func (m *mirror) view(c *runCache, n int) ([]Step, []Approval) {
	for i := range n {
		if i == len(m.steps) {
			m.steps, m.seen = append(m.steps, Step{}), append(m.seen, 0)
		}
		if m.seen[i] != c.version[i] {
			m.steps[i], m.seen[i] = cloneStep(c.steps[i]), c.version[i]
		}
	}
	if m.apprSeen != c.apprVersion {
		m.approvals, m.apprSeen = cloneApprovals(c.approvals), c.apprVersion
	}
	var steps []Step
	if n > 0 {
		steps = make([]Step, n)
		decisions, policies, observations := make([]Decision, n), make([]PolicyDecision, n), make([]Observation, n)
		for i, st := range m.steps[:n] {
			if st.Decision != nil {
				decisions[i] = *st.Decision
				st.Decision = &decisions[i]
			}
			if st.Policy != nil {
				policies[i] = *st.Policy
				st.Policy = &policies[i]
			}
			if st.Observation != nil {
				observations[i] = *st.Observation
				st.Observation = &observations[i]
			}
			steps[i] = st
		}
	}
	return steps, slices.Clone(m.approvals)
}

// cloneBytes copies b, keeping nil and empty apart as a decode does.
func cloneBytes[T ~[]byte](b T) T {
	if b == nil {
		return nil
	}
	return append(make(T, 0, len(b)), b...)
}

// cloneStep copies a step, what it points to, and its bytes.
func cloneStep(st Step) Step {
	if st.Decision != nil {
		d := *st.Decision
		d.Args, d.Result = cloneBytes(d.Args), cloneBytes(d.Result)
		st.Decision = &d
	}
	if st.Policy != nil {
		p := *st.Policy
		p.Capability, p.Presentation = cloneBytes(p.Capability), cloneBytes(p.Presentation)
		st.Policy = &p
	}
	if st.Observation != nil {
		o := *st.Observation
		o.Content = cloneBytes(o.Content)
		st.Observation = &o
	}
	return st
}

func cloneSteps(steps []Step) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	for i, st := range steps {
		out[i] = cloneStep(st)
	}
	return out
}

func cloneApprovals(approvals []Approval) []Approval {
	if approvals == nil {
		return nil
	}
	out := make([]Approval, len(approvals))
	for i, a := range approvals {
		a.Capability, a.Presentation = cloneBytes(a.Capability), cloneBytes(a.Presentation)
		a.Request.Args, a.Request.Spec.InputSchema = cloneBytes(a.Request.Args), cloneBytes(a.Request.Spec.InputSchema)
		out[i] = a
	}
	return out
}
