package agentrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
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
	// leaseDB is a connection of the lease heartbeat's own on a file-backed
	// store, so a consumer holding DB() cannot delay a renewal; it is db
	// itself for ":memory:", where a second connection would be a second
	// database and no other process can take a run over.
	leaseDB *sql.DB
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
// by another owner and has not expired, or that this driver is executing
// in another call: the run is being executed elsewhere, and Resume changes
// nothing. It matches ErrRunState with errors.Is. A loop finds it too when
// another owner took its run over.
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

// ErrUnsafeSchema means a database holds a trigger or a view. The runtime
// creates neither, and either would run SQL of the file's author inside
// the runtime's own statements, so both openers refuse such a file.
var ErrUnsafeSchema = errors.New("agentrt: database holds a trigger or view")

// ErrInsecureMode is returned by both openers for a database, or a WAL
// file beside it, that another user could write: anyone who can write the
// file can forge an approval, because an approval's hash has no secret in
// it. It names the file and its mode. A sidecar owned by another user than
// the database is refused the same way. The rules apply where file modes
// mean owner, group, and others, which excludes Windows.
type ErrInsecureMode struct {
	Path   string
	Mode   fs.FileMode
	Reason string
}

func (e ErrInsecureMode) Error() string {
	return fmt.Sprintf("agentrt: %s is %s, %s; the runtime keeps its database readable and writable by its owner only: chmod 600 %s and chown it to the user that runs the runtime", e.Path, e.Mode, e.Reason, e.Path)
}

// ErrSymlink is returned by OpenExisting when the path it is given is a
// symbolic link. It is not a case of ErrInsecureMode: the fix is to name
// the database file itself, not to change a mode, and the link's target
// is never examined. The error wrapping it names the path.
var ErrSymlink = errors.New("agentrt: database path is a symbolic link")

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

// fileParams are the connection settings of every file-backed store. The
// schema is not trusted to run functions with side effects, and defensive
// mode stops SQL from corrupting the file's own structures, because the
// file may come from someone else. A writer syncs every commit: in WAL
// mode synchronous(NORMAL) can lose the last commits on power loss, which
// would lose a committed step.tool_started for a tool that ran.
func fileParams(write bool) url.Values {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "trusted_schema(0)")
	q.Set("_defensive", "1")
	if write {
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", "immediate")
	}
	return q
}

// OpenStore opens or creates the database at path and applies migrations.
// Concurrent first opens are safe: the version is read and the migrations
// applied inside one IMMEDIATE transaction. A database migrated by a newer
// build is refused with ErrSchemaVersion, one holding a trigger or a view
// with ErrUnsafeSchema.
//
// A new database is created readable and writable by its owner only, and
// SQLite gives its WAL files the database's mode. An existing one that
// group or others can write is refused with ErrInsecureMode; one they can
// only read is made owner-only, WAL files included, when this process
// owns it.
func OpenStore(path string) (*Store, error) {
	var name string
	if path == ":memory:" {
		name = "file::memory:?_pragma=foreign_keys(1)&_txlock=immediate"
	} else {
		if err := secureFiles(path, true, true); err != nil {
			return nil, err
		}
		name = dsn(path, fileParams(true))
	}
	db, err := sql.Open("sqlite", name)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	// One connection: SQLite has a single writer and the in-memory database is
	// per connection.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, leaseDB: db}
	fail := func(err error) (*Store, error) {
		s.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := s.useWAL(context.Background()); err != nil {
			return fail(err)
		}
		if err := refuseSchemaObjects(context.Background(), db, path); err != nil {
			return fail(err)
		}
	}
	if err := s.migrate(context.Background()); err != nil {
		return fail(err)
	}
	if err := s.prepare(context.Background()); err != nil {
		return fail(err)
	}
	if path != ":memory:" {
		if s.leaseDB, err = openLeaseDB(name); err != nil {
			s.leaseDB = db
			return fail(err)
		}
	}
	return s, nil
}

// openLeaseDB opens the heartbeat's own connection to a file.
func openLeaseDB(name string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", name)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// secureFiles applies the mode rules to a database file and its WAL files
// before SQLite opens them. create makes a missing database, owner-only;
// write tightens one that others can read, if this process owns it.
func secureFiles(path string, create, write bool) error {
	if !modeRules {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		// O_EXCL: a concurrent first open that created it wins, and the
		// file is then checked like any other.
		f, cerr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if cerr == nil {
			f.Close()
		} else if !errors.Is(cerr, fs.ErrExist) {
			return fmt.Errorf("agentrt: open store: %w", cerr)
		}
		info, err = os.Stat(path)
	}
	if err != nil {
		return fmt.Errorf("agentrt: open store: %w", err)
	}
	owner, known := fileOwner(info)
	type file struct {
		path string
		info fs.FileInfo
	}
	found := []file{{path, info}}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("agentrt: open store: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return ErrInsecureMode{Path: p, Mode: fi.Mode(), Reason: "which is not a regular file"}
		}
		if uid, ok := fileOwner(fi); known && ok && uid != owner {
			return ErrInsecureMode{Path: p, Mode: fi.Mode(), Reason: "owned by another user than the database"}
		}
		found = append(found, file{p, fi})
	}
	for _, f := range found {
		if f.info.Mode().Perm()&0o022 != 0 {
			return ErrInsecureMode{Path: f.path, Mode: f.info.Mode(), Reason: "writable by group or others"}
		}
	}
	if !write {
		return nil
	}
	for _, f := range found {
		if uid, ok := fileOwner(f.info); ok && uid == os.Getuid() && f.info.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(f.path, 0o600); err != nil {
				return fmt.Errorf("agentrt: open store: %w", err)
			}
		}
	}
	return nil
}

// refuseSchemaObjects fails with ErrUnsafeSchema when the database holds a
// trigger or a view, before any statement of the runtime's touches a table.
func refuseSchemaObjects(ctx context.Context, db *sql.DB, path string) error {
	var typ, name string
	err := db.QueryRowContext(ctx, `SELECT type, name FROM sqlite_master WHERE type IN ('trigger', 'view') LIMIT 1`).Scan(&typ, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agentrt: open store: %w", err)
	}
	return fmt.Errorf("%w: %s has %s %q", ErrUnsafeSchema, path, typ, name)
}

// The statements the loop runs at every step.
const (
	sqlSelectRun     = `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs WHERE id = ?`
	sqlRunStatus     = `SELECT status, lease_owner = ? FROM runs WHERE id = ?`
	sqlClaimStep     = `UPDATE runs SET step_count = step_count + 1 WHERE id=? AND status=? AND step_count=? AND lease_owner=?`
	sqlInsertStep    = `INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, observation_json, observation_hash, started_at, finished_at, spec_hash, policy_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`
	sqlUpdateStep    = `UPDATE steps SET status=?, decision_json=?, policy_json=?, observation_json=?, observation_hash=?, started_at=?, finished_at=?, spec_hash=?, policy_id=? WHERE id=?`
	sqlAddActiveTime = `UPDATE runs SET active_ms = active_ms + ? WHERE id=?`
	sqlChainHead     = `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0), COALESCE((SELECT MAX(seq) FROM events), 0)), COALESCE((SELECT hash FROM events ORDER BY seq DESC LIMIT 1), '')`
	sqlAppendEvent   = `INSERT INTO events (seq, run_id, step_id, at, type, payload_json, hash) VALUES (?,?,?,?,?,?,?)`
	sqlPutSpec       = `INSERT OR IGNORE INTO tool_specs (hash, spec_json) VALUES (?,?)`
)

func (s *Store) prepare(ctx context.Context) error {
	s.stmts = map[string]*sql.Stmt{}
	for _, q := range []string{sqlSelectRun, sqlRunStatus, sqlClaimStep, sqlInsertStep, sqlUpdateStep + stepGuard(1), sqlAddActiveTime, sqlChainHead, sqlAppendEvent, sqlPutSpec} {
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
// read-only store never writes to the file, nor changes its mode.
//
// The path is the file itself: a symbolic link is refused with ErrSymlink
// rather than followed, so what is opened is what the operator named. The mode rules
// and the trigger and view refusal are OpenStore's.
func OpenExisting(path string, readOnly bool) (*Store, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s; name the database file itself", ErrSymlink, path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("agentrt: open store: %s is not a regular file", path)
	}
	if err := secureFiles(path, false, !readOnly); err != nil {
		return nil, err
	}
	q := fileParams(!readOnly)
	if readOnly {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rw")
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
	if err != nil || readOnly {
		return s, err
	}
	if s.leaseDB, err = openLeaseDB(dsn(path, q)); err != nil {
		s.leaseDB = s.db
		s.Close()
		return nil, err
	}
	return s, nil
}

const sqliteReadonlyDirectory = 1544 // SQLITE_READONLY_DIRECTORY

func openExisting(path string, q url.Values) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path, q))
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := refuseSchemaObjects(context.Background(), db, path); err != nil {
		db.Close()
		return nil, err
	}
	var current int
	if err := db.QueryRowContext(context.Background(), `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		db.Close()
		return nil, fmt.Errorf("agentrt: open store: %s is not an agent-runtime database: %w", path, err)
	}
	if current != len(migrations) {
		db.Close()
		return nil, fmt.Errorf("%w: %s is at version %d, this build writes %d", ErrSchemaVersion, path, current, len(migrations))
	}
	return &Store{db: db, leaseDB: db}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	s.closeStmts()
	if s.leaseDB != nil && s.leaseDB != s.db {
		s.leaseDB.Close()
	}
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
//
// Holding the connection, with a transaction or with rows not yet closed,
// stalls every write the loop makes. The lease heartbeat of a file-backed
// store renews on a connection of its own, but a loop stalled past its
// lease's expiry forfeits the run: it starts no tool or model call once
// the expiry has passed, and another process may take the run over.
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
	// What a decision can be rechecked against: each distinct tool spec by
	// its hash, the spec and policy each step was decided under, and the
	// hash chain over the events, which the migration backfills for the
	// events already stored. A step written before it has neither.
	`CREATE TABLE tool_specs (
		hash TEXT PRIMARY KEY,
		spec_json TEXT NOT NULL
	);
	ALTER TABLE steps ADD COLUMN spec_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE steps ADD COLUMN policy_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN hash TEXT NOT NULL DEFAULT '';`,
}

// leaseMigration is the version that added the run lease.
const leaseMigration = 5

// chainMigration is the version that added the event hash chain, whose
// hashes it computes for the events already stored.
const chainMigration = 6

// preLeaseOwner is the lease owner stamped, with an expiry DefaultLeaseTTL
// away, on every RUNNING run when the lease migration is applied: such a
// run may still be executing in a process of a version that knew nothing
// of leases, so Resume waits one lease out before taking it over.
const preLeaseOwner = "agentrt:pre-lease-version"

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
		if i+1 == leaseMigration {
			// A run an older version left RUNNING may still be executing
			// in a process of that version, which ignores leases. It gets
			// one lease's grace before Resume may take it over.
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET lease_owner=?, lease_expires_at=? WHERE status=?`, preLeaseOwner, formatTime(time.Now().Add(DefaultLeaseTTL)), StatusRunning); err != nil {
				return fmt.Errorf("agentrt: migration %d: %w", i+1, err)
			}
		}
		if i+1 == chainMigration {
			if err := backfillChain(ctx, tx); err != nil {
				return fmt.Errorf("agentrt: migration %d: %w", i+1, err)
			}
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

func marshalOpt(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	switch x := v.(type) {
	case *Decision:
		if x == nil {
			return "", nil
		}
	case *PolicyDecision:
		if x == nil {
			return "", nil
		}
	case *Observation:
		if x == nil {
			return "", nil
		}
	case json.RawMessage:
		if len(x) == 0 {
			return "", nil
		}
		return string(x), nil
	}
	b, err := toJSON(v)
	return string(b), err
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
		out = append(out, rows[i].decode(nil, nil))
	}
	return out, nil
}

func (s *Store) listStepRows(ctx context.Context, runID string) ([]stepRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, idx, status, decision_json, policy_json, observation_json, started_at, finished_at, spec_hash, policy_id FROM steps WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var r stepRow
		if err := rows.Scan(&r.id, &r.runID, &r.index, &r.status, &r.decision, &r.policy, &r.observation, &r.started, &r.finished, &r.specHash, &r.policyID); err != nil {
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
	specHash, policyID                                     string
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
func encodeStep(st Step, prev *stepRow) (stepRow, error) {
	r := stepRow{id: st.ID, runID: st.RunID, index: st.Index, status: st.Status, hash: obsHash(st.Observation), started: formatTime(st.StartedAt), finished: formatTime(st.FinishedAt), specHash: st.SpecHash, policyID: st.PolicyID}
	r.src.decision, r.src.policy, r.src.observation = st.Decision, st.Policy, st.Observation
	if prev == nil || prev.id != st.ID {
		prev = &stepRow{}
	}
	var err error
	if st.Decision != nil && st.Decision == prev.src.decision {
		r.decision = prev.decision
	} else if r.decision, err = marshalOpt(st.Decision); err != nil {
		return stepRow{}, err
	}
	if st.Policy != nil && st.Policy == prev.src.policy {
		r.policy = prev.policy
	} else if r.policy, err = marshalOpt(st.Policy); err != nil {
		return stepRow{}, err
	}
	if st.Observation != nil && st.Observation == prev.src.observation {
		r.observation = prev.observation
	} else if r.observation, err = marshalOpt(st.Observation); err != nil {
		return stepRow{}, err
	}
	return r, nil
}

// decode is the step as ListSteps reads it. prev and prevStep, the row and
// step last decoded for the same step, lend the values whose text has not
// changed; the caller must never write through them. A column that does
// not decode leaves its field empty and is named in DecodeError, so one
// damaged row cannot make its run unreadable.
func (r *stepRow) decode(prev *stepRow, prevStep *Step) Step {
	st := Step{ID: r.id, RunID: r.runID, Index: r.index, Status: r.status, SpecHash: r.specHash, PolicyID: r.policyID}
	if prev == nil || prevStep == nil || prev.id != r.id {
		prev, prevStep = &stepRow{}, &Step{}
	}
	var bad []string
	if r.decision != "" {
		if st.Decision = prevStep.Decision; r.decision != prev.decision || st.Decision == nil {
			st.Decision = new(Decision)
			if err := json.Unmarshal([]byte(r.decision), st.Decision); err != nil {
				st.Decision, bad = nil, append(bad, "decision: "+err.Error())
			}
		}
	}
	if r.policy != "" {
		if st.Policy = prevStep.Policy; r.policy != prev.policy || st.Policy == nil {
			st.Policy = new(PolicyDecision)
			if err := json.Unmarshal([]byte(r.policy), st.Policy); err != nil {
				st.Policy, bad = nil, append(bad, "policy: "+err.Error())
			}
		}
	}
	if r.observation != "" {
		if st.Observation = prevStep.Observation; r.observation != prev.observation || st.Observation == nil {
			st.Observation = new(Observation)
			if err := json.Unmarshal([]byte(r.observation), st.Observation); err != nil {
				st.Observation, bad = nil, append(bad, "observation: "+err.Error())
			}
		}
	}
	var err error
	if st.StartedAt, err = parseTime(r.started); err != nil {
		bad = append(bad, "started_at: "+err.Error())
	}
	if st.FinishedAt, err = parseTime(r.finished); err != nil {
		bad = append(bad, "finished_at: "+err.Error())
	}
	if len(bad) > 0 {
		st.DecodeError = strings.Join(bad, "; ")
	}
	return st
}

// decodeErrors names the steps and approvals of a run that did not decode.
func decodeErrors(steps []Step, approvals []Approval) error {
	for _, st := range steps {
		if st.DecodeError != "" {
			return fmt.Errorf("step %d (%s) could not be decoded: %s", st.Index, st.ID, st.DecodeError)
		}
	}
	for _, a := range approvals {
		if a.DecodeError != "" {
			return fmt.Errorf("approval %s could not be decoded: %s", a.ID, a.DecodeError)
		}
	}
	return nil
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

// firstApprovals reads a run's first n approvals in creation order, each
// whole, as GetApproval reads one.
func (s *Store) firstApprovals(ctx context.Context, runID string, n int) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE run_id = ? ORDER BY rowid LIMIT ?`, runID, n)
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
	return r.decode(), nil
}

// decode is the approval as ListApprovals reads it. A column that does not
// decode is named in DecodeError, as for a step.
func (r *approvalRow) decode() Approval {
	a := Approval{ID: r.id, RunID: r.runID, StepID: r.stepID, Kind: r.kind, Hash: r.hash, Status: r.status, DecidedBy: r.decidedBy, Note: r.note}
	var bad []string
	var err error
	if a.ExpiresAt, err = parseTime(r.expires); err != nil {
		bad = append(bad, "expires_at: "+err.Error())
	}
	a.Capability, a.Presentation = json.RawMessage(r.capability), json.RawMessage(r.presentation)
	if err := json.Unmarshal([]byte(r.request), &a.Request); err != nil {
		a.Request, bad = ToolRequest{}, append(bad, "request: "+err.Error())
	}
	if a.CreatedAt, err = parseTime(r.created); err != nil {
		bad = append(bad, "created_at: "+err.Error())
	}
	if a.DecidedAt, err = parseTime(r.decided); err != nil {
		bad = append(bad, "decided_at: "+err.Error())
	}
	if len(bad) > 0 {
		a.DecodeError = strings.Join(bad, "; ")
	}
	return a
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

// MaxPageText is the most characters the page reads (ListRunsPage,
// ListStepsPage, ListApprovalsPage, ListEventsPage) return from any one
// text column, so a crafted database cannot make a front end that lists it
// read without bound. A capped text column keeps its first MaxPageText
// characters and ends in a marker giving its full length; a capped JSON
// column is replaced by {"truncated":true,"length":n,"head":"<its first
// MaxPageText characters>"}, which is still JSON. Each page read returns
// the total its listing would have, from a COUNT, so a front end can say
// how much it did not show without reading it.
const MaxPageText = 64 << 10

// capText and capJSON are SQL that cap a column at MaxPageText characters.
func capText(col string) string {
	return fmt.Sprintf(`CASE WHEN length(%[1]s) > %[2]d THEN substr(%[1]s, 1, %[2]d) || '…[truncated: ' || length(%[1]s) || ' characters]' ELSE %[1]s END`, col, MaxPageText)
}

func capJSON(col string) string {
	return fmt.Sprintf(`CASE WHEN length(%[1]s) > %[2]d THEN json_object('truncated', json('true'), 'length', length(%[1]s), 'head', substr(%[1]s, 1, %[2]d)) ELSE %[1]s END`, col, MaxPageText)
}

// reduceJSON is SQL for a JSON column that keeps the column whole within
// MaxPageText and otherwise builds a small object from fields, pairs of a
// key and the SQL of its value, so a front end still learns what the row
// was without reading all of it.
func reduceJSON(col string, fields ...[2]string) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf(`'%s', %s`, f[0], f[1]))
	}
	return fmt.Sprintf(`CASE WHEN length(%[1]s) <= %[2]d THEN %[1]s WHEN json_valid(%[1]s) THEN json_object(%[3]s) ELSE '' END`, col, MaxPageText, strings.Join(parts, ", "))
}

// extract is SQL for the value at path in a JSON column, capped at
// MaxPageText characters.
func extract(col, path string) string {
	return fmt.Sprintf(`substr(json_extract(%s, '%s'), 1, %d)`, col, path, MaxPageText)
}

// pageArgs refuses a page that is not one.
func pageArgs(what string, limit, offset int) error {
	if limit <= 0 || offset < 0 {
		return fmt.Errorf("agentrt: list %s: limit %d and offset %d", what, limit, offset)
	}
	return nil
}

// pageRow is called for each row a page read decodes. Tests count with it.
var pageRow = func() {}

func (s *Store) count(ctx context.Context, query string, args ...any) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// ListRunsPage is ListRuns for a front end reading a database it may not
// trust: at most limit runs, newest first, after skipping offset, with
// every text column capped at MaxPageText, and the number of runs in the
// database. Limits that do not decode, or that were capped, read as zero,
// and so does a time that does not parse, rather than failing the page.
func (s *Store) ListRunsPage(ctx context.Context, limit, offset int) ([]Run, int, error) {
	if err := pageArgs("runs", limit, offset); err != nil {
		return nil, 0, err
	}
	total, err := s.count(ctx, `SELECT COUNT(*) FROM runs`)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, runPageSelect+` ORDER BY rowid DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		pageRow()
		r, err := scanRunPage(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetRunCapped is GetRun read the way ListRunsPage reads a run: every text
// column capped at MaxPageText, and limits or times that do not decode
// read as zero. It is what a front end showing one run of a database it
// may not trust reads; the driver never uses it.
func (s *Store) GetRunCapped(ctx context.Context, id string) (Run, error) {
	r, err := scanRunPage(s.db.QueryRowContext(ctx, runPageSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

var runPageSelect = `SELECT ` + capText("id") + `, ` + capText("goal") + `, ` + capText("status") + `, ` + capText("reason") + `, ` + capText("reason_detail") + `, CASE WHEN length(limits_json) > ` + fmt.Sprint(MaxPageText) + ` THEN '' ELSE limits_json END, step_count, ` + capText("created_at") + `, ` + capText("started_at") + `, ` + capText("finished_at") + `, ` + capJSON("result_json") + `, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, active_ms FROM runs`

func scanRunPage(sc scanner) (Run, error) {
	var r Run
	var limits, created, started, finished, result string
	var cost, activeMS int64
	if err := sc.Scan(&r.ID, &r.Goal, &r.Status, &r.Reason, &r.ReasonDetail, &limits, &r.StepCount, &created, &started, &finished, &result, &r.ModelCalls, &r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CachedInputTokens, &cost, &activeMS); err != nil {
		return Run{}, err
	}
	r.EstimatedCost, r.ActiveTime = Micros(cost), time.Duration(activeMS)*time.Millisecond
	if json.Unmarshal([]byte(limits), &r.Limits) != nil {
		r.Limits = Limits{}
	}
	r.CreatedAt, _ = parseTime(created)
	r.StartedAt, _ = parseTime(started)
	r.FinishedAt, _ = parseTime(finished)
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	return r, nil
}

// ListStepsPage is ListSteps bounded the same way: at most limit steps of a
// run in index order after skipping offset, and the run's number of steps.
// A decision, policy, or observation column over MaxPageText is not read
// whole: only the decision's kind and tool, the policy's outcome, or the
// observation's kind and summary are, and DecodeError names the column and
// its length. A page is for showing; the driver never reads one.
func (s *Store) ListStepsPage(ctx context.Context, runID string, limit, offset int) ([]Step, int, error) {
	if err := pageArgs("steps", limit, offset); err != nil {
		return nil, 0, err
	}
	total, err := s.count(ctx, `SELECT COUNT(*) FROM steps WHERE run_id = ?`, runID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+capText("id")+`, `+capText("run_id")+`, idx, `+capText("status")+`, `+
		reduceJSON("decision_json", [2]string{"kind", extract("decision_json", "$.kind")}, [2]string{"tool", extract("decision_json", "$.tool")})+`, `+
		reduceJSON("policy_json", [2]string{"outcome", extract("policy_json", "$.outcome")})+`, `+
		reduceJSON("observation_json", [2]string{"kind", extract("observation_json", "$.kind")}, [2]string{"summary", extract("observation_json", "$.summary")})+`, `+
		`length(decision_json), length(policy_json), length(observation_json), `+capText("started_at")+`, `+capText("finished_at")+`, `+capText("spec_hash")+`, `+capText("policy_id")+
		` FROM steps WHERE run_id = ? ORDER BY idx LIMIT ? OFFSET ?`, runID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		pageRow()
		var r stepRow
		var lens [3]int
		if err := rows.Scan(&r.id, &r.runID, &r.index, &r.status, &r.decision, &r.policy, &r.observation, &lens[0], &lens[1], &lens[2], &r.started, &r.finished, &r.specHash, &r.policyID); err != nil {
			return nil, 0, err
		}
		st := r.decode(nil, nil)
		var cut []string
		for i, name := range []string{"decision", "policy", "observation"} {
			if lens[i] > MaxPageText {
				cut = append(cut, fmt.Sprintf("%s: %d characters, over MaxPageText, not read whole", name, lens[i]))
			}
		}
		if len(cut) > 0 {
			if st.DecodeError != "" {
				cut = append([]string{st.DecodeError}, cut...)
			}
			st.DecodeError = strings.Join(cut, "; ")
		}
		out = append(out, st)
	}
	return out, total, rows.Err()
}

// ListApprovalsPage is ListApprovals bounded the same way: at most limit
// approvals of a run in creation order after skipping offset, only those
// with the given status unless status is empty, and how many there are.
// Capability and presentation are capped as JSON columns; a request over
// MaxPageText is read as its run, step, and tool name with its arguments
// replaced by {"truncated":true,"length":n}, and DecodeError says so. An
// approval read this way is for showing: to decide one, read it with
// GetApproval, whose hash the decision is bound to.
func (s *Store) ListApprovalsPage(ctx context.Context, runID string, status ApprovalStatus, limit, offset int) ([]Approval, int, error) {
	if err := pageArgs("approvals", limit, offset); err != nil {
		return nil, 0, err
	}
	total, err := s.count(ctx, `SELECT COUNT(*) FROM approvals WHERE run_id = ? AND (? = '' OR status = ?)`, runID, status, status)
	if err != nil {
		return nil, 0, err
	}
	request := reduceJSON("request_json",
		[2]string{"run_id", extract("request_json", "$.run_id")},
		[2]string{"step_id", extract("request_json", "$.step_id")},
		[2]string{"spec", `json_object('name', ` + extract("request_json", "$.spec.name") + `)`},
		[2]string{"args", `json_object('truncated', json('true'), 'length', length(request_json))`})
	rows, err := s.db.QueryContext(ctx, `SELECT `+capText("id")+`, `+capText("run_id")+`, `+capText("step_id")+`, `+capText("kind")+`, `+capJSON("capability_json")+`, `+capJSON("presentation_json")+`, `+request+`, length(request_json), `+capText("hash")+`, `+capText("status")+`, `+capText("created_at")+`, `+capText("decided_at")+`, `+capText("decided_by")+`, `+capText("note")+`, `+capText("expires_at")+
		` FROM approvals WHERE run_id = ? AND (? = '' OR status = ?) ORDER BY rowid LIMIT ? OFFSET ?`, runID, status, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		pageRow()
		var r approvalRow
		var reqLen int
		if err := rows.Scan(&r.id, &r.runID, &r.stepID, &r.kind, &r.capability, &r.presentation, &r.request, &reqLen, &r.hash, &r.status, &r.created, &r.decided, &r.decidedBy, &r.note, &r.expires); err != nil {
			return nil, 0, err
		}
		a := r.decode()
		if reqLen > MaxPageText {
			cut := fmt.Sprintf("request: %d characters, over MaxPageText; its arguments were not read", reqLen)
			if a.DecodeError != "" {
				cut = a.DecodeError + "; " + cut
			}
			a.DecodeError = cut
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// ListEventsPage is ListEvents bounded the same way: at most limit events
// of a run in sequence order after skipping offset, each text column
// capped at MaxPageText and a time that does not parse read as zero, and
// the run's number of events.
func (s *Store) ListEventsPage(ctx context.Context, runID string, limit, offset int) ([]Event, int, error) {
	if err := pageArgs("events", limit, offset); err != nil {
		return nil, 0, err
	}
	total, err := s.count(ctx, `SELECT COUNT(*) FROM events WHERE run_id = ?`, runID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, `+capText("run_id")+`, `+capText("step_id")+`, `+capText("at")+`, `+capText("type")+`, `+capJSON("payload_json")+` FROM events WHERE run_id = ? ORDER BY seq LIMIT ? OFFSET ?`, runID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		pageRow()
		var e Event
		var at, payload string
		if err := rows.Scan(&e.Seq, &e.RunID, &e.StepID, &at, &e.Type, &payload); err != nil {
			return nil, 0, err
		}
		e.At, _ = parseTime(at)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// ListEventsAfter returns at most limit events with Seq greater than seq,
// in Seq order: of one run when runID is set, of every run interleaved in
// commit order when it is empty. Each text column is capped at MaxPageText
// and a time that does not parse reads as zero, as in ListEventsPage. Seq
// is assigned in the transaction that commits the event, so it rises in
// commit order across the database and is never reused: the last Seq
// returned is a cursor a follower resumes after.
func (s *Store) ListEventsAfter(ctx context.Context, runID string, seq int64, limit int) ([]Event, error) {
	if err := pageArgs("events", limit, 0); err != nil {
		return nil, err
	}
	query := `SELECT seq, ` + capText("run_id") + `, ` + capText("step_id") + `, ` + capText("at") + `, ` + capText("type") + `, ` + capJSON("payload_json") + ` FROM events WHERE seq > ?`
	args := []any{seq}
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY seq LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		pageRow()
		var e Event
		var at, payload string
		if err := rows.Scan(&e.Seq, &e.RunID, &e.StepID, &at, &e.Type, &payload); err != nil {
			return nil, err
		}
		e.At, _ = parseTime(at)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ApprovalReason returns the policy's reason for an approval, capped at
// MaxPageText characters: the Reason of the PolicyDecision that paused the
// run on it, as its approval.requested event recorded it. The approval
// row has no reason of its own. The step's recorded policy agrees while
// the approval is pending; once it is granted and resumed, the step holds
// the policy's decision at resume, and this still returns the one that
// asked. ErrNotFound when the run has no such approval; empty when the
// approval has no approval.requested event.
func (s *Store) ApprovalReason(ctx context.Context, runID, approvalID string) (string, error) {
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT `+extract("e.payload_json", "$.reason")+` FROM events e
		WHERE e.run_id = a.run_id AND e.step_id = a.step_id AND e.type = ? AND CASE WHEN json_valid(e.payload_json) THEN json_extract(e.payload_json, '$.approval_id') END = a.id
		ORDER BY e.seq LIMIT 1)
		FROM approvals a WHERE a.run_id = ? AND a.id = ?`, EventApprovalRequested, runID, approvalID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return reason.String, nil
}

// PendingApprovalIDsOf is PendingApprovalIDs for the named runs only, with
// at most max ids per run, each capped at MaxPageText: what a page of runs
// needs to say which approval each is waiting on, bounded by the page.
func (s *Store) PendingApprovalIDsOf(ctx context.Context, runIDs []string, max int) (map[string][]string, error) {
	out := map[string][]string{}
	if len(runIDs) == 0 || max <= 0 {
		return out, nil
	}
	ids, err := json.Marshal(runIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, `+capText("id")+` FROM (
		SELECT a.run_id, a.id, ROW_NUMBER() OVER (PARTITION BY a.run_id ORDER BY a.rowid) AS n, a.rowid AS r
		FROM approvals a JOIN runs ON runs.id = a.run_id
		WHERE a.status = ? AND runs.status = ? AND a.run_id IN (SELECT value FROM json_each(?))
	) WHERE n <= ? ORDER BY r`, ApprovalPending, StatusWaitingForApproval, string(ids), max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		pageRow()
		var runID, id string
		if err := rows.Scan(&runID, &id); err != nil {
			return nil, err
		}
		out[runID] = append(out[runID], id)
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
	// head is the last event in the chain, read at the transaction's
	// first append and advanced by each, so a transaction that appends
	// several events reads it once.
	head struct {
		seq  int64
		hash string
		read bool
	}
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
	limits, err := toJSON(r.Limits)
	if err != nil {
		return err
	}
	result, err := marshalOpt(r.Result)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `INSERT INTO runs (id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens, output_tokens, cached_input_tokens, estimated_cost_micros, lease_owner, lease_expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Goal, r.Status, r.Reason, r.ReasonDetail, string(limits), r.StepCount, formatTime(r.CreatedAt), formatTime(r.StartedAt), formatTime(r.FinishedAt), result, r.ModelCalls, r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CachedInputTokens, int64(r.EstimatedCost), owner, until)
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

// Lease returns the lease stored on a run: the owner of the driver that
// holds it and when it expires by that driver's wall clock. A run nobody
// holds, paused, finished, or released by the call that executed it, has
// an empty owner and a zero time. A lease whose time has passed is
// stale, not released: Resume takes such a run over. ErrNotFound when
// there is no such run.
func (s *Store) Lease(ctx context.Context, runID string) (owner string, until time.Time, err error) {
	return s.lease(ctx, runID)
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

// requireLease is requireStatus for a write that starts a side effect: the
// run must be RUNNING under the writer's lease, and the lease unexpired by
// the wall clock, so a loop whose lease lapsed starts nothing even when
// nobody has taken the run over.
func (t *txn) requireLease(ctx context.Context, id string) error {
	status, owner, until, err := t.runStatus(ctx, id)
	if err != nil {
		return err
	}
	if err := inStatus(id, status, running); err != nil {
		return err
	}
	if owner != t.lease.owner {
		return ErrRunLeased{RunID: id, Owner: owner, ExpiresAt: until}
	}
	if now := t.lease.wall(); !until.After(now) {
		return fmt.Errorf("%w: run %s: the lease expired at %s", ErrLeaseLost, id, until.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// renewLease moves the expiry of a lease its owner still holds, and
// reports false when it does not. It is one statement outside any
// transaction, on the heartbeat's own connection of a file-backed store,
// so a consumer holding DB() does not delay it.
func (s *Store) renewLease(ctx context.Context, id, owner string, until time.Time) (bool, error) {
	res, err := s.leaseDB.ExecContext(ctx, `UPDATE runs SET lease_expires_at=? WHERE id=? AND lease_owner=?`, formatTime(until), id, owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// releaseLease clears a lease its owner still holds.
func (s *Store) releaseLease(ctx context.Context, id, owner string) error {
	_, err := s.leaseDB.ExecContext(ctx, `UPDATE runs SET lease_owner='', lease_expires_at='' WHERE id=? AND lease_owner=?`, id, owner)
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
	result, err := marshalOpt(r.Result)
	if err != nil {
		return err
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE runs SET status=?, reason=?, reason_detail=?, finished_at=?, result_json=?, lease_owner=?, lease_expires_at=? WHERE id=?`,
		r.Status, r.Reason, r.ReasonDetail, formatTime(r.FinishedAt), result, owner, until, r.ID)
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

func (t *txn) encodeStep(st Step) (stepRow, error) {
	if t.cache == nil {
		return encodeStep(st, nil)
	}
	return encodeStep(st, &t.cache.last)
}

func (t *txn) insertStep(ctx context.Context, st Step) error {
	r, err := t.encodeStep(st)
	if err != nil {
		return err
	}
	if _, err := t.exec(ctx, sqlInsertStep,
		r.id, r.runID, r.index, r.status, r.decision, r.policy, r.observation, r.hash, r.started, r.finished, r.specHash, r.policyID); err != nil {
		return err
	}
	t.steps = append(t.steps, r)
	return nil
}

// updateStep writes a step whose stored status is one of from, and fails
// with ErrRunState when it is not: another writer, such as Cancel, moved
// the step first.
func (t *txn) updateStep(ctx context.Context, st Step, from ...StepStatus) error {
	r, err := t.encodeStep(st)
	if err != nil {
		return err
	}
	res, err := t.exec(ctx, sqlUpdateStep+stepGuard(len(from)),
		append([]any{r.status, r.decision, r.policy, r.observation, r.hash, r.started, r.finished, r.specHash, r.policyID, r.id}, stepArgs(from)...)...)
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

// sqlAddUsage adds a model call's charge to a run's totals. The charge is
// never negative, and a total saturates rather than overflowing, which in
// SQLite would turn it into a float.
var sqlAddUsage = strings.NewReplacer("$T", fmt.Sprint(math.MaxInt), "$C", fmt.Sprint(int64(math.MaxInt64))).Replace(`UPDATE runs SET model_calls = model_calls + 1,
	input_tokens = CASE WHEN input_tokens > $T - ?1 THEN $T ELSE input_tokens + ?1 END,
	output_tokens = CASE WHEN output_tokens > $T - ?2 THEN $T ELSE output_tokens + ?2 END,
	cached_input_tokens = CASE WHEN cached_input_tokens > $T - ?3 THEN $T ELSE cached_input_tokens + ?3 END,
	estimated_cost_micros = CASE WHEN estimated_cost_micros > $C - ?4 THEN $C ELSE estimated_cost_micros + ?4 END
	WHERE id = ?5`)

func (t *txn) addRunUsage(ctx context.Context, runID string, u Usage, cost Micros) error {
	_, err := t.tx.ExecContext(ctx, sqlAddUsage, max(u.InputTokens, 0), max(u.OutputTokens, 0), max(u.CachedInputTokens, 0), max(int64(cost), 0), runID)
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

// decideApproval records a decision on an approval that is still in
// status from and still carries hash: the row decided is the row that was
// checked, whatever another writer did since.
func (t *txn) decideApproval(ctx context.Context, a Approval, from ApprovalStatus, hash string) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE approvals SET status=?, decided_at=?, decided_by=?, note=? WHERE id=? AND status=? AND hash=?`,
		a.Status, formatTime(a.DecidedAt), a.DecidedBy, a.Note, a.ID, from, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: approval %s", ErrNotPending, a.ID)
	}
	return nil
}

// getApproval reads one approval inside the transaction.
func (t *txn) getApproval(ctx context.Context, runID, id string) (Approval, error) {
	a, err := scanApproval(t.tx.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ? AND run_id = ?`, id, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

func obsHash(o *Observation) string {
	if o == nil {
		return ""
	}
	return o.ContentHash
}

// emit appends an event whose payload is v encoded.
func (t *txn) emit(ctx context.Context, e Event, v any) error {
	p, err := toJSON(v)
	if err != nil {
		return err
	}
	e.Payload = p
	return t.appendEvent(ctx, e)
}

// appendEvent appends an event and its link in the chain: the hash over
// the previous event's hash and this event's fields as stored. The
// transaction holds the write lock from its start, so the head it reads
// is the last event committed and no other writer can append before it
// commits. The seq is the one AUTOINCREMENT would assign, past every seq
// ever used, so a follower's cursor is never reused.
func (t *txn) appendEvent(ctx context.Context, e Event) error {
	if len(e.Payload) == 0 {
		e.Payload = json.RawMessage("{}")
	}
	if !t.head.read {
		if err := t.queryRow(ctx, sqlChainHead).Scan(&t.head.seq, &t.head.hash); err != nil {
			return err
		}
		t.head.read = true
	}
	e.Seq = t.head.seq + 1
	at, payload := formatTime(e.At), string(e.Payload)
	hash := eventHash(t.head.hash, e.Seq, e.RunID, e.StepID, at, e.Type, payload)
	if _, err := t.exec(ctx, sqlAppendEvent, e.Seq, e.RunID, e.StepID, at, e.Type, payload, hash); err != nil {
		return err
	}
	t.head.seq, t.head.hash = e.Seq, hash
	t.events = append(t.events, e)
	return nil
}
