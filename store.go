package agentrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// Store persists runs, steps, and events in SQLite. It is the single
// implementation; there is no store interface until a second one is needed.
//
// A Store opened on ":memory:" lives for the life of the connection and is
// meant for unit tests. A file-backed store uses WAL mode and survives
// process restarts.
type Store struct {
	db *sql.DB
}

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("agentrt: not found")

// ErrRunState means a run, or one of its steps, is not in the state the
// operation requires: a second Resume lost the race to the first, a live
// loop found the run cancelled under it, or an operator acted on a run
// that is not waiting. Status transitions are compare-and-set inside the
// transaction that makes them, so exactly one caller wins.
var ErrRunState = errors.New("agentrt: run is not in the required state")

// ErrNotPending means an approval has already been decided or expired.
var ErrNotPending = errors.New("agentrt: approval is not pending")

// ErrSchemaVersion means a database's schema is not the one this build of
// the runtime writes. OpenExisting returns it rather than migrating, and
// OpenStore returns it for a database a newer build has migrated.
var ErrSchemaVersion = errors.New("agentrt: database schema version does not match this build")

// dsn builds a modernc.org/sqlite URI for a file path. The driver splits
// the DSN at its first '?', and SQLite decodes %HH in the path and ends it
// at '#', so those three characters are escaped. Every transaction begins
// IMMEDIATE: it takes the write lock up front and waits on busy_timeout,
// rather than failing when a read-then-write transaction cannot upgrade.
func dsn(path string, q url.Values) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	prefix := "file:"
	if strings.HasPrefix(escaped, "/") {
		prefix = "file://" // an empty authority, so a path starting "//" stays a path
	}
	return prefix + escaped + "?" + q.Encode()
}

// OpenStore opens or creates the database at path and applies migrations.
// Concurrent first opens are safe: the version is read and the migrations
// applied inside one IMMEDIATE transaction. A database migrated by a newer
// build is refused with ErrSchemaVersion.
func OpenStore(path string) (*Store, error) {
	var name string
	if path == ":memory:" {
		name = "file::memory:?_pragma=foreign_keys(1)&_txlock=immediate"
	} else {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate")
		name = dsn(path, q)
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	// One connection: SQLite has a single writer and the in-memory database is
	// per connection.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db}
	if path != ":memory:" {
		if err := s.useWAL(context.Background()); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// useWAL switches the file to WAL mode, which then persists in the file.
// The switch needs an exclusive lock and SQLite returns SQLITE_BUSY for it
// without waiting on the busy timeout, so a concurrent first open retries
// it for as long as the timeout would have waited.
func (s *Store) useWAL(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var mode string
		err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				return fmt.Errorf("agentrt: open store: journal mode is %s, not wal", mode)
			}
			return nil
		}
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			return fmt.Errorf("agentrt: open store: journal mode: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const sqliteBusy = 5

// OpenExisting opens the database at path without creating or migrating it,
// which is what an operator's tool wants: a mistyped path is an error, not
// a new empty database, and a newer binary does not change the schema under
// the consumer that owns the file. readOnly opens it for reading only; a
// read-only store never writes to the file.
func OpenExisting(path string, readOnly bool) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("agentrt: open store: %s is not a regular file", path)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rw")
		q.Set("_txlock", "immediate")
	}
	s, err := openExisting(path, q)
	var se *sqlite.Error
	if readOnly && errors.As(err, &se) && se.Code() == sqliteReadonlyDirectory {
		// A WAL database with no -shm file in a directory this process
		// cannot write. No writer can be live either, because it would
		// have had to create the WAL files there, so the file is read as
		// immutable, which needs no shared-memory file.
		q.Set("immutable", "1")
		s, err = openExisting(path, q)
	}
	return s, err
}

const sqliteReadonlyDirectory = 1544 // SQLITE_READONLY_DIRECTORY

func openExisting(path string, q url.Values) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path, q))
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	var current int
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		db.Close()
		return nil, fmt.Errorf("agentrt: open store: %s is not an agent-runtime database: %w", path, err)
	}
	if current != len(migrations) {
		db.Close()
		return nil, fmt.Errorf("%w: %s is at version %d, this build writes %d", ErrSchemaVersion, path, current, len(migrations))
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying database for consumers that keep their own
// tables in the same file and for tests. The runtime's tables are its own.
//
// The handle shares the store's single connection. A transaction a
// consumer opens on it holds that connection until it commits or rolls
// back, so it must not call Store methods or the driver meanwhile. Write
// transactions begin IMMEDIATE and wait on the busy timeout for another
// process's writer rather than failing when they upgrade from a read.
func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	`CREATE TABLE runs (
		id TEXT PRIMARY KEY,
		goal TEXT NOT NULL,
		status TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		reason_detail TEXT NOT NULL DEFAULT '',
		limits_json TEXT NOT NULL,
		step_count INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		started_at TEXT NOT NULL DEFAULT '',
		finished_at TEXT NOT NULL DEFAULT '',
		result_json TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE steps (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id),
		idx INTEGER NOT NULL,
		status TEXT NOT NULL,
		decision_json TEXT NOT NULL DEFAULT '',
		policy_json TEXT NOT NULL DEFAULT '',
		observation_json TEXT NOT NULL DEFAULT '',
		observation_hash TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		finished_at TEXT NOT NULL DEFAULT '',
		UNIQUE(run_id, idx)
	);
	CREATE TABLE events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL REFERENCES runs(id),
		step_id TEXT NOT NULL DEFAULT '',
		at TEXT NOT NULL,
		type TEXT NOT NULL,
		payload_json TEXT NOT NULL
	);
	CREATE INDEX events_run ON events(run_id, seq);`,
	`CREATE TABLE approvals (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id),
		step_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		capability_json TEXT NOT NULL,
		presentation_json TEXT NOT NULL,
		request_json TEXT NOT NULL,
		hash TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		decided_at TEXT NOT NULL DEFAULT '',
		decided_by TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX approvals_run ON approvals(run_id, created_at);`,
	`CREATE TABLE model_calls (
		id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id),
		step_id TEXT NOT NULL,
		attempt INTEGER NOT NULL,
		status TEXT NOT NULL,
		model TEXT NOT NULL,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cached_input_tokens INTEGER NOT NULL DEFAULT 0,
		cost_micros INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		dispatched_at TEXT NOT NULL,
		completed_at TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX model_calls_run ON model_calls(run_id, dispatched_at);
	ALTER TABLE runs ADD COLUMN model_calls INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN cached_input_tokens INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN estimated_cost_micros INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE runs ADD COLUMN active_ms INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE approvals ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';`,
}

func (s *Store) migrate(ctx context.Context) (err error) {
	// One IMMEDIATE transaction reads the version and applies what is
	// missing, so a second process opening the same new file waits and
	// then finds the work done.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("agentrt: migrate: begin: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("agentrt: migrate: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("agentrt: migrate: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("%w: the database is at version %d, this build knows %d", ErrSchemaVersion, current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("agentrt: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, formatTime(time.Now())); err != nil {
			return fmt.Errorf("agentrt: migration %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agentrt: migrate: %w", err)
	}
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func marshalOpt(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case *Decision:
		if x == nil {
			return ""
		}
	case *PolicyDecision:
		if x == nil {
			return ""
		}
	case *Observation:
		if x == nil {
			return ""
		}
	case json.RawMessage:
		if len(x) == 0 {
			return ""
		}
		return string(x)
	}
	return string(toJSON(v))
}

// CreateRun inserts a new run. The caller is responsible for the run.created
// event; the driver writes it in the same transaction.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	return s.tx(ctx, nil, func(t *txn) error { return t.insertRun(ctx, r) })
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// ListRuns returns every run, newest first. Order is insertion order:
// stored timestamps drop trailing zeros and do not sort as text.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs ORDER BY rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (Run, error) {
	var r Run
	var limits, created, started, finished, result string
	var cost, activeMS int64
	err := sc.Scan(&r.ID, &r.Goal, &r.Status, &r.Reason, &r.ReasonDetail, &limits, &r.StepCount, &created, &started, &finished, &result, &r.ModelCalls, &r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CachedInputTokens, &cost, &activeMS)
	r.EstimatedCost = Micros(cost)
	r.ActiveTime = time.Duration(activeMS) * time.Millisecond
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if err := json.Unmarshal([]byte(limits), &r.Limits); err != nil {
		return Run{}, fmt.Errorf("agentrt: run %s limits: %w", r.ID, err)
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return Run{}, err
	}
	if r.StartedAt, err = parseTime(started); err != nil {
		return Run{}, err
	}
	if r.FinishedAt, err = parseTime(finished); err != nil {
		return Run{}, err
	}
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	return r, nil
}

// ListSteps returns the steps of a run in index order.
func (s *Store) ListSteps(ctx context.Context, runID string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, idx, status, decision_json, policy_json, observation_json, started_at, finished_at FROM steps WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		var decision, policy, observation, started, finished string
		if err := rows.Scan(&st.ID, &st.RunID, &st.Index, &st.Status, &decision, &policy, &observation, &started, &finished); err != nil {
			return nil, err
		}
		if decision != "" {
			st.Decision = new(Decision)
			if err := json.Unmarshal([]byte(decision), st.Decision); err != nil {
				return nil, err
			}
		}
		if policy != "" {
			st.Policy = new(PolicyDecision)
			if err := json.Unmarshal([]byte(policy), st.Policy); err != nil {
				return nil, err
			}
		}
		if observation != "" {
			st.Observation = new(Observation)
			if err := json.Unmarshal([]byte(observation), st.Observation); err != nil {
				return nil, err
			}
		}
		if st.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if st.FinishedAt, err = parseTime(finished); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListModelCalls returns a run's model call attempts in dispatch order.
func (s *Store) ListModelCalls(ctx context.Context, runID string) ([]ModelCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, attempt, status, model, input_tokens, output_tokens, cached_input_tokens, cost_micros, latency_ms, error, dispatched_at, completed_at FROM model_calls WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCall
	for rows.Next() {
		var c ModelCall
		var cost, latency int64
		var dispatched, completed string
		if err := rows.Scan(&c.ID, &c.RunID, &c.StepID, &c.Attempt, &c.Status, &c.Model, &c.Usage.InputTokens, &c.Usage.OutputTokens, &c.Usage.CachedInputTokens, &cost, &latency, &c.Error, &dispatched, &completed); err != nil {
			return nil, err
		}
		c.Cost, c.Latency = Micros(cost), time.Duration(latency)*time.Millisecond
		if c.DispatchedAt, err = parseTime(dispatched); err != nil {
			return nil, err
		}
		if c.CompletedAt, err = parseTime(completed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListApprovals returns a run's approvals in creation order.
func (s *Store) ListApprovals(ctx context.Context, runID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by, note, expires_at FROM approvals WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		var capability, presentation, request, created, decided, expires string
		if err := rows.Scan(&a.ID, &a.RunID, &a.StepID, &a.Kind, &capability, &presentation, &request, &a.Hash, &a.Status, &created, &decided, &a.DecidedBy, &a.Note, &expires); err != nil {
			return nil, err
		}
		if a.ExpiresAt, err = parseTime(expires); err != nil {
			return nil, err
		}
		a.Capability, a.Presentation = json.RawMessage(capability), json.RawMessage(presentation)
		if err := json.Unmarshal([]byte(request), &a.Request); err != nil {
			return nil, err
		}
		if a.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if a.DecidedAt, err = parseTime(decided); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApproval loads one approval.
func (s *Store) GetApproval(ctx context.Context, runID, id string) (Approval, error) {
	all, err := s.ListApprovals(ctx, runID)
	if err != nil {
		return Approval{}, err
	}
	for _, a := range all {
		if a.ID == id {
			return a, nil
		}
	}
	return Approval{}, ErrNotFound
}

// ListEvents returns the events of a run in sequence order.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, run_id, step_id, at, type, payload_json FROM events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at, payload string
		if err := rows.Scan(&e.Seq, &e.RunID, &e.StepID, &at, &e.Type, &payload); err != nil {
			return nil, err
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// txn is one write transaction. Events appended inside it are delivered to the
// observer only after commit.
type txn struct {
	tx     *sql.Tx
	events []Event
}

func (s *Store) tx(ctx context.Context, obs Observer, fn func(t *txn) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		// A panic inside fn must not leave the single connection inside an
		// open transaction; roll back and let the panic continue.
		if !done {
			tx.Rollback()
		}
	}()
	t := &txn{tx: tx}
	if err := fn(t); err != nil {
		done = true
		tx.Rollback()
		return err
	}
	done = true
	if err := tx.Commit(); err != nil {
		return err
	}
	if obs != nil {
		for _, e := range t.events {
			obs(e)
		}
	}
	return nil
}

func (t *txn) insertRun(ctx context.Context, r Run) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO runs (id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Goal, r.Status, r.Reason, r.ReasonDetail, string(toJSON(r.Limits)), r.StepCount, formatTime(r.CreatedAt), formatTime(r.StartedAt), formatTime(r.FinishedAt), marshalOpt(r.Result), r.ModelCalls, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CachedInputTokens, int64(r.EstimatedCost))
	return err
}

// loadRun reads a run inside the transaction, so what it returns is what
// the transaction's writes apply to, accounting totals included.
func (t *txn) loadRun(ctx context.Context, id string) (Run, error) {
	row := t.tx.QueryRowContext(ctx, `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// requireRun fails with ErrRunState unless the run is in one of from.
func (t *txn) requireRun(ctx context.Context, id string, from ...RunStatus) (Run, error) {
	r, err := t.loadRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	for _, f := range from {
		if r.Status == f {
			return r, nil
		}
	}
	return r, fmt.Errorf("%w: run %s is %s, not %s", ErrRunState, id, r.Status, statusList(from))
}

func statusList(ss []RunStatus) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return strings.Join(out, " or ")
}

// transition moves a run from one of from to r's status, reason, detail,
// finish time, and result, and fails with ErrRunState when another writer
// moved it first. It never writes step_count or the accounting columns,
// which have their own increments.
func (t *txn) transition(ctx context.Context, r Run, from ...RunStatus) error {
	if _, err := t.requireRun(ctx, r.ID, from...); err != nil {
		return err
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE runs SET status=?, reason=?, reason_detail=?, finished_at=?, result_json=? WHERE id=?`,
		r.Status, r.Reason, r.ReasonDetail, formatTime(r.FinishedAt), marshalOpt(r.Result), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: update run %s: %w", r.ID, ErrNotFound)
	}
	return nil
}

// claimStep advances step_count from index to index+1 on a RUNNING run.
// A second loop on the same run, or a run cancelled meanwhile, loses.
func (t *txn) claimStep(ctx context.Context, runID string, index int) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE runs SET step_count = step_count + 1 WHERE id=? AND status=? AND step_count=?`, runID, StatusRunning, index)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		r, err := t.loadRun(ctx, runID)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: run %s is %s at step %d, cannot start step %d", ErrRunState, runID, r.Status, r.StepCount, index)
	}
	return nil
}

func (t *txn) insertStep(ctx context.Context, st Step) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, observation_json, observation_hash, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		st.ID, st.RunID, st.Index, st.Status, marshalOpt(st.Decision), marshalOpt(st.Policy), marshalOpt(st.Observation), obsHash(st.Observation), formatTime(st.StartedAt), formatTime(st.FinishedAt))
	return err
}

// updateStep writes a step whose stored status is one of from, and fails
// with ErrRunState when it is not: another writer, such as Cancel, moved
// the step first.
func (t *txn) updateStep(ctx context.Context, st Step, from ...StepStatus) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, decision_json=?, policy_json=?, observation_json=?, observation_hash=?, started_at=?, finished_at=? WHERE id=?`+stepGuard(len(from)),
		append([]any{st.Status, marshalOpt(st.Decision), marshalOpt(st.Policy), marshalOpt(st.Observation), obsHash(st.Observation), formatTime(st.StartedAt), formatTime(st.FinishedAt), st.ID}, stepArgs(from)...)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		var status string
		if err := t.tx.QueryRowContext(ctx, `SELECT status FROM steps WHERE id=?`, st.ID).Scan(&status); err != nil {
			return fmt.Errorf("agentrt: update step %s: %w", st.ID, ErrNotFound)
		}
		return fmt.Errorf("%w: step %s is %s", ErrRunState, st.ID, status)
	}
	return nil
}

func stepGuard(n int) string {
	if n == 0 {
		return ""
	}
	return " AND status IN (?" + strings.Repeat(",?", n-1) + ")"
}

func stepArgs(from []StepStatus) []any {
	out := make([]any, len(from))
	for i, s := range from {
		out[i] = s
	}
	return out
}

func (t *txn) insertModelCall(ctx context.Context, c ModelCall) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO model_calls (id, run_id, step_id, attempt, status, model, dispatched_at) VALUES (?,?,?,?,?,?,?)`,
		c.ID, c.RunID, c.StepID, c.Attempt, c.Status, c.Model, formatTime(c.DispatchedAt))
	return err
}

func (t *txn) updateModelCall(ctx context.Context, c ModelCall) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE model_calls SET status=?, input_tokens=?, output_tokens=?, cached_input_tokens=?, cost_micros=?, latency_ms=?, error=?, completed_at=? WHERE id=?`,
		c.Status, c.Usage.InputTokens, c.Usage.OutputTokens, c.Usage.CachedInputTokens, int64(c.Cost), c.Latency.Milliseconds(), c.Error, formatTime(c.CompletedAt), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: update model call %s: %w", c.ID, ErrNotFound)
	}
	return nil
}

func (t *txn) addActiveTime(ctx context.Context, runID string, dur time.Duration) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE runs SET active_ms = active_ms + ? WHERE id=?`, dur.Milliseconds(), runID)
	return err
}

func (t *txn) addRunUsage(ctx context.Context, runID string, u Usage, cost Micros) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE runs SET model_calls = model_calls + 1, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cached_input_tokens = cached_input_tokens + ?, estimated_cost_micros = estimated_cost_micros + ? WHERE id=?`,
		u.InputTokens, u.OutputTokens, u.CachedInputTokens, int64(cost), runID)
	return err
}

func (t *txn) insertApproval(ctx context.Context, a Approval) error {
	req, err := json.Marshal(a.Request)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by, note, expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.RunID, a.StepID, a.Kind, string(orEmptyObject(a.Capability)), string(orEmptyObject(a.Presentation)), string(req), a.Hash, a.Status, formatTime(a.CreatedAt), formatTime(a.DecidedAt), a.DecidedBy, a.Note, formatTime(a.ExpiresAt))
	return err
}

func (t *txn) decideApproval(ctx context.Context, a Approval) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE approvals SET status=?, decided_at=?, decided_by=?, note=? WHERE id=? AND status=?`,
		a.Status, formatTime(a.DecidedAt), a.DecidedBy, a.Note, a.ID, ApprovalPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: approval %s", ErrNotPending, a.ID)
	}
	return nil
}

func obsHash(o *Observation) string {
	if o == nil {
		return ""
	}
	return o.ContentHash
}

func (t *txn) appendEvent(ctx context.Context, e Event) error {
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	row := t.tx.QueryRowContext(ctx, `INSERT INTO events (run_id, step_id, at, type, payload_json) VALUES (?,?,?,?,?) RETURNING seq`,
		e.RunID, e.StepID, formatTime(e.At), e.Type, string(e.Payload))
	if err := row.Scan(&e.Seq); err != nil {
		return err
	}
	t.events = append(t.events, e)
	return nil
}
