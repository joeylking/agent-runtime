package agentrt

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// DefaultLeaseTTL is the lease a driver holds on a run it executes when
// Config.LeaseTTL is zero. The lease is renewed at a third of it.
const DefaultLeaseTTL = 30 * time.Second

// leaseTerms are what a driver's writes claim: its owner id, and the expiry
// a write that moves a run to RUNNING stamps, read from the wall clock.
type leaseTerms struct {
	owner string
	ttl   time.Duration
	wall  func() time.Time
	// arg is owner as a statement argument, boxed once rather than at
	// every write.
	arg any
}

func (l *leaseTerms) until() time.Time { return l.wall().Add(l.ttl) }

// lease is one call's hold on a run: from the write that moves the run to
// RUNNING until the call returns. A heartbeat renews it on the wall clock,
// whatever the loop is doing, so a long tool or model call keeps it. The
// renewal is compare-and-set on the owner; when it fails the lease is lost
// and the loop stops before its next side effect.
type lease struct {
	d      *Driver
	runID  string
	until  atomic.Int64 // unix nanoseconds of the last expiry written
	lost   atomic.Bool
	cancel context.CancelFunc
	done   chan struct{}
}

func (d *Driver) newLease(runID string) *lease { return &lease{d: d, runID: runID} }

// start begins the heartbeat for a lease acquired by a write that read the
// wall clock no earlier than at, so at+ttl is no later than what it stored.
func (l *lease) start(at time.Time) {
	if l.cancel != nil {
		return
	}
	ttl := l.d.terms.ttl
	l.until.Store(at.Add(ttl).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel, l.done = cancel, make(chan struct{})
	if l.d.noHeartbeat {
		close(l.done)
		return
	}
	every := ttl / 3
	if l.d.renewEvery > 0 {
		every = l.d.renewEvery
	}
	go func() {
		defer close(l.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			at := l.d.wall()
			rctx, rcancel := context.WithTimeout(ctx, ttl/3)
			held, err := l.d.store.renewLease(rctx, l.runID, l.d.terms.owner, at.Add(ttl))
			rcancel()
			switch {
			case err != nil:
				// Retried at the next tick; ok fails once the expiry passes.
			case !held:
				l.lost.Store(true)
				return
			default:
				l.until.Store(at.Add(ttl).UnixNano())
			}
		}
	}()
}

// ok reports that the lease is still held: no renewal has found another
// owner and the last expiry written has not passed.
func (l *lease) ok() bool {
	return l == nil || !l.lost.Load() && l.d.wall().UnixNano() < l.until.Load()
}

func (l *lease) err() error {
	return fmt.Errorf("%w: run %s", ErrLeaseLost, l.runID)
}

// stop ends the heartbeat and waits for it. Unless the call left the run
// paused or finished, which released the lease in the same transaction,
// the lease is released so another process need not wait for it to
// expire; the release is compare-and-set on the owner.
func (l *lease) stop(out Run, err error) {
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.done
	if err == nil && (out.Status == StatusWaitingForApproval || out.Status.Terminal()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	l.d.store.releaseLease(ctx, l.runID, l.d.terms.owner)
}
