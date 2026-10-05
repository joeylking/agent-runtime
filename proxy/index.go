package proxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// The proxy keeps one table of its own in the runtime's database file, the
// index of its calls, under migrations of its own in a table of its own,
// as docs/architecture.md allows a consumer: the runtime's tables and its
// schema_migrations are never written, so agentrt, which refuses a
// database whose runtime schema is not its own, reads the file as before.
//
// A row says that a call with this request key was given this run id. It
// is written before the run is begun, so a row whose run does not exist
// means nothing started: what a call did is read from its run and steps,
// never from the index. open marks the latest row for its key, at most one
// per key by a unique index; a row is closed only in the transaction that
// opens the next one for its key, and only once its run has finished, so
// a run that has not finished always has its key's open row and the next
// identical call finds it. A row whose run never began stays open, and
// the next call with its key begins the run under the same id: of two
// calls that do, the store lets one begin it.
//
// settled_at caches that the run reached a terminal status, which is
// permanent, so the scans for pending approvals and interruptions skip
// it, and outcome caches what its last attempt at the tool came to; a row
// whose run had not begun when a scan found it is settled with no attempt,
// and the call that then begins its run unsettles it. Neither is more than
// a cache of what the run's own record says.
var indexMigrations = []string{
	`CREATE TABLE proxy_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session TEXT NOT NULL,
		request_key TEXT NOT NULL,
		tool TEXT NOT NULL,
		side_effect TEXT NOT NULL,
		run_id TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		open INTEGER NOT NULL DEFAULT 1,
		closed_at INTEGER NOT NULL DEFAULT 0,
		settled_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE UNIQUE INDEX proxy_calls_open ON proxy_calls(session, request_key) WHERE open = 1;
	CREATE INDEX proxy_calls_key ON proxy_calls(session, request_key, id);
	CREATE INDEX proxy_calls_unsettled ON proxy_calls(session, settled_at, id);`,
	`ALTER TABLE proxy_calls ADD COLUMN outcome INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX proxy_calls_outcome ON proxy_calls(session, request_key, outcome, id);`,
}

// outcome is what a settled row's run last came to at its tool: unseen
// until the row is settled, then none for a run whose tool never started,
// known for a result or an error the server answered, and unknown for a
// call cut off, timed out, or left without an answer, and for a re-run of
// one that never executed.
type outcome int

const (
	outcomeUnseen outcome = iota
	outcomeNone
	outcomeKnown
	outcomeUnknown
)

// index is the proxy's table, on the store's own connection: every write
// transaction on it begins IMMEDIATE, so one transaction here is serialised
// against every writer of the file, in this process and any other.
type index struct {
	db *sql.DB
}

// migrateIndex applies what is missing of indexMigrations in one IMMEDIATE
// transaction, so concurrent first opens are safe, and refuses a database
// a newer proxy migrated.
func migrateIndex(ctx context.Context, store *agentrt.Store) (*index, error) {
	db := store.DB()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("proxy: migrate: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS proxy_schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return nil, fmt.Errorf("proxy: migrate: %w", err)
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM proxy_schema_migrations`).Scan(&current); err != nil {
		return nil, fmt.Errorf("proxy: migrate: %w", err)
	}
	if current > len(indexMigrations) {
		return nil, fmt.Errorf("proxy: the database's call index is at version %d and this build knows %d: a newer agentrt-proxy migrated it", current, len(indexMigrations))
	}
	for i := current; i < len(indexMigrations); i++ {
		if _, err := tx.ExecContext(ctx, indexMigrations[i]); err != nil {
			return nil, fmt.Errorf("proxy: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, time.Now().UnixNano()); err != nil {
			return nil, fmt.Errorf("proxy: migration %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("proxy: migrate: %w", err)
	}
	return &index{db: db}, nil
}

// row is one call in the index.
type row struct {
	id      int64
	key     string
	tool    string
	class   agentrt.SideEffect
	runID   string
	created time.Time
	open    bool
	closed  time.Time
	settled time.Time
	outcome outcome
}

const rowColumns = `id, request_key, tool, side_effect, run_id, created_at, open, closed_at, settled_at, outcome`

func unix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func stamp(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func scanRows(rows *sql.Rows, err error) ([]row, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		var created, closed, settled int64
		var open int
		if err := rows.Scan(&r.id, &r.key, &r.tool, &r.class, &r.runID, &created, &open, &closed, &settled, &r.outcome); err != nil {
			return nil, err
		}
		r.created, r.open, r.closed, r.settled = unix(created), open == 1, unix(closed), unix(settled)
		out = append(out, r)
	}
	return out, rows.Err()
}

// forKey returns the open row for the key, if any, and every closed row
// closed at or after since, newest first. A run that finished at or after
// since was closed no earlier, so these are every run with this key that
// can have executed, been rejected, or expired since then.
func (x *index) forKey(ctx context.Context, session, key string, since time.Time) (open *row, recent []row, err error) {
	rows, err := scanRows(x.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM proxy_calls WHERE session = ? AND request_key = ? AND (open = 1 OR closed_at >= ?) ORDER BY id DESC`,
		session, key, stamp(since)))
	if err != nil {
		return nil, nil, fmt.Errorf("proxy: index: %w", err)
	}
	for i := range rows {
		if rows[i].open && open == nil {
			open = &rows[i]
		}
	}
	return open, rows, nil
}

// unsettled returns the session's rows whose run has not been seen to
// finish, oldest first: those that can be waiting on an approval or cut
// off by a crash.
func (x *index) unsettled(ctx context.Context, session string) ([]row, error) {
	rows, err := scanRows(x.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM proxy_calls WHERE session = ? AND settled_at = 0 ORDER BY id`, session))
	if err != nil {
		return nil, fmt.Errorf("proxy: index: %w", err)
	}
	return rows, nil
}

// attempts returns the key's rows that can hold an attempt at its tool,
// newest first: every row but those settled with none.
func (x *index) attempts(ctx context.Context, session, key string) ([]row, error) {
	rows, err := scanRows(x.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM proxy_calls WHERE session = ? AND request_key = ? AND outcome != ? ORDER BY id DESC`,
		session, key, outcomeNone))
	if err != nil {
		return nil, fmt.Errorf("proxy: index: %w", err)
	}
	return rows, nil
}

// recent returns the session's rows unsettled or settled at or after
// since, newest first, at most limit of them.
func (x *index) recent(ctx context.Context, session string, since time.Time, limit int) ([]row, error) {
	rows, err := scanRows(x.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM proxy_calls WHERE session = ? AND (settled_at = 0 OR settled_at >= ?) ORDER BY id DESC LIMIT ?`,
		session, stamp(since), limit))
	if err != nil {
		return nil, fmt.Errorf("proxy: index: %w", err)
	}
	return rows, nil
}

// errRace means another call changed the key's open row between the read
// and the claim; the caller reads again.
var errRace = errors.New("proxy: the request's index row changed")

// claim opens a row for a new run of the key, closing prev, the open row
// read before, whose run has finished, in the same transaction, which
// holds the write lock: the open row is read again inside it and must
// still be prev, or none when prev is nil, or errRace is returned and
// nothing is written.
func (x *index) claim(ctx context.Context, session, key, tool string, class agentrt.SideEffect, runID string, prev *row, now time.Time) error {
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	defer tx.Rollback()
	var openID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM proxy_calls WHERE session = ? AND request_key = ? AND open = 1`, session, key).Scan(&openID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if prev != nil {
			return errRace
		}
	case err != nil:
		return fmt.Errorf("proxy: index: %w", err)
	case prev == nil || openID != prev.id:
		return errRace
	}
	if prev != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_calls SET open = 0, closed_at = ? WHERE id = ? AND open = 1`, stamp(now), prev.id); err != nil {
			return fmt.Errorf("proxy: index: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO proxy_calls (session, request_key, tool, side_effect, run_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		session, key, tool, string(class), runID, stamp(now)); err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	return nil
}

// settle records that a row's run was seen in a terminal status, and what
// its last attempt came to. A row settled before outcomes were kept gets
// its outcome now.
func (x *index) settle(ctx context.Context, id int64, o outcome, now time.Time) error {
	if _, err := x.db.ExecContext(ctx, `UPDATE proxy_calls SET settled_at = CASE WHEN settled_at = 0 THEN ? ELSE settled_at END, outcome = ? WHERE id = ? AND outcome = 0`,
		stamp(now), o, id); err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	return nil
}

// settleEmpty settles a row whose run has not begun, so the scans stop
// reading it, unless the run began meanwhile. The runtime's runs table is
// read here, by primary key, inside the statement, because no Store method
// can make the check and the write one step.
func (x *index) settleEmpty(ctx context.Context, id int64, now time.Time) error {
	if _, err := x.db.ExecContext(ctx, `UPDATE proxy_calls SET settled_at = ?, outcome = ? WHERE id = ? AND settled_at = 0 AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.id = proxy_calls.run_id)`,
		stamp(now), outcomeNone, id); err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	return nil
}

// unsettle undoes settleEmpty for a run that has now begun, so the scans
// read it until it finishes. It writes only when the row was settled.
func (x *index) unsettle(ctx context.Context, runID string) error {
	r, err := x.byRun(ctx, runID)
	if err != nil {
		return err
	}
	if r.settled.IsZero() {
		return nil
	}
	if _, err := x.db.ExecContext(ctx, `UPDATE proxy_calls SET settled_at = 0, outcome = 0 WHERE id = ?`, r.id); err != nil {
		return fmt.Errorf("proxy: index: %w", err)
	}
	return nil
}

// byRun returns the row of a run.
func (x *index) byRun(ctx context.Context, runID string) (*row, error) {
	rows, err := scanRows(x.db.QueryContext(ctx, `SELECT `+rowColumns+` FROM proxy_calls WHERE run_id = ?`, runID))
	if err != nil {
		return nil, fmt.Errorf("proxy: index: %w", err)
	}
	if len(rows) == 0 {
		return nil, agentrt.ErrNotFound
	}
	return &rows[0], nil
}
