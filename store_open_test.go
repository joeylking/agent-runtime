package agentrt_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
	"github.com/joeylking/agent-runtime/scripted"
)

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func completedRun(t *testing.T, st *agentrt.Store) agentrt.Run {
	t.Helper()
	d, err := agentrt.NewDriver(agentrt.Config{Store: st, Agent: &scripted.Agent{Decisions: []agentrt.Decision{scripted.Complete(`{}`)}}, Policy: agentrt.DefaultPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.Start(context.Background(), "g", limits(5, 3))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// '?', '#', and '%' in a path name the file they spell, in both openers.
func TestStore_PathWithURICharacters(t *testing.T) {
	dir := t.TempDir()
	name := "we?ird#na%25me %3F.db"
	path := filepath.Join(dir, name)
	st, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	run := completedRun(t, st)
	st.Close()
	for _, f := range files(t, dir) {
		if !strings.HasPrefix(f, name) {
			t.Fatalf("created %q, want only %q and its WAL files", f, name)
		}
	}
	for _, ro := range []bool{true, false} {
		st, err := agentrt.OpenExisting(path, ro)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetRun(context.Background(), run.ID); err != nil {
			t.Fatalf("readOnly=%v: %v", ro, err)
		}
		st.Close()
	}
}

// A consumer's read-then-write transaction on DB() waits for another
// process's writer instead of failing when it upgrades: transactions
// begin IMMEDIATE and wait on the busy timeout.
func TestStore_ReadThenWriteTransactionWaitsForTheWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	a, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	if _, err := a.DB().Exec(`CREATE TABLE notes (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	writer, err := a.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO notes VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		tx, err := b.DB().BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		var n int
		err = tx.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&n)
		close(read)
		if err == nil {
			_, err = tx.Exec(`INSERT INTO notes VALUES (?)`, n+1)
		}
		if err != nil {
			tx.Rollback()
			done <- err
			return
		}
		done <- tx.Commit()
	}()
	// The writer commits once the other transaction has read, or after a
	// pause if that transaction is waiting to begin.
	select {
	case <-read:
	case <-time.After(300 * time.Millisecond):
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("read-then-write transaction: %v", err)
	}
	var n int
	a.DB().QueryRow(`SELECT MAX(n) FROM notes`).Scan(&n)
	if n != 2 {
		t.Fatalf("max = %d, want the second write to have seen the first", n)
	}
}

func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// OpenExisting never creates a file, and a read-only store reads a WAL
// database, with a writer live or not and in a read-only directory,
// without changing the database file.
func TestOpenExisting_ReadOnlyNeverWrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := agentrt.OpenExisting(filepath.Join(dir, "missing.db"), true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := agentrt.OpenExisting(filepath.Join(dir, "missing.db"), false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file, read-write: %v", err)
	}
	if got := files(t, dir); len(got) != 0 {
		t.Fatalf("OpenExisting created %v", got)
	}

	path := filepath.Join(dir, "runs.db")
	writer, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	run := completedRun(t, writer)
	// The writer is live, so the -wal and -shm files exist.
	if got := files(t, dir); len(got) != 3 {
		t.Fatalf("files with a live writer = %v", got)
	}
	ro, err := agentrt.OpenExisting(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ro.GetRun(context.Background(), run.ID); err != nil || got.Status != agentrt.StatusCompleted {
		t.Fatalf("read with a live writer: %+v %v", got, err)
	}
	if _, err := ro.DB().Exec(`DELETE FROM events`); err == nil {
		t.Fatal("a read-only store accepted a write")
	}
	// The writer's later commits are visible to the reader.
	second := completedRun(t, writer)
	if _, err := ro.GetRun(context.Background(), second.ID); err != nil {
		t.Fatalf("reader missed the writer's commit: %v", err)
	}
	ro.Close()
	writer.Close()

	// No writer, and a directory nothing may be created in.
	after := files(t, dir)
	before := digest(t, path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	ro, err = agentrt.OpenExisting(path, true)
	if err != nil {
		t.Fatalf("read-only directory: %v", err)
	}
	runs, err := ro.ListRuns(context.Background())
	if err != nil || len(runs) != 2 {
		t.Fatalf("read-only directory: %d runs, %v", len(runs), err)
	}
	ro.Close()
	os.Chmod(dir, 0o755)
	if digest(t, path) != before {
		t.Fatal("a read-only store changed the database file")
	}
	if got := files(t, dir); strings.Join(got, ",") != strings.Join(after, ",") {
		t.Fatalf("files %v became %v", after, got)
	}
}
