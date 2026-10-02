package agentrt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// wallClock is a wall clock a test moves by hand. The heartbeat reads it
// from its own goroutine.
type wallClock struct {
	mu sync.Mutex
	at time.Time
}

func newWallClock() *wallClock { return &wallClock{at: time.Now()} }

func (w *wallClock) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.at
}

func (w *wallClock) advance(d time.Duration) {
	w.mu.Lock()
	w.at = w.at.Add(d)
	w.mu.Unlock()
}

// process is one process's store and driver on a shared database file.
type process struct {
	store *Store
	d     *Driver
}

func openProcess(t *testing.T, path string, cfg Config) process {
	t.Helper()
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg.Store = st
	if cfg.Policy == nil {
		cfg.Policy = DefaultPolicy()
	}
	d, err := NewDriver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return process{store: st, d: d}
}

// gate is a tool that signals when it is called and returns when released.
type gate struct {
	spec    ToolSpec
	calls   atomic.Int32
	entered chan string
	release chan struct{}
	during  func(ToolCall)
}

func newGate(name string) *gate {
	return &gate{spec: ToolSpec{Name: name, Description: name, InputSchema: []byte(`{"type":"object"}`), SideEffect: LocalMutation, Timeout: time.Minute},
		entered: make(chan string, 8), release: make(chan struct{})}
}

func (g *gate) Spec() ToolSpec { return g.spec }
func (g *gate) Call(ctx context.Context, c ToolCall) (ToolResult, error) {
	g.calls.Add(1)
	g.entered <- c.RunID
	if g.during != nil {
		g.during(c)
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
	return ToolResult{Content: []byte(`{"done":true}`)}, nil
}

type result struct {
	run Run
	err error
}

// snapshot is every stored row of a run, for checking that nothing changed.
func snapshot(t *testing.T, st *Store, runID string) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{`SELECT * FROM runs WHERE id = ?`, `SELECT * FROM steps WHERE run_id = ? ORDER BY rowid`, `SELECT * FROM events WHERE run_id = ? ORDER BY seq`, `SELECT * FROM approvals WHERE run_id = ? ORDER BY rowid`} {
		rows, err := st.DB().Query(q, runID)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&out, vals...)
		}
		rows.Close()
	}
	return out.String()
}

func storedLease(t *testing.T, st *Store, runID string) (string, time.Time) {
	t.Helper()
	owner, until, err := st.lease(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return owner, until
}

func leaseLimits() Limits {
	return Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 5}
}

// Two processes on one file: while the first executes a run, the second's
// Resume is refused with the owner and expiry, reads no clock, and
// changes nothing; the first then finishes as if nothing had happened.
func TestLease_ResumeOfALiveRunIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideComplete, Result: []byte(`{}`)}}}
	p1 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}})
	reads := 0
	p2 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, Now: func() time.Time { reads++; return time.Now() }})
	ctx := context.Background()
	done := make(chan result, 1)
	go func() { r, err := p1.d.Start(ctx, "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered

	before := snapshot(t, p2.store, runID)
	_, err := p2.d.Resume(ctx, runID)
	var leased ErrRunLeased
	if !errors.As(err, &leased) || !errors.Is(err, ErrRunState) {
		t.Fatalf("second resume: %v, want ErrRunLeased", err)
	}
	if leased.RunID != runID || leased.Owner != p1.d.terms.owner || !leased.ExpiresAt.After(time.Now()) {
		t.Fatalf("leased = %+v, want owner %s with a live expiry", leased, p1.d.terms.owner)
	}
	if after := snapshot(t, p2.store, runID); after != before {
		t.Fatalf("a refused resume changed the run:\n before %s\n after  %s", before, after)
	}
	if reads != 0 {
		t.Fatalf("a refused resume read the clock %d times", reads)
	}

	close(work.release)
	r := <-done
	if r.err != nil || r.run.Status != StatusCompleted {
		t.Fatalf("first process: %v, %s %s", r.err, r.run.Status, r.run.ReasonDetail)
	}
	if n := work.calls.Load(); n != 1 {
		t.Fatalf("tool called %d times", n)
	}
	if owner, _ := storedLease(t, p1.store, runID); owner != "" {
		t.Fatalf("a finished run still leased by %q", owner)
	}
}

// The first process dies holding the lease: its heartbeat stops and the
// lease is never released. Until the TTL passes the second is refused;
// then it takes the run over as interrupted, records the takeover,
// reconciles the step once, and finishes. The dead process's tool, which
// cannot be recalled, returns late and its outcome is not recorded: the
// step belongs to the new owner's reconciliation.
func TestLease_ExpiredLeaseIsTakenOverAndReconciledOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	wall := newWallClock()
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideComplete, Result: []byte(`{"ok":true}`)}}}
	p1 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: 10 * time.Second})
	p1.d.wall, p1.d.noHeartbeat = wall.Now, true
	var reconciled []RunView
	p2 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: 10 * time.Second,
		Reconcile: func(_ context.Context, v RunView) (Reconciliation, error) {
			reconciled = append(reconciled, v)
			return Reconciliation{Outcome: ReconcileContinue}, nil
		}})
	p2.d.wall = wall.Now
	ctx := context.Background()
	done := make(chan result, 1)
	go func() { r, err := p1.d.Start(ctx, "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered

	wall.advance(9 * time.Second)
	if _, err := p2.d.Resume(ctx, runID); !errors.As(err, new(ErrRunLeased)) {
		t.Fatalf("resume before the TTL passed: %v", err)
	}
	wall.advance(2 * time.Second)
	got, err := p2.d.Resume(ctx, runID)
	if err != nil || got.Status != StatusCompleted || string(got.Result) != `{"ok":true}` {
		t.Fatalf("takeover: %v, %s %s", err, got.Status, got.ReasonDetail)
	}
	if len(reconciled) != 1 || len(reconciled[0].Steps) != 1 || reconciled[0].Steps[0].Status != StepInterrupted {
		t.Fatalf("reconciled %d times: %+v", len(reconciled), reconciled)
	}
	events, _ := p2.store.ListEvents(ctx, runID)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := "run.created run.started step.started step.decided step.policy step.tool_started lease.taken_over step.interrupted run.resumed step.started step.decided run.finished"
	if strings.Join(types, " ") != want {
		t.Fatalf("events:\n %s\nwant\n %s", strings.Join(types, " "), want)
	}
	for _, e := range events {
		if e.Type == EventLeaseTakenOver && (!strings.Contains(string(e.Payload), p1.d.terms.owner) || !strings.Contains(string(e.Payload), p2.d.terms.owner)) {
			t.Fatalf("takeover payload = %s", e.Payload)
		}
	}

	close(work.release)
	r := <-done
	if !errors.Is(r.err, ErrLeaseLost) {
		t.Fatalf("dead process returned %v, %s", r.err, r.run.Status)
	}
	steps, _ := p2.store.ListSteps(ctx, runID)
	if steps[0].Status != StepInterrupted || steps[0].Observation.Kind != ObserveInterrupted {
		t.Fatalf("the late outcome overwrote the interrupted step: %+v", steps[0])
	}
	if n := work.calls.Load(); n != 1 {
		t.Fatalf("tool called %d times", n)
	}
}

// A loop whose lease was taken finds out through its heartbeat and its
// next write: the outcome of the tool in flight is not recorded, and no
// further tool is called.
func TestLease_LoopThatLosesItsLeaseStopsBeforeTheNextToolCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideToolCall, Tool: "work", Args: []byte(`{"n":2}`)}, {Kind: DecideComplete}}}
	p := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: 5 * time.Second})
	p.d.renewEvery = 25 * time.Millisecond
	other := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}})
	work.during = func(c ToolCall) {
		// Another owner takes the lease; the heartbeat notices.
		if _, err := other.store.DB().Exec(`UPDATE runs SET lease_owner = 'other', lease_expires_at = ? WHERE id = ?`, formatTime(time.Now().Add(time.Hour)), c.RunID); err != nil {
			t.Error(err)
		}
		time.Sleep(600 * time.Millisecond)
	}
	close(work.release)
	run, err := p.d.Start(context.Background(), "g", leaseLimits())
	if !errors.Is(err, ErrLeaseLost) || !errors.As(err, new(ErrRunLeased)) {
		t.Fatalf("Start = %s, %v; want ErrLeaseLost", run.Status, err)
	}
	if n := work.calls.Load(); n != 1 {
		t.Fatalf("tool called %d times after the lease was lost", n)
	}
	runID := <-work.entered
	steps, _ := other.store.ListSteps(context.Background(), runID)
	if len(steps) != 1 || steps[0].Status != StepExecuting || steps[0].Observation != nil {
		t.Fatalf("steps = %+v, want the step left executing for the new owner", steps)
	}
	if owner, _ := storedLease(t, other.store, runID); owner != "other" {
		t.Fatalf("the loser released a lease it did not hold: owner %q", owner)
	}
}

// A lease that expires unrenewed, with no one taking it, is lost too: the
// tool in flight is recorded, because the loop still owns the run, but no
// further tool starts. The lease is released, so the next Resume takes the
// run at once and carries on.
func TestLease_ExpiredLeaseStopsTheLoopBeforeItsNextSideEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	wall := newWallClock()
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideToolCall, Tool: "work", Args: []byte(`{"n":2}`)}, {Kind: DecideComplete}}}
	p := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: time.Second})
	p.d.wall, p.d.noHeartbeat = wall.Now, true
	work.during = func(ToolCall) { wall.advance(2 * time.Second) }
	close(work.release)
	ctx := context.Background()
	_, err := p.d.Start(ctx, "g", leaseLimits())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Start: %v, want ErrLeaseLost", err)
	}
	runID := <-work.entered
	steps, _ := p.store.ListSteps(ctx, runID)
	if n := work.calls.Load(); n != 1 || len(steps) != 1 || steps[0].Status != StepDone {
		t.Fatalf("calls %d, steps %+v", n, steps)
	}
	if owner, _ := storedLease(t, p.store, runID); owner != "" {
		t.Fatalf("lease not released: %q", owner)
	}
	next := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}})
	got, err := next.d.Resume(ctx, runID)
	if err != nil || got.Status != StatusCompleted || work.calls.Load() != 2 {
		t.Fatalf("resume: %v, %s, %d calls", err, got.Status, work.calls.Load())
	}
	events, _ := next.store.ListEvents(ctx, runID)
	for _, e := range events {
		if e.Type == EventLeaseTakenOver {
			t.Fatal("a released lease was recorded as taken over")
		}
	}
}

type countingModel struct{ calls atomic.Int32 }

func (m *countingModel) Name() string { return "m" }
func (m *countingModel) Generate(context.Context, ModelRequest) (ModelResponse, error) {
	m.calls.Add(1)
	return ModelResponse{Text: "hi"}, nil
}

type modelThenComplete struct{ before func() }

func (a modelThenComplete) Decide(ctx context.Context, in StepInput) (Decision, error) {
	a.before()
	if _, err := in.Model.Generate(ctx, ModelRequest{MaxOutputTokens: 10}); err != nil {
		return Decision{}, err
	}
	return Decision{Kind: DecideComplete}, nil
}

// A model request is a side effect too: once the lease is lost none is
// dispatched, and the run is not failed by a loop that no longer owns it.
func TestLease_NoModelCallAfterTheLeaseIsLost(t *testing.T) {
	wall := newWallClock()
	m := &countingModel{}
	p := openProcess(t, filepath.Join(t.TempDir(), "l.db"), Config{Agent: modelThenComplete{before: func() { wall.advance(time.Minute) }}, Model: &ModelConfig{Model: m}})
	p.d.wall, p.d.noHeartbeat = wall.Now, true
	run, err := p.d.Start(context.Background(), "g", leaseLimits())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Start = %s, %v; want ErrLeaseLost", run.Status, err)
	}
	runs, _ := p.store.ListRuns(context.Background())
	calls, _ := p.store.ListModelCalls(context.Background(), runs[0].ID)
	if m.calls.Load() != 0 || len(calls) != 0 || runs[0].Status != StatusRunning {
		t.Fatalf("model calls %d, recorded %d, run %s", m.calls.Load(), len(calls), runs[0].Status)
	}
}

// An operator's Cancel needs no lease: it ends a run another process is
// executing, and that process stops at its next write.
func TestLease_CancelNeedsNoLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	p := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}})
	operator := openProcess(t, path, Config{Agent: agent})
	done := make(chan result, 1)
	go func() { r, err := p.d.Start(context.Background(), "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered
	if err := Cancel(context.Background(), operator.store, nil, runID, "joey", "stop"); err != nil {
		t.Fatal(err)
	}
	if owner, _ := storedLease(t, operator.store, runID); owner != "" {
		t.Fatalf("a cancelled run is still leased by %q", owner)
	}
	close(work.release)
	r := <-done
	if r.err != nil || r.run.Status != StatusCancelled || r.run.Reason != ReasonOperatorCancelled {
		t.Fatalf("loop returned %s/%s, %v", r.run.Status, r.run.Reason, r.err)
	}
}

// A tool call that lasts several TTLs keeps the lease: the heartbeat does
// not wait for the loop.
func TestLease_LongToolCallKeepsTheLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	const ttl = time.Second
	work := newGate("work")
	agent := &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "work", Args: []byte(`{}`)}, {Kind: DecideComplete}}}
	p1 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: ttl})
	p2 := openProcess(t, path, Config{Agent: agent, Tools: []Tool{work}, LeaseTTL: ttl})
	p1.d.renewEvery = 50 * time.Millisecond
	done := make(chan result, 1)
	go func() { r, err := p1.d.Start(context.Background(), "g", leaseLimits()); done <- result{r, err} }()
	runID := <-work.entered
	for range 3 {
		time.Sleep(ttl)
		if _, err := p2.d.Resume(context.Background(), runID); !errors.As(err, new(ErrRunLeased)) {
			t.Fatalf("resume during a long tool call: %v", err)
		}
	}
	close(work.release)
	if r := <-done; r.err != nil || r.run.Status != StatusCompleted {
		t.Fatalf("long run: %v, %s %s", r.err, r.run.Status, r.run.ReasonDetail)
	}
}

// settled waits for the goroutine count to fall back to at most n.
func settled(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > n {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("%d goroutines, want %d:\n%s", runtime.NumGoroutine(), n, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type panicAgent struct{}

func (panicAgent) Decide(context.Context, StepInput) (Decision, error) { panic("agent bug") }

// The heartbeat ends when the call returns, normally or by a panic, and a
// panicking loop releases its lease.
func TestLease_HeartbeatStopsOnReturnAndOnPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	slow := &gate{spec: ToolSpec{Name: "slow", Description: "s", InputSchema: []byte(`{"type":"object"}`), SideEffect: ReadOnly, Timeout: time.Second},
		entered: make(chan string, 1), release: make(chan struct{})}
	// Several renewals during the call. The lease itself is long, so a
	// stalled runner cannot let it lapse.
	slow.during = func(ToolCall) { time.Sleep(200 * time.Millisecond) }
	close(slow.release)
	p := openProcess(t, path, Config{Agent: &listAgent{decisions: []Decision{{Kind: DecideToolCall, Tool: "slow", Args: []byte(`{}`)}, {Kind: DecideComplete}}}, Tools: []Tool{slow}, LeaseTTL: 5 * time.Second})
	q := openProcess(t, path, Config{Agent: panicAgent{}, LeaseTTL: 5 * time.Second})
	p.d.renewEvery, q.d.renewEvery = 25*time.Millisecond, 25*time.Millisecond
	base := runtime.NumGoroutine()
	for range 3 {
		if run, err := p.d.Start(context.Background(), "g", leaseLimits()); err != nil || run.Status != StatusCompleted {
			t.Fatalf("run: %v %s", err, run.Status)
		}
		<-slow.entered
	}
	settled(t, base)
	for range 3 {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("the agent's panic did not propagate")
				}
			}()
			q.d.Start(context.Background(), "g", leaseLimits())
		}()
	}
	settled(t, base)
	runs, _ := q.store.ListRuns(context.Background())
	for _, r := range runs {
		if owner, _ := storedLease(t, q.store, r.ID); owner != "" {
			t.Fatalf("run %s (%s) still leased by %q after its loop panicked", r.ID, r.Status, owner)
		}
	}
}

// v021 is a database as v0.2.1 wrote it: the first four migrations, with a
// run found RUNNING mid-step and a finished one.
func v021(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`}
	for i, m := range migrations[:4] {
		stmts = append(stmts, m, fmt.Sprintf(`INSERT INTO schema_migrations VALUES (%d, '2026-09-28T00:00:00Z')`, i+1))
	}
	stmts = append(stmts,
		`INSERT INTO runs (id, goal, status, limits_json, step_count, created_at, started_at, active_ms) VALUES ('crashed', 'g', 'RUNNING', '{"max_steps":10,"max_consecutive_tool_failures":3,"loop_threshold":5}', 1, '2026-09-28T00:00:00Z', '2026-09-28T00:00:00Z', 1500)`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, started_at) VALUES ('s0', 'crashed', 0, 'executing', '{"kind":"tool_call","tool":"work","args":{}}', '{"outcome":"allow"}', '2026-09-28T00:00:00Z')`,
		`INSERT INTO runs (id, goal, status, reason, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens) VALUES ('done', 'g', 'COMPLETED', 'goal_completed', '{"max_steps":10,"max_consecutive_tool_failures":3,"loop_threshold":5}', 2, '2026-09-28T00:00:00Z', '2026-09-28T00:00:00Z', '2026-09-28T00:01:00Z', '{"ok":true}', 3, 1200)`,
	)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// The first four migrations are what v0.2.1 applied; a migration is never
// edited once released, only followed by another.
func TestStore_ReleasedMigrationsAreUnchanged(t *testing.T) {
	sum := sha256.Sum256([]byte(strings.Join(migrations[:4], "\x00")))
	if got := hex.EncodeToString(sum[:]); got != "6ea2e8d7079464c92debd7aea61e380770248ad161b796e9d86fa5b449b7869b" {
		t.Fatalf("the v0.2.1 migrations changed: %s", got)
	}
}

// A database written by v0.2.1 opens and migrates with its runs intact.
// Its RUNNING run may still be executing in a v0.2.1 process, which knows
// nothing of leases, so the migration stamps it with a lease one TTL long:
// Resume is refused until that passes and then takes the run over, with
// a takeover event naming the older version. OpenExisting refuses the
// database before the migration and opens it after.
func TestStore_V021DatabaseMigratesWithItsRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v021.db")
	v021(t, path)
	for _, ro := range []bool{true, false} {
		if _, err := OpenExisting(path, ro); !errors.Is(err, ErrSchemaVersion) {
			t.Fatalf("OpenExisting(readOnly=%v) of a v0.2.1 database: %v", ro, err)
		}
	}
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var version int
	st.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if version != 5 || len(migrations) != 5 {
		t.Fatalf("version %d of %d", version, len(migrations))
	}
	ctx := context.Background()
	done, err := st.GetRun(ctx, "done")
	if err != nil || done.Status != StatusCompleted || string(done.Result) != `{"ok":true}` || done.ModelCalls != 3 || done.Usage.InputTokens != 1200 || done.StepCount != 2 {
		t.Fatalf("finished run: %+v %v", done, err)
	}
	crashed, err := st.GetRun(ctx, "crashed")
	if err != nil || crashed.Status != StatusRunning || crashed.ActiveTime != 1500*time.Millisecond {
		t.Fatalf("interrupted run: %+v %v", crashed, err)
	}
	migrated := time.Now()
	if owner, until := storedLease(t, st, "crashed"); owner != preLeaseOwner || until.Before(migrated.Add(DefaultLeaseTTL-5*time.Second)) || until.After(migrated.Add(DefaultLeaseTTL)) {
		t.Fatalf("lease %q %v on a migrated run, want %q about %s from now", owner, until, preLeaseOwner, DefaultLeaseTTL)
	}
	if owner, _ := storedLease(t, st, "done"); owner != "" {
		t.Fatalf("a finished run was leased by %q", owner)
	}
	if ro, err := OpenExisting(path, true); err != nil {
		t.Fatalf("OpenExisting after the migration: %v", err)
	} else {
		ro.Close()
	}
	work := &stubTool{spec: ToolSpec{Name: "work", Description: "w", InputSchema: []byte(`{"type":"object"}`), SideEffect: LocalMutation, Timeout: time.Second}}
	d, err := NewDriver(Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Policy: DefaultPolicy(), Tools: []Tool{work},
		Reconcile: func(context.Context, RunView) (Reconciliation, error) {
			return Reconciliation{Outcome: ReconcileContinue}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	var leased ErrRunLeased
	if _, err := d.Resume(ctx, "crashed"); !errors.As(err, &leased) || leased.Owner != preLeaseOwner {
		t.Fatalf("resume within the grace lease: %v", err)
	}
	d.wall = func() time.Time { return migrated.Add(DefaultLeaseTTL + time.Second) }
	got, err := d.Resume(ctx, "crashed")
	if err != nil || got.Status != StatusCompleted || work.calls != 0 {
		t.Fatalf("resume: %v %s, %d calls", err, got.Status, work.calls)
	}
	events, _ := st.ListEvents(ctx, "crashed")
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	if want := "lease.taken_over step.interrupted run.resumed step.started step.decided run.finished"; strings.Join(types, " ") != want {
		t.Fatalf("events %v, want %s", types, want)
	}
	if !strings.Contains(string(events[0].Payload), preLeaseOwner) {
		t.Fatalf("takeover payload = %s", events[0].Payload)
	}
}

// A newer database is refused by both openers, and so is an older one by
// OpenExisting, which never migrates.
func TestOpenExisting_RefusesEitherOtherVersion(t *testing.T) {
	older := filepath.Join(t.TempDir(), "old.db")
	v021(t, older)
	if _, err := OpenExisting(older, false); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("older: %v", err)
	}
	newer := filepath.Join(t.TempDir(), "new.db")
	st, err := OpenStore(newer)
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`INSERT INTO schema_migrations VALUES (?, '')`, len(migrations)+1)
	st.Close()
	if _, err := OpenExisting(newer, false); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("newer: %v", err)
	}
	if _, err := OpenStore(newer); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("newer, OpenStore: %v", err)
	}
}
