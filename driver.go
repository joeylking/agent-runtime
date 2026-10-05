package agentrt

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Config configures a Driver.
type Config struct {
	Store    *Store
	Agent    Agent
	Policy   Policy
	Tools    []Tool
	Observer Observer
	// Model, when set, is wrapped in a ModelCaller that agents receive in
	// StepInput. Scripted agents leave it nil.
	Model *ModelConfig
	// Reconcile, when set, runs before a run found mid-step is continued by
	// Resume. Consumers reconcile their own journaled operations and say
	// whether the run continues, completed, waits, or is in conflict.
	Reconcile func(ctx context.Context, view RunView) (Reconciliation, error)
	// Now and NewID may be overridden by tests.
	Now   func() time.Time
	NewID func() string
	// LeaseTTL is how long the lease a driver takes on a run it executes
	// lasts without renewal; a heartbeat renews it at a third of that.
	// Zero means DefaultLeaseTTL. Lease times are read from the wall
	// clock, never from Now.
	LeaseTTL time.Duration
	// LeaseOwner names this driver in the leases it takes, for an operator
	// reading them. The owner stored is the name followed by a random
	// suffix drawn per Driver, so two drivers, in one process or two,
	// never hold a lease as the same owner, and a restarted process waits
	// out the leases its previous life held like any other process. Empty
	// means the random part alone.
	LeaseOwner string
}

// Driver runs the step loop. It is safe to reuse for many runs but a single
// run is executed by one call at a time.
//
// The loop is the agent's decisions passed through a Session, the same one
// a Gate hands to a loop the runtime does not own; a Gate is a Driver with
// no agent.
type Driver struct {
	store     *Store
	agent     Agent
	policy    Policy
	tools     map[string]Tool
	schemas   map[string]*compiledSchema
	specs     []ToolSpec
	observer  Observer
	model     *ModelConfig
	reconcile func(ctx context.Context, view RunView) (Reconciliation, error)
	now       func() time.Time
	newID     func() string
	// reload makes the loop load its steps and approvals from the store at
	// every step and encode every field of each step it writes, as it did
	// before it kept them; written is called after each write the loop
	// applies to its cache. Both are for tests.
	reload  bool
	written func(*runCache)
	// terms are the lease this driver's writes claim. wall is the clock
	// lease times are read from, sleep waits between model retries, and
	// noHeartbeat stops a lease from being renewed, as when a process
	// dies; the last three are for tests.
	terms       leaseTerms
	wall        func() time.Time
	sleep       func(context.Context, time.Duration) error
	noHeartbeat bool
	// renewEvery overrides the renewal interval of a third of the TTL, so
	// a test can renew often under a lease long enough for a slow runner.
	renewEvery time.Duration
	// active holds the runs a call or session of this driver is executing,
	// so a second Start, Resume, Begin, or Attach of one of them here is
	// refused rather than taking over the lease this driver itself holds.
	mu     sync.Mutex
	active map[string]bool
}

// NewDriver validates the configuration and compiles every tool schema.
func NewDriver(cfg Config) (*Driver, error) {
	if cfg.Store == nil {
		return nil, errors.New("agentrt: store is required")
	}
	if cfg.Agent == nil {
		return nil, errors.New("agentrt: agent is required")
	}
	d, err := newDriver(GateConfig{Store: cfg.Store, Policy: cfg.Policy, Tools: cfg.Tools, Observer: cfg.Observer, Model: cfg.Model,
		Reconcile: cfg.Reconcile, Now: cfg.Now, NewID: cfg.NewID, LeaseTTL: cfg.LeaseTTL, LeaseOwner: cfg.LeaseOwner})
	if err != nil {
		return nil, err
	}
	d.agent = cfg.Agent
	return d, nil
}

// newDriver is NewDriver without an agent: a Gate is a driver whose loop
// belongs to its caller.
func newDriver(cfg GateConfig) (*Driver, error) {
	if cfg.Store == nil {
		return nil, errors.New("agentrt: store is required")
	}
	if cfg.Policy == nil {
		return nil, errors.New("agentrt: policy is required")
	}
	if cfg.Model != nil && cfg.Model.Model == nil {
		return nil, errors.New("agentrt: model config without a model")
	}
	d := &Driver{
		store:     cfg.Store,
		policy:    cfg.Policy,
		tools:     map[string]Tool{},
		schemas:   map[string]*compiledSchema{},
		observer:  cfg.Observer,
		model:     cfg.Model,
		reconcile: cfg.Reconcile,
		now:       cfg.Now,
		newID:     cfg.NewID,
		wall:      time.Now,
		sleep:     sleep,
	}
	d.terms = leaseTerms{owner: newID(), ttl: cfg.LeaseTTL, wall: func() time.Time { return d.wall() }}
	if cfg.LeaseOwner != "" {
		d.terms.owner = cfg.LeaseOwner + "/" + d.terms.owner[:12]
	}
	if d.terms.ttl <= 0 {
		d.terms.ttl = DefaultLeaseTTL
	}
	d.terms.arg = d.terms.owner
	if d.now == nil {
		d.now = time.Now
	}
	if d.newID == nil {
		d.newID = newID
	}
	for _, t := range cfg.Tools {
		spec := t.Spec()
		if _, dup := d.tools[spec.Name]; dup && spec.Name != "" {
			return nil, fmt.Errorf("agentrt: duplicate tool %q", spec.Name)
		}
		ts, err := CompileTool(spec)
		if err != nil {
			return nil, err
		}
		d.tools[spec.Name] = t
		d.schemas[spec.Name] = ts.schema
		d.specs = append(d.specs, spec)
	}
	sort.Slice(d.specs, func(i, j int) bool { return d.specs[i].Name < d.specs[j].Name })
	return d, nil
}

// Start creates a run and executes it until it reaches a terminal status or
// pauses for approval. The returned run reflects the persisted state.
func (d *Driver) Start(ctx context.Context, goal string, limits Limits) (Run, error) {
	return d.StartWithID(ctx, d.newID(), goal, limits)
}

// StartWithID is Start with a caller-chosen run id, so a consumer can key
// its own tables by the same identifier. The id must be unique.
func (d *Driver) StartWithID(ctx context.Context, id, goal string, limits Limits) (Run, error) {
	s, err := d.begin(ctx, id, goal, limits)
	if err != nil {
		return Run{}, err
	}
	defer s.Close()
	return d.loop(ctx, s)
}

// claim marks a run as executing in a call of this driver, and reports
// false when another call already is.
func (d *Driver) claim(runID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active[runID] {
		return false
	}
	if d.active == nil {
		d.active = map[string]bool{}
	}
	d.active[runID] = true
	return true
}

func (d *Driver) unclaim(runID string) {
	d.mu.Lock()
	delete(d.active, runID)
	d.mu.Unlock()
}

// busy is the error for a run another call of this driver is executing.
func (d *Driver) busy(ctx context.Context, runID string) error {
	e := ErrRunLeased{RunID: runID, Owner: d.terms.owner}
	if owner, until, err := d.store.lease(ctx, runID); err == nil && owner == d.terms.owner {
		e.ExpiresAt = until
	}
	return e
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recordTimeout bounds the writes that record work already done after the
// caller's context was cancelled.
const recordTimeout = 10 * time.Second

// afterEffect returns the context for recording something that has already
// happened: a tool that ran, a model request that was sent. Cancelling the
// caller's context must not lose that record, so it is detached from
// cancellation and given a short deadline of its own.
func afterEffect(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}
