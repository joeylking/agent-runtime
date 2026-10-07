package agentrt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The chain's encoding is a contract with anyone who recomputes it, so one
// link is pinned: seven netstrings, SHA-256, hex.
func TestEventHash_Golden(t *testing.T) {
	if got := eventHash("", 1, "r", "", "2026-10-01T09:00:00Z", "run.started", "{}"); got != "215d2d4feca5e8542594838002508a9d6739ae568ac25b15fa28058b3943b60f" {
		t.Fatalf("first link %s", got)
	}
}

// chainedRuns runs n short runs on st, each with a tool call and a
// completion, and returns how many events they wrote.
func chainedRuns(t *testing.T, st *Store, n int) int64 {
	t.Helper()
	for i := range n {
		d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{call("read", fmt.Sprintf(`{"n":%d}`, i)), {Kind: DecideComplete}}}, Policy: DefaultPolicy(),
			Tools: []Tool{counted("read", ReadOnly)}})
		if run, err := d.Start(context.Background(), "g", Limits{MaxSteps: 5, MaxConsecutiveToolFailures: 3, LoopThreshold: 3}); err != nil || run.Status != StatusCompleted {
			t.Fatalf("run %d: %v %s", i, err, run.Status)
		}
	}
	seq, _, err := st.ChainHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// Every event is chained to the one before it across runs, the head is the
// newest event's stored hash, and a range is checked on its own.
func TestVerifyEvents_IntactChainAcrossRuns(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	if rep, err := st.VerifyEvents(ctx, 0, 0); err != nil || rep.Checked != 0 || rep.Break != nil {
		t.Fatalf("an empty database: %+v %v", rep, err)
	}
	n := chainedRuns(t, st, 3)
	rep, err := st.VerifyEvents(ctx, 0, 0)
	if err != nil || rep.Break != nil || rep.Checked != n || rep.From != 1 || rep.To != n {
		t.Fatalf("whole chain: %+v %v", rep, err)
	}
	seq, head, _ := st.ChainHead(ctx)
	if seq != n || head != rep.Hash || len(head) != 64 {
		t.Fatalf("head %d %s, report %+v", seq, head, rep)
	}
	if h, err := st.EventHash(ctx, n); err != nil || h != head {
		t.Fatalf("EventHash(%d) = %s %v", n, h, err)
	}
	if _, err := st.EventHash(ctx, n+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EventHash past the head: %v", err)
	}
	// Each stored hash is the link of its own fields after the previous one.
	rows, err := st.DB().Query(`SELECT seq, run_id, step_id, at, type, payload_json, hash FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for rows.Next() {
		var seq int64
		var runID, stepID, at, typ, payload, hash string
		rows.Scan(&seq, &runID, &stepID, &at, &typ, &payload, &hash)
		if want := eventHash(prev, seq, runID, stepID, at, typ, payload); hash != want {
			t.Fatalf("event %d hash %s, want %s", seq, hash, want)
		}
		prev = hash
	}
	rows.Close()
	if rep, err := st.VerifyEvents(ctx, 5, 8); err != nil || rep.Break != nil || rep.Checked != 4 || rep.From != 5 || rep.To != 8 {
		t.Fatalf("range: %+v %v", rep, err)
	}
}

// One altered payload, one deleted event, and two events swapped each fail
// verification at the first event that no longer chains, and checking
// stops there. An event deleted from the end leaves an intact chain, which
// only a head kept elsewhere shows is short.
func TestVerifyEvents_FindsTheFirstAlteredEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		alter  func(db *sql.DB, k int64) error
		breaks func(k int64) int64
		detail string
	}{
		{"altered payload", func(db *sql.DB, k int64) error {
			_, err := db.Exec(`UPDATE events SET payload_json = '{"forged":true}' WHERE seq = ?`, k)
			return err
		}, func(k int64) int64 { return k }, "stored hash"},
		{"deleted event", func(db *sql.DB, k int64) error {
			_, err := db.Exec(`DELETE FROM events WHERE seq = ?`, k)
			return err
		}, func(k int64) int64 { return k + 1 }, "missing"},
		{"swapped events", func(db *sql.DB, k int64) error {
			for _, q := range []string{
				`UPDATE events SET seq = -1 WHERE seq = ?1`,
				`UPDATE events SET seq = ?1 WHERE seq = ?1 + 1`,
				`UPDATE events SET seq = ?1 + 1 WHERE seq = -1`,
			} {
				if _, err := db.Exec(q, k); err != nil {
					return err
				}
			}
			return nil
		}, func(k int64) int64 { return k }, "stored hash"},
		{"altered first event", func(db *sql.DB, _ int64) error {
			_, err := db.Exec(`UPDATE events SET at = '2000-01-01T00:00:00Z' WHERE seq = 1`)
			return err
		}, func(int64) int64 { return 1 }, "stored hash"},
		{"deleted first event", func(db *sql.DB, _ int64) error {
			_, err := db.Exec(`DELETE FROM events WHERE seq = 1`)
			return err
		}, func(int64) int64 { return 2 }, "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := memStore(t)
			ctx := context.Background()
			n := chainedRuns(t, st, 2)
			k := n / 2
			if err := tc.alter(st.DB(), k); err != nil {
				t.Fatal(err)
			}
			rep, err := st.VerifyEvents(ctx, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.breaks(k)
			if rep.Break == nil || rep.Break.Seq != want || rep.To != want || !strings.Contains(rep.Break.Detail, tc.detail) {
				t.Fatalf("break %+v, report %+v; want one at seq %d", rep.Break, rep, want)
			}
			if rep.Break.Expected == rep.Break.Found || len(rep.Break.Expected) != 64 {
				t.Fatalf("expected %q found %q", rep.Break.Expected, rep.Break.Found)
			}
		})
	}

	st := memStore(t)
	ctx := context.Background()
	n := chainedRuns(t, st, 2)
	kept, _, _ := st.ChainHead(ctx)
	keptHash, _ := st.EventHash(ctx, kept)
	st.DB().Exec(`DELETE FROM events WHERE seq = ?`, n)
	if rep, err := st.VerifyEvents(ctx, 0, 0); err != nil || rep.Break != nil || rep.To != n-1 {
		t.Fatalf("a chain cut at the end verifies on its own: %+v %v", rep, err)
	}
	if seq, _, _ := st.ChainHead(ctx); seq == kept {
		t.Fatal("the head did not move")
	}
	if _, err := st.EventHash(ctx, kept); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the kept head %d (%s) is still there: %v", kept, keptHash, err)
	}
	// The next event chains to what is there and takes a fresh seq, so
	// the gap shows.
	chainedRuns(t, st, 1)
	if rep, _ := st.VerifyEvents(ctx, 0, 0); rep.Break == nil || rep.Break.Seq != n+1 {
		t.Fatalf("after the cut end, a new event: %+v", rep)
	}
}

// A field longer than one page carries is read and hashed in chunks, and
// alters the chain like any other.
func TestVerifyEvents_ReadsLongFieldsInChunks(t *testing.T) {
	st := memStore(t)
	ctx := context.Background()
	big := `{"pad":"` + strings.Repeat("é", MaxPageText) + `"}`
	if err := st.tx(ctx, nil, func(t *txn) error {
		if err := t.insertRun(ctx, Run{ID: "r", Goal: "g", Status: StatusCompleted, Limits: DefaultLimits()}); err != nil {
			return err
		}
		for _, p := range []string{`{}`, big, `{"after":1}`} {
			if err := t.appendEvent(ctx, Event{RunID: "r", At: time.Unix(1, 0), Type: "x", Payload: []byte(p)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.VerifyEvents(ctx, 0, 0); err != nil || rep.Break != nil || rep.Checked != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	st.DB().Exec(`UPDATE events SET payload_json = replace(payload_json, 'éé"', 'eé"') WHERE seq = 2`)
	if rep, err := st.VerifyEvents(ctx, 0, 0); err != nil || rep.Break == nil || rep.Break.Seq != 2 {
		t.Fatalf("a change past the first chunk: %+v %v", rep, err)
	}
}

// Many runs appending at once, from several stores on one file as several
// processes would, leave one intact chain: each transaction reads the head
// under the write lock it began with.
func TestVerifyEvents_ConcurrentWritersKeepTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	const stores, runsPer = 4, 6
	var wg sync.WaitGroup
	errs := make(chan error, stores*runsPer)
	var opened []*Store
	for range stores {
		st, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		opened = append(opened, st)
	}
	defer func() {
		for _, st := range opened {
			st.Close()
		}
	}()
	for i, st := range opened {
		for j := range runsPer {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d, err := NewDriver(Config{Store: st, Agent: &listAgent{decisions: []Decision{call("read", `{"n":1}`), call("read", `{"n":2}`), call("read", `{"n":3}`), {Kind: DecideComplete}}},
					Policy: DefaultPolicy(), Tools: []Tool{counted("read", ReadOnly)}})
				if err != nil {
					errs <- err
					return
				}
				run, err := d.StartWithID(context.Background(), fmt.Sprintf("run-%d-%d", i, j), "g", Limits{MaxSteps: 10, MaxConsecutiveToolFailures: 3, LoopThreshold: 5})
				if err == nil && run.Status != StatusCompleted {
					err = fmt.Errorf("run %s %s", run.Status, run.ReasonDetail)
				}
				errs <- err
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	var total int64
	opened[0].DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&total)
	rep, err := opened[0].VerifyEvents(ctx, 0, 0)
	if err != nil || rep.Break != nil || rep.Checked != total || total != stores*runsPer*20 {
		t.Fatalf("%d events, report %+v %v", total, rep, err)
	}
	var runs int
	opened[0].DB().QueryRow(`SELECT COUNT(DISTINCT run_id) FROM events`).Scan(&runs)
	if runs != stores*runsPer {
		t.Fatalf("%d runs in the chain", runs)
	}
}

// v040 is a database as v0.4.0 wrote it: the first five migrations, with a
// finished run, a run waiting on an approved publish, and their events
// interleaved, one of them longer than a page carries.
func v040(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`}
	for i, m := range migrations[:5] {
		stmts = append(stmts, m, fmt.Sprintf(`INSERT INTO schema_migrations VALUES (%d, '2026-10-01T00:00:00Z')`, i+1))
	}
	limits := `{"max_steps":10,"max_consecutive_tool_failures":3,"loop_threshold":5}`
	stmts = append(stmts,
		`INSERT INTO runs (id, goal, status, reason, limits_json, step_count, created_at, started_at, finished_at, result_json, model_calls, input_tokens) VALUES ('done', 'g', 'COMPLETED', 'goal_completed', '`+limits+`', 2, '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z', '2026-10-01T00:01:00Z', '{"ok":true}', 3, 1200)`,
		`INSERT INTO runs (id, goal, status, limits_json, step_count, created_at, started_at) VALUES ('waiting', 'publish', 'WAITING_FOR_APPROVAL', '`+limits+`', 1, '2026-10-01T00:00:01Z', '2026-10-01T00:00:01Z')`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, observation_json, observation_hash, started_at, finished_at) VALUES ('d0', 'done', 0, 'done', '{"kind":"tool_call","tool":"read","args":{"n":1}}', '{"outcome":"allow","reason":"ok"}', '{"kind":"tool_result","content":{"n":1}}', 'h', '2026-10-01T00:00:00Z', '2026-10-01T00:00:01Z')`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, started_at, finished_at) VALUES ('d1', 'done', 1, 'done', '{"kind":"complete","result":{"ok":true}}', '2026-10-01T00:00:01Z', '2026-10-01T00:00:02Z')`,
		`INSERT INTO steps (id, run_id, idx, status, decision_json, policy_json, started_at) VALUES ('w0', 'waiting', 0, 'awaiting_approval', '{"kind":"tool_call","tool":"publish","args":{"n":1}}', '{"outcome":"require_approval","reason":"remote","kind":"remote_mutation","capability":{},"presentation":{}}', '2026-10-01T00:00:01Z')`,
	)
	events := [][5]string{
		{"done", "", "2026-10-01T00:00:00Z", "run.created", `{"goal":"g","limits":` + limits + `}`},
		{"done", "", "2026-10-01T00:00:00Z", "run.started", `{}`},
		{"waiting", "", "2026-10-01T00:00:01Z", "run.created", `{"goal":"publish","limits":` + limits + `}`},
		{"done", "d0", "2026-10-01T00:00:00.5Z", "step.decided", `{"kind":"tool_call","tool":"read","args":{"n":1},"reason":"` + strings.Repeat("long ", 2000) + `"}`},
		{"waiting", "", "2026-10-01T00:00:01Z", "run.started", `{}`},
		{"waiting", "w0", "2026-10-01T00:00:01.25Z", "step.policy", `{"outcome":"require_approval","reason":"remote"}`},
		{"done", "", "2026-10-01T00:01:00Z", "run.finished", `{"detail":"","reason":"goal_completed","status":"COMPLETED","steps":2}`},
	}
	for _, e := range events {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO events (run_id, step_id, at, type, payload_json) VALUES ('%s', '%s', '%s', '%s', '%s')`, e[0], e[1], e[2], e[3], e[4]))
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// A database written by v0.4.0 is refused by OpenExisting until it is
// migrated, and migrates with its runs intact and every stored event
// chained in seq order, the same hashes on any open; its steps record no
// spec or policy. A run it left waiting resumes onto the chain, and the
// resumed decision records the spec it ran with.
func TestStore_V040DatabaseMigratesWithChainedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v040.db")
	v040(t, path)
	for _, ro := range []bool{true, false} {
		if _, err := OpenExisting(path, ro); !errors.Is(err, ErrSchemaVersion) {
			t.Fatalf("OpenExisting(readOnly=%v) of a v0.4.0 database: %v", ro, err)
		}
	}
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var version int
	st.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version)
	if version != chainMigration || len(migrations) != chainMigration {
		t.Fatalf("version %d of %d", version, len(migrations))
	}
	done, err := st.GetRun(ctx, "done")
	if err != nil || done.Status != StatusCompleted || string(done.Result) != `{"ok":true}` || done.ModelCalls != 3 || done.Usage.InputTokens != 1200 || done.StepCount != 2 {
		t.Fatalf("finished run: %+v %v", done, err)
	}
	steps, err := st.ListSteps(ctx, "done")
	if err != nil || len(steps) != 2 || steps[0].Decision.Tool != "read" || steps[0].SpecHash != "" || steps[0].PolicyID != "" || steps[0].DecodeError != "" {
		t.Fatalf("migrated steps: %+v %v", steps, err)
	}
	rep, err := st.VerifyEvents(ctx, 0, 0)
	if err != nil || rep.Break != nil || rep.Checked != 7 {
		t.Fatalf("backfilled chain: %+v %v", rep, err)
	}
	hashes := func(st *Store) string {
		var out []string
		rows, _ := st.DB().Query(`SELECT hash FROM events ORDER BY seq`)
		defer rows.Close()
		for rows.Next() {
			var h string
			rows.Scan(&h)
			out = append(out, h)
		}
		return strings.Join(out, " ")
	}
	backfilled := hashes(st)
	if first := strings.Fields(backfilled)[0]; first != eventHash("", 1, "done", "", "2026-10-01T00:00:00Z", "run.created", `{"goal":"g","limits":{"max_steps":10,"max_consecutive_tool_failures":3,"loop_threshold":5}}`) {
		t.Fatalf("first backfilled hash %s", first)
	}
	st.Close()

	// The same file built again migrates to the same hashes, and opening
	// the migrated file again changes none.
	again := filepath.Join(t.TempDir(), "again.db")
	v040(t, again)
	other, err := OpenStore(again)
	if err != nil {
		t.Fatal(err)
	}
	if got := hashes(other); got != backfilled {
		t.Fatalf("a second migration of the same events gave other hashes:\n%s\n%s", got, backfilled)
	}
	other.Close()
	if st, err = OpenStore(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := hashes(st); got != backfilled {
		t.Fatal("reopening the migrated database changed its hashes")
	}
	if ro, err := OpenExisting(path, true); err != nil {
		t.Fatalf("OpenExisting after the migration: %v", err)
	} else {
		ro.Close()
	}

	if _, err := st.DB().Exec(`INSERT INTO approvals (id, run_id, step_id, kind, capability_json, presentation_json, request_json, hash, status, created_at, decided_at, decided_by) VALUES ('a0', 'waiting', 'w0', 'remote_mutation', '{}', '{}', '{}', '', 'approved', '2026-10-01T00:00:02Z', '2026-10-01T00:00:03Z', 'op')`); err != nil {
		t.Fatal(err)
	}
	publish := counted("publish", RemoteMutation)
	req := ToolRequest{RunID: "waiting", StepID: "w0", Spec: publish.spec, Args: []byte(`{"n":1}`)}
	h, err := approvalHash("remote_mutation", []byte(`{}`), []byte(`{}`), req)
	if err != nil {
		t.Fatal(err)
	}
	reqJSON, _ := toJSON(req)
	st.DB().Exec(`UPDATE approvals SET request_json = ?, hash = ? WHERE id = 'a0'`, string(reqJSON), h)
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{}, {Kind: DecideComplete}}}, Policy: SideEffectPolicy{RemoteMutation: Allow}, Tools: []Tool{publish}})
	run, err := d.Resume(ctx, "waiting")
	if err != nil || run.Status != StatusCompleted || publish.calls.Load() != 1 {
		t.Fatalf("resume: %+v %v, %d calls", run, err, publish.calls.Load())
	}
	if rep, err := st.VerifyEvents(ctx, 0, 0); err != nil || rep.Break != nil || rep.Checked <= 7 {
		t.Fatalf("after the resume: %+v %v", rep, err)
	}
	events, _ := st.ListEvents(ctx, "waiting")
	var resumed string
	for _, e := range events {
		if e.Type == EventStepPolicy && e.Seq > 7 {
			resumed = string(e.Payload)
		}
	}
	e, _ := newSpecEntry(publish.spec)
	if !strings.Contains(resumed, `"spec_hash":"`+e.hash+`"`) {
		t.Fatalf("the resumed step.policy %s does not name the spec %s", resumed, e.hash)
	}
	if _, err := st.ToolSpec(ctx, e.hash); err != nil {
		t.Fatalf("the resumed spec was not stored: %v", err)
	}
}
