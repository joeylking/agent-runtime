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
	// stmts holds the loop's statements, prepared when a writer opens the
	// store: preparing is much of a short statement's cost in SQLite. It is
	// not written after open. A statement not in it is prepared per call.
	stmts map[string]*sql.Stmt
}

// ErrNotFound is returned when a run does not exist.
var ErrNotFound = errors.New("agentrt: not found")

// ErrRunState means a run, or one of its steps, is not in the state the
// operation requires: a second Resume lost the race to the first, a live
// loop found the run cancelled under it, or an operator acted on a run
// that is not waiting. Status transitions are compare-and-set inside the
// transaction that makes them, so exactly one caller wins.
var ErrRunState = errors.New("agentrt: run is not in the required state")

// ErrRunLeased is returned by Resume for a RUNNING run whose lease is held
// by another owner and has not expired: the run is being executed
// elsewhere, and Resume changes nothing. It matches ErrRunState with
// errors.Is. A loop finds it too when another owner took its run over.
type ErrRunLeased struct {
	RunID     string
	Owner     string
	ExpiresAt time.Time
}

func (e ErrRunLeased) Error() string {
	return fmt.Sprintf("agentrt: run %s is leased by %s until %s", e.RunID, e.Owner, e.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (e ErrRunLeased) Unwrap() error { return ErrRunState }

// ErrLeaseLost is returned by a Start or Resume whose loop lost its lease
// on the run: another owner took it over, or it expired unrenewed. The
// loop stops before its next side effect and leaves the step in flight to
// the next owner's reconciliation.
var ErrLeaseLost = errors.New("agentrt: run lease lost")

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
	if err := s.prepare(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// The statements the loop runs at every step.
const (
	sqlSelectRun     = `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs WHERE id = ?`
	sqlRunStatus     = `SELECT status, lease_owner = ? FROM runs WHERE id = ?`
	sqlClaimStep     = `UPDATE runs SET step_count = step_count + 1 WHERE id=? AND status=? AND step_count=? AND lease_owner=?`
	sqlInsertStep    = `INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, observation_json, observation_hash, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?)`
	sqlUpdateStep    = `UPDATE steps SET status=?, decision_json=?, policy_json=?, observation_json=?, observation_hash=?, started_at=?, finished_at=? WHERE id=?`
	sqlAddActiveTime = `UPDATE runs SET active_ms = active_ms + ? WHERE id=?`
	sqlAppendEvent   = `INSERT INTO events (run_id, step_id, at, type, payload_json) VALUES (?,?,?,?,?) RETURNING seq`
)

func (s *Store) prepare(ctx context.Context) error {
	s.stmts = map[string]*sql.Stmt{}
	for _, q := range []string{sqlSelectRun, sqlRunStatus, sqlClaimStep, sqlInsertStep, sqlUpdateStep + stepGuard(1), sqlAddActiveTime, sqlAppendEvent} {
		st, err := s.db.PrepareContext(ctx, q)
		if err != nil {
			s.closeStmts()
			return fmt.Errorf("agentrt: open store: prepare: %w", err)
		}
		s.stmts[q] = st
	}
	return nil
}

func (s *Store) closeStmts() {
	for _, st := range s.stmts {
		st.Close()
	}
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
func (s *Store) Close() error {
	s.closeStmts()
	return s.db.Close()
}

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
	// The lease on a RUNNING run. A run written before it has none, which
	// Resume treats as expired.
	`ALTER TABLE runs ADD COLUMN lease_owner TEXT NOT NULL DEFAULT '';
	ALTER TABLE runs ADD COLUMN lease_expires_at TEXT NOT NULL DEFAULT '';`,
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

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	if st := s.stmts[sqlSelectRun]; st != nil {
		return scanRun(st.QueryRowContext(ctx, id))
	}
	return scanRun(s.db.QueryRowContext(ctx, sqlSelectRun, id))
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
	rows, err := s.listStepRows(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []Step
	for i := range rows {
		st, err := rows[i].decode(nil, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Store) listStepRows(ctx context.Context, runID string) ([]stepRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, idx, status, decision_json, policy_json, observation_json, started_at, finished_at FROM steps WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var r stepRow
		if err := rows.Scan(&r.id, &r.runID, &r.index, &r.status, &r.decision, &r.policy, &r.observation, &r.started, &r.finished); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// stepRow is a step as stored: the columns written and read back.
type stepRow struct {
	id, runID                                              string
	index                                                  int
	status                                                 StepStatus
	decision, policy, observation, hash, started, finished string
	// src are the values the JSON columns were encoded from. The driver
	// never writes through a step's pointers, it assigns new ones, so a
	// later write of the same step reuses the text of any it kept.
	src struct {
		decision    *Decision
		policy      *PolicyDecision
		observation *Observation
	}
}

// encodeStep is st as stored. prev, the row last written for the same step,
// lends the text of the values st still points to.
func encodeStep(st Step, prev *stepRow) stepRow {
	r := stepRow{id: st.ID, runID: st.RunID, index: st.Index, status: st.Status, hash: obsHash(st.Observation), started: formatTime(st.StartedAt), finished: formatTime(st.FinishedAt)}
	r.src.decision, r.src.policy, r.src.observation = st.Decision, st.Policy, st.Observation
	if prev == nil || prev.id != st.ID {
		prev = &stepRow{}
	}
	if st.Decision != nil && st.Decision == prev.src.decision {
		r.decision = prev.decision
	} else {
		r.decision = marshalOpt(st.Decision)
	}
	if st.Policy != nil && st.Policy == prev.src.policy {
		r.policy = prev.policy
	} else {
		r.policy = marshalOpt(st.Policy)
	}
	if st.Observation != nil && st.Observation == prev.src.observation {
		r.observation = prev.observation
	} else {
		r.observation = marshalOpt(st.Observation)
	}
	return r
}

// decode is the step as ListSteps reads it. prev and prevStep, the row and
// step last decoded for the same step, lend the values whose text has not
// changed; the caller must never write through them.
func (r *stepRow) decode(prev *stepRow, prevStep *Step) (Step, error) {
	st := Step{ID: r.id, RunID: r.runID, Index: r.index, Status: r.status}
	if prev == nil || prevStep == nil || prev.id != r.id {
		prev, prevStep = &stepRow{}, &Step{}
	}
	var err error
	if r.decision != "" {
		if st.Decision = prevStep.Decision; r.decision != prev.decision || st.Decision == nil {
			st.Decision = new(Decision)
			if err := json.Unmarshal([]byte(r.decision), st.Decision); err != nil {
				return Step{}, err
			}
		}
	}
	if r.policy != "" {
		if st.Policy = prevStep.Policy; r.policy != prev.policy || st.Policy == nil {
			st.Policy = new(PolicyDecision)
			if err := json.Unmarshal([]byte(r.policy), st.Policy); err != nil {
				return Step{}, err
			}
		}
	}
	if r.observation != "" {
		if st.Observation = prevStep.Observation; r.observation != prev.observation || st.Observation == nil {
			st.Observation = new(Observation)
			if err := json.Unmarshal([]byte(r.observation), st.Observation); err != nil {
				return Step{}, err
			}
		}
	}
	if st.StartedAt, err = parseTime(r.started); err != nil {
		return Step{}, err
	}
	if st.FinishedAt, err = parseTime(r.finished); err != nil {
		return Step{}, err
	}
	return st, nil
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

const approvalColumns = `id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by, note, expires_at`

// ListApprovals returns a run's approvals in creation order.
func (s *Store) ListApprovals(ctx context.Context, runID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApproval loads one approval of a run.
func (s *Store) GetApproval(ctx context.Context, runID, id string) (Approval, error) {
	a, err := scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ? AND run_id = ?`, id, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// PendingApprovalIDs maps each run waiting for approval to the ids of its
// pending approvals, in creation order, in one query. It is what a listing
// of many runs needs without reading every approval's request.
func (s *Store) PendingApprovalIDs(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.run_id, a.id FROM approvals a JOIN runs r ON r.id = a.run_id WHERE a.status = ? AND r.status = ? ORDER BY a.rowid`, ApprovalPending, StatusWaitingForApproval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var runID, id string
		if err := rows.Scan(&runID, &id); err != nil {
			return nil, err
		}
		out[runID] = append(out[runID], id)
	}
	return out, rows.Err()
}

// approvalRow is an approval as stored.
type approvalRow struct {
	id, runID, stepID, kind, capability, presentation, request, hash string
	status                                                           ApprovalStatus
	created, decided, decidedBy, note, expires                       string
}

func scanApproval(sc scanner) (Approval, error) {
	var r approvalRow
	if err := sc.Scan(&r.id, &r.runID, &r.stepID, &r.kind, &r.capability, &r.presentation, &r.request, &r.hash, &r.status, &r.created, &r.decided, &r.decidedBy, &r.note, &r.expires); err != nil {
		return Approval{}, err
	}
	return r.decode()
}

// decode is the approval as ListApprovals reads it.
func (r *approvalRow) decode() (Approval, error) {
	a := Approval{ID: r.id, RunID: r.runID, StepID: r.stepID, Kind: r.kind, Hash: r.hash, Status: r.status, DecidedBy: r.decidedBy, Note: r.note}
	var err error
	if a.ExpiresAt, err = parseTime(r.expires); err != nil {
		return Approval{}, err
	}
	a.Capability, a.Presentation = json.RawMessage(r.capability), json.RawMessage(r.presentation)
	if err := json.Unmarshal([]byte(r.request), &a.Request); err != nil {
		return Approval{}, err
	}
	if a.CreatedAt, err = parseTime(r.created); err != nil {
		return Approval{}, err
	}
	if a.DecidedAt, err = parseTime(r.decided); err != nil {
		return Approval{}, err
	}
	return a, nil
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
	stmts  map[string]*sql.Stmt
	events []Event
	// cache, when set, is the loop's copy of the run, whose last written
	// row lends the text of unchanged fields. steps and approvals are the
	// rows written, which the driver applies to it after commit.
	cache     *runCache
	steps     []stepRow
	approvals []approvalRow
	// lease, set on a driver's writes, is the owner a write to a RUNNING
	// run must hold, and the lease a move to RUNNING takes. The operator
	// paths leave it nil: they need no lease.
	lease *leaseTerms
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
	t := &txn{tx: tx, stmts: s.stmts}
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

// exec and queryRow run a statement in the transaction, prepared when the
// store holds it.
func (t *txn) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if st := t.stmts[query]; st != nil {
		return t.tx.StmtContext(ctx, st).ExecContext(ctx, args...)
	}
	return t.tx.ExecContext(ctx, query, args...)
}

func (t *txn) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if st := t.stmts[query]; st != nil {
		return t.tx.StmtContext(ctx, st).QueryRowContext(ctx, args...)
	}
	return t.tx.QueryRowContext(ctx, query, args...)
}

func (t *txn) insertRun(ctx context.Context, r Run) error {
	owner, until := t.leaseFor(r.Status)
	_, err := t.tx.ExecContext(ctx, `INSERT INTO runs (id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, lease_owner, lease_expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Goal, r.Status, r.Reason, r.ReasonDetail, string(toJSON(r.Limits)), r.StepCount, formatTime(r.CreatedAt), formatTime(r.StartedAt), formatTime(r.FinishedAt), marshalOpt(r.Result), r.ModelCalls, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CachedInputTokens, int64(r.EstimatedCost), owner, until)
	return err
}

// leaseFor is the lease a run written in status holds: the writer's, while
// it is RUNNING, and none otherwise, so pausing or finishing a run releases
// its lease in the same transaction.
func (t *txn) leaseFor(status RunStatus) (owner, until string) {
	if status != StatusRunning || t.lease == nil {
		return "", ""
	}
	return t.lease.owner, formatTime(t.lease.until())
}

// takeLease acquires the lease of a run in one of from for the writer,
// unless another owner holds it unexpired at now, and returns the lease it
// replaced. A run with no lease, as one written before leases, is free.
func (t *txn) takeLease(ctx context.Context, id string, now time.Time, from ...RunStatus) (prevOwner string, prevUntil time.Time, err error) {
	status, owner, until, err := t.runStatus(ctx, id)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := inStatus(id, status, from); err != nil {
		return "", time.Time{}, err
	}
	if owner != "" && owner != t.lease.owner && until.After(now) {
		return "", time.Time{}, ErrRunLeased{RunID: id, Owner: owner, ExpiresAt: until}
	}
	if _, err := t.tx.ExecContext(ctx, `UPDATE runs SET lease_owner=?, lease_expires_at=? WHERE id=?`, t.lease.owner, formatTime(t.lease.until()), id); err != nil {
		return "", time.Time{}, err
	}
	return owner, until, nil
}

// lease reads a run's lease outside a transaction.
func (s *Store) lease(ctx context.Context, id string) (owner string, until time.Time, err error) {
	var exp string
	if err := s.db.QueryRowContext(ctx, `SELECT lease_owner, lease_expires_at FROM runs WHERE id = ?`, id).Scan(&owner, &exp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, ErrNotFound
		}
		return "", time.Time{}, err
	}
	until, err = parseTime(exp)
	return owner, until, err
}

// renewLease moves the expiry of a lease its owner still holds, and
// reports false when it does not. It is one statement outside any
// transaction, so it waits for the store's connection like any other and
// never holds it for long.
func (s *Store) renewLease(ctx context.Context, id, owner string, until time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET lease_expires_at=? WHERE id=? AND lease_owner=?`, formatTime(until), id, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// releaseLease clears a lease its owner still holds.
func (s *Store) releaseLease(ctx context.Context, id, owner string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET lease_owner='', lease_expires_at='' WHERE id=? AND lease_owner=?`, id, owner)
	return err
}

// loadRun reads a run inside the transaction, so what it returns is what
// the transaction's writes apply to, accounting totals included.
func (t *txn) loadRun(ctx context.Context, id string) (Run, error) {
	return scanRun(t.queryRow(ctx, sqlSelectRun, id))
}

// requireRun is requireStatus returning the run.
func (t *txn) requireRun(ctx context.Context, id string, from ...RunStatus) (Run, error) {
	if err := t.requireStatus(ctx, id, from...); err != nil {
		return Run{}, err
	}
	return t.loadRun(ctx, id)
}

// requireStatus fails with ErrRunState unless the run is in one of from,
// and, for a driver's write to a RUNNING run, with ErrRunLeased unless the
// writer holds its lease.
func (t *txn) requireStatus(ctx context.Context, id string, from ...RunStatus) error {
	var owner any = ""
	if t.lease != nil {
		owner = t.lease.arg
	}
	var status RunStatus
	var held bool
	if err := t.queryRow(ctx, sqlRunStatus, owner, id).Scan(&status, &held); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := inStatus(id, status, from); err != nil {
		return err
	}
	if status == StatusRunning && t.lease != nil && !held {
		_, other, until, err := t.runStatus(ctx, id)
		if err != nil {
			return err
		}
		return ErrRunLeased{RunID: id, Owner: other, ExpiresAt: until}
	}
	return nil
}

// runStatus reads a run's status and lease.
func (t *txn) runStatus(ctx context.Context, id string) (status RunStatus, owner string, until time.Time, err error) {
	var exp string
	if err := t.tx.QueryRowContext(ctx, `SELECT status, lease_owner, lease_expires_at FROM runs WHERE id = ?`, id).Scan(&status, &owner, &exp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", time.Time{}, ErrNotFound
		}
		return "", "", time.Time{}, err
	}
	until, err = parseTime(exp)
	return status, owner, until, err
}

func inStatus(id string, status RunStatus, from []RunStatus) error {
	for _, f := range from {
		if status == f {
			return nil
		}
	}
	return fmt.Errorf("%w: run %s is %s, not %s", ErrRunState, id, status, statusList(from))
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
// moved it first. A move to RUNNING takes the writer's lease and any other
// move releases it. It never writes step_count or the accounting columns,
// which have their own increments.
func (t *txn) transition(ctx context.Context, r Run, from ...RunStatus) error {
	if err := t.requireStatus(ctx, r.ID, from...); err != nil {
		return err
	}
	owner, until := t.leaseFor(r.Status)
	res, err := t.tx.ExecContext(ctx, `UPDATE runs SET status=?, reason=?, reason_detail=?, finished_at=?, result_json=?, lease_owner=?, lease_expires_at=? WHERE id=?`,
		r.Status, r.Reason, r.ReasonDetail, formatTime(r.FinishedAt), marshalOpt(r.Result), owner, until, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: update run %s: %w", r.ID, ErrNotFound)
	}
	return nil
}

// claimStep advances step_count from index to index+1 on a RUNNING run
// whose lease the writer holds. A second loop on the same run, or a run
// cancelled meanwhile, loses.
func (t *txn) claimStep(ctx context.Context, runID string, index int) error {
	var owner any = ""
	if t.lease != nil {
		owner = t.lease.arg
	}
	res, err := t.exec(ctx, sqlClaimStep, runID, StatusRunning, index, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if err := t.requireStatus(ctx, runID, StatusRunning); err != nil {
			return err
		}
		r, err := t.loadRun(ctx, runID)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: run %s is %s at step %d, cannot start step %d", ErrRunState, runID, r.Status, r.StepCount, index)
	}
	return nil
}

func (t *txn) encodeStep(st Step) stepRow {
	if t.cache == nil {
		return encodeStep(st, nil)
	}
	return encodeStep(st, &t.cache.last)
}

func (t *txn) insertStep(ctx context.Context, st Step) error {
	r := t.encodeStep(st)
	if _, err := t.exec(ctx, sqlInsertStep,
		r.id, r.runID, r.index, r.status, r.decision, r.policy, r.observation, r.hash, r.started, r.finished); err != nil {
		return err
	}
	t.steps = append(t.steps, r)
	return nil
}

// updateStep writes a step whose stored status is one of from, and fails
// with ErrRunState when it is not: another writer, such as Cancel, moved
// the step first.
func (t *txn) updateStep(ctx context.Context, st Step, from ...StepStatus) error {
	r := t.encodeStep(st)
	res, err := t.exec(ctx, sqlUpdateStep+stepGuard(len(from)),
		append([]any{r.status, r.decision, r.policy, r.observation, r.hash, r.started, r.finished, r.id}, stepArgs(from)...)...)
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
	t.steps = append(t.steps, r)
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
	_, err := t.exec(ctx, sqlAddActiveTime, dur.Milliseconds(), runID)
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
	r := approvalRow{id: a.ID, runID: a.RunID, stepID: a.StepID, kind: a.Kind, capability: string(orEmptyObject(a.Capability)), presentation: string(orEmptyObject(a.Presentation)), request: string(req), hash: a.Hash,
		status: a.Status, created: formatTime(a.CreatedAt), decided: formatTime(a.DecidedAt), decidedBy: a.DecidedBy, note: a.Note, expires: formatTime(a.ExpiresAt)}
	if _, err := t.tx.ExecContext(ctx, `INSERT INTO approvals (`+approvalColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.id, r.runID, r.stepID, r.kind, r.capability, r.presentation, r.request, r.hash, r.status, r.created, r.decided, r.decidedBy, r.note, r.expires); err != nil {
		return err
	}
	t.approvals = append(t.approvals, r)
	return nil
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
	row := t.queryRow(ctx, sqlAppendEvent,
		e.RunID, e.StepID, formatTime(e.At), e.Type, string(e.Payload))
	if err := row.Scan(&e.Seq); err != nil {
		return err
	}
	t.events = append(t.events, e)
	return nil
}
