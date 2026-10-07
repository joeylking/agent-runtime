// Package export follows the events table of a Store and hands every
// committed event, in commit order, to a sink: the JSON Lines writer here,
// or the OpenTelemetry span exporter in the nested module export/otel.
//
// Events are written in the transaction that makes the state change they
// explain, so the table is complete and a follower needs no other input
// (docs/architecture.md, ADR 2). The event's Seq is its cursor: SQLite
// assigns it from an AUTOINCREMENT key inside the single write transaction
// that commits the event, so it is strictly increasing in commit order
// across every run in a database and never reused. A follower that
// remembers the last Seq it delivered resumes exactly after it.
//
// A Follower only reads. Opened on a Store from agentrt.OpenExisting with
// readOnly set, it can run as a separate process beside the consumer that
// owns the database, which needs no code for it. It reads a bounded page
// at a time through agentrt.Store.ListEventsAfter, with every text column
// capped at agentrt.MaxPageText, so a huge table, or one crafted row,
// cannot exhaust the follower's memory.
package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/trace"
)

const (
	// DefaultInterval is how long a Follower waits between polls when it
	// has delivered everything committed so far and Interval is zero.
	DefaultInterval = 250 * time.Millisecond
	// MaxInterval bounds Interval: a follower never waits longer than this
	// between polls, so an event is never more than a minute from its sink.
	MaxInterval = time.Minute
	// DefaultPageSize is how many events a Follower reads per query when
	// PageSize is zero.
	DefaultPageSize = 500
	// MaxPageSize bounds PageSize, so one page holds at most this many
	// payloads of up to agentrt.MaxPageText each.
	MaxPageSize = 10_000
)

// Sink receives one event. A non-nil error stops the Follower, which
// returns it; the cursor then names the last event delivered before it, so
// a restart resumes with the event that failed.
type Sink func(agentrt.Event) error

// Cursor persists the Seq of the last event a Follower delivered. Load
// returns zero when nothing was saved yet.
type Cursor interface {
	Load(ctx context.Context) (int64, error)
	Save(ctx context.Context, seq int64) error
}

// FileCursor keeps the cursor in a file holding the Seq in decimal, written
// to a temporary file in the same directory and renamed over the old one,
// so a crash leaves the previous cursor rather than a torn file. A missing
// file loads as zero.
type FileCursor struct {
	Path string
}

// Load implements Cursor.
func (c FileCursor) Load(context.Context) (int64, error) {
	b, err := os.ReadFile(c.Path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("export: load cursor: %w", err)
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("export: load cursor: %s does not hold a sequence number", c.Path)
	}
	return seq, nil
}

// Save implements Cursor.
func (c FileCursor) Save(_ context.Context, seq int64) error {
	dir, base := filepath.Split(c.Path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".*")
	if err != nil {
		return fmt.Errorf("export: save cursor: %w", err)
	}
	tmp := f.Name()
	_, werr := fmt.Fprintf(f, "%d\n", seq)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("export: save cursor: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("export: save cursor: %w", err)
	}
	if err := os.Rename(tmp, c.Path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("export: save cursor: %w", err)
	}
	return nil
}

// MemCursor is a Cursor in memory, for a follower that starts over each
// time it runs or whose caller persists Seq itself.
type MemCursor struct {
	Seq int64
}

// Load implements Cursor.
func (c *MemCursor) Load(context.Context) (int64, error) { return c.Seq, nil }

// Save implements Cursor.
func (c *MemCursor) Save(_ context.Context, seq int64) error {
	c.Seq = seq
	return nil
}

// Follower delivers a Store's events in commit order, each once per cursor
// position. The zero value is not usable: Store must be set.
type Follower struct {
	// Store is read and never written. agentrt.OpenExisting(path, true)
	// opens one that cannot write.
	Store *agentrt.Store
	// RunID restricts delivery to one run. Empty delivers every run's
	// events interleaved in commit order.
	RunID string
	// Cursor is where the follower resumes from and saves to. Nil starts
	// at the first event and remembers nothing beyond this call.
	Cursor Cursor
	// Interval is the wait between polls once everything committed has
	// been delivered: DefaultInterval when zero, never more than
	// MaxInterval.
	Interval time.Duration
	// PageSize is how many events one query reads: DefaultPageSize when
	// zero, never more than MaxPageSize.
	PageSize int
	// Once makes Follow return nil as soon as it has delivered every event
	// committed when it last looked, instead of polling for more.
	Once bool
}

// Follow reads events after the cursor and calls sink for each, in Seq
// order, until ctx is done, sink fails, or, with Once, nothing more is
// committed. Follow returns ctx.Err() when ctx ends it.
//
// The cursor is saved after each page and before Follow returns, so
// delivery is exactly once across stops that return: a follower stopped
// by its context, by a sink error, or by Once, and started again on the
// same cursor, delivers each event once (the event a sink refused is
// delivered again, since it was never delivered). It is at least once
// after a hard kill: a process that dies between a sink call and the save
// that follows it, or whose save fails, starts again from the cursor last
// saved and delivers again what it delivered since, at most one page of
// PageSize events. A sink that must not act twice keys on Seq.
func (f *Follower) Follow(ctx context.Context, sink Sink) error {
	if f.Store == nil {
		return errors.New("export: Follower.Store is nil")
	}
	if sink == nil {
		return errors.New("export: sink is nil")
	}
	interval := f.Interval
	switch {
	case interval < 0:
		return fmt.Errorf("export: interval %v is negative", interval)
	case interval == 0:
		interval = DefaultInterval
	case interval > MaxInterval:
		interval = MaxInterval
	}
	size := f.PageSize
	switch {
	case size < 0:
		return fmt.Errorf("export: page size %d is negative", size)
	case size == 0:
		size = DefaultPageSize
	case size > MaxPageSize:
		size = MaxPageSize
	}
	cursor := f.Cursor
	if cursor == nil {
		cursor = &MemCursor{}
	}
	seq, err := cursor.Load(ctx)
	if err != nil {
		return err
	}
	if seq < 0 {
		return fmt.Errorf("export: cursor %d is negative", seq)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		page, err := f.page(ctx, seq, size)
		if err != nil {
			return err
		}
		delivered := false
		for _, e := range page {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, saveIf(ctx, cursor, seq, delivered))
			}
			if err := sink(e); err != nil {
				return errors.Join(err, saveIf(ctx, cursor, seq, delivered))
			}
			seq, delivered = e.Seq, true
		}
		if err := saveIf(ctx, cursor, seq, delivered); err != nil {
			return err
		}
		if len(page) == size {
			continue
		}
		if f.Once {
			return nil
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// saveIf saves seq when something was delivered since the last save. It
// saves with a background context so a cancelled ctx still records what
// was delivered under it.
func saveIf(ctx context.Context, c Cursor, seq int64, delivered bool) error {
	if !delivered {
		return nil
	}
	return c.Save(context.WithoutCancel(ctx), seq)
}

// Head returns the Seq the follower has delivered up to, as its Cursor
// saved it, and the stored hash of that event: the head of the event chain
// as far as the follower delivered it, cut at 128 characters as
// agentrt.Store.EventHash reads it. An operator keeps the pair where
// whoever can write the database cannot reach, beside what the sink wrote,
// and later checks it with agentrt verify -head SEQ:HASH, which fails
// unless the walk from the first event reaches Seq with no break and the
// event at Seq still has that hash. In code the check is the same three
// parts: agentrt.Store.VerifyEvents from the first event to at least Seq
// with no Break, a To of at least Seq (a database cut before Seq verifies
// intact up to where it ends), and the event at Seq's hash, the report's
// Hash when To is Seq or EventHash otherwise, equal to the one kept. A
// chain that passes was not rewritten up to Seq. The hash covers every
// run's events up to Seq, a follower of one run included, because the
// chain is the whole database's. Head is zero and empty when nothing was delivered, or the
// Cursor is nil and so remembers nothing; agentrt.ErrNotFound means the
// event the cursor names is no longer stored. It reads the cursor and one
// row and changes nothing, and may be called while Follow runs, with a
// Cursor safe for that, as FileCursor is.
func (f *Follower) Head(ctx context.Context) (seq int64, hash string, err error) {
	if f.Store == nil {
		return 0, "", errors.New("export: Follower.Store is nil")
	}
	if f.Cursor == nil {
		return 0, "", nil
	}
	if seq, err = f.Cursor.Load(ctx); err != nil || seq <= 0 {
		return 0, "", err
	}
	if hash, err = f.Store.EventHash(ctx, seq); err != nil {
		return 0, "", fmt.Errorf("export: head at seq %d: %w", seq, err)
	}
	return seq, hash, nil
}

// page reads at most size events after seq, in Seq order, for the
// follower's run or every run, through agentrt.Store.ListEventsAfter: one
// bounded query, closed before anything is delivered, so the store's
// connection is never held across a sink call.
func (f *Follower) page(ctx context.Context, seq int64, size int) ([]agentrt.Event, error) {
	page, err := f.Store.ListEventsAfter(ctx, f.RunID, seq, size)
	if err != nil {
		return nil, fmt.Errorf("export: read events: %w", err)
	}
	return page, nil
}

// JSONL returns a Sink writing one JSON object per event to w, the record
// trace.JSONL writes: seq, run_id, step_id, at, type, and the payload as
// recorded. It is trace.JSONL behind a Sink, so the two never differ. A
// write error stops the follower.
func JSONL(w io.Writer) Sink {
	obs, errFn := trace.JSONLWithErr(w)
	return func(e agentrt.Event) error {
		obs(e)
		return errFn()
	}
}
