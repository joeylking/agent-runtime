package agentrt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
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

// OpenStore opens or creates the database at path and applies migrations.
func OpenStore(path string) (*Store, error) {
	var dsn string
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	} else {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "foreign_keys(1)")
		q.Add("_pragma", "synchronous(NORMAL)")
		dsn = "file:" + path + "?" + q.Encode()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("agentrt: open store: %w", err)
	}
	// One connection: SQLite has a single writer and the in-memory database is
	// per connection.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying database for consumers that keep their own
// tables in the same file and for tests. The runtime's tables are its own.
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
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("agentrt: migrate: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("agentrt: migrate: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("agentrt: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, formatTime(time.Now())); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
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
	return string(mustJSON(v))
}

// CreateRun inserts a new run. The caller is responsible for the run.created
// event; the driver writes it in the same transaction.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	return s.tx(ctx, nil, func(t *txn) error { return t.insertRun(ctx, r) })
}

// GetRun loads a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// ListRuns returns every run, newest first.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json FROM runs ORDER BY created_at DESC, id`)
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
	err := sc.Scan(&r.ID, &r.Goal, &r.Status, &r.Reason, &r.ReasonDetail, &limits, &r.StepCount, &created, &started, &finished, &result)
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

// ListApprovals returns a run's approvals in creation order.
func (s *Store) ListApprovals(ctx context.Context, runID string) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by, note FROM approvals WHERE run_id = ? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		var capability, presentation, request, created, decided string
		if err := rows.Scan(&a.ID, &a.RunID, &a.StepID, &a.Kind, &capability, &presentation, &request, &a.Hash, &a.Status, &created, &decided, &a.DecidedBy, &a.Note); err != nil {
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
	t := &txn{tx: tx}
	if err := fn(t); err != nil {
		tx.Rollback()
		return err
	}
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
	_, err := t.tx.ExecContext(ctx, `INSERT INTO runs (id, goal, status, reason, reason_detail, limits_json, step_count, created_at, started_at, finished_at, result_json) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Goal, r.Status, r.Reason, r.ReasonDetail, string(mustJSON(r.Limits)), r.StepCount, formatTime(r.CreatedAt), formatTime(r.StartedAt), formatTime(r.FinishedAt), marshalOpt(r.Result))
	return err
}

func (t *txn) updateRun(ctx context.Context, r Run) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE runs SET status=?, reason=?, reason_detail=?, step_count=?, started_at=?, finished_at=?, result_json=? WHERE id=?`,
		r.Status, r.Reason, r.ReasonDetail, r.StepCount, formatTime(r.StartedAt), formatTime(r.FinishedAt), marshalOpt(r.Result), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: update run %s: %w", r.ID, ErrNotFound)
	}
	return nil
}

func (t *txn) insertStep(ctx context.Context, st Step) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, observation_json, observation_hash, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		st.ID, st.RunID, st.Index, st.Status, marshalOpt(st.Decision), marshalOpt(st.Policy), marshalOpt(st.Observation), obsHash(st.Observation), formatTime(st.StartedAt), formatTime(st.FinishedAt))
	return err
}

func (t *txn) updateStep(ctx context.Context, st Step) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE steps SET status=?, decision_json=?, policy_json=?, observation_json=?, observation_hash=?, finished_at=? WHERE id=?`,
		st.Status, marshalOpt(st.Decision), marshalOpt(st.Policy), marshalOpt(st.Observation), obsHash(st.Observation), formatTime(st.FinishedAt), st.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: update step %s: %w", st.ID, ErrNotFound)
	}
	return nil
}

func (t *txn) insertApproval(ctx context.Context, a Approval) error {
	req, err := json.Marshal(a.Request)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by, note) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.RunID, a.StepID, a.Kind, string(orEmptyObject(a.Capability)), string(orEmptyObject(a.Presentation)), string(req), a.Hash, a.Status, formatTime(a.CreatedAt), formatTime(a.DecidedAt), a.DecidedBy, a.Note)
	return err
}

func (t *txn) decideApproval(ctx context.Context, a Approval) error {
	res, err := t.tx.ExecContext(ctx, `UPDATE approvals SET status=?, decided_at=?, decided_by=?, note=? WHERE id=? AND status=?`,
		a.Status, formatTime(a.DecidedAt), a.DecidedBy, a.Note, a.ID, ApprovalPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("agentrt: approval %s is not pending: %w", a.ID, ErrNotFound)
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
