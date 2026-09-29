package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// TestUserCacheDir_IsCreatedPerUserOnly: a fixed name in the shared
// temporary directory let another account on the same machine pre-create
// or read the database first; the per-user cache directory must not.
func TestUserCacheDir_IsCreatedPerUserOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_CACHE_HOME", "")
	}
	dir, err := userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(dir, filepath.Join("agentrt")) {
		t.Fatalf("dir = %q, want it to end in .../agentrt", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("dir mode = %o, want 0700", perm)
		}
	}
}

func TestDefaultDBPath_IsFixedUnderTheCacheDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_CACHE_HOME", "")
	}
	a, err := defaultDBPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := defaultDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("default path is not fixed: %q != %q", a, b)
	}
	if filepath.Base(a) != "live.db" {
		t.Fatalf("base = %q, want live.db", filepath.Base(a))
	}
}

// TestRun_CreatesTheDatabaseFile0600 checks the non-model part of `go run
// ./examples/live`: the database it opens, WAL files included, must not be
// group/world-readable, whatever the umask. The example no longer chmods
// the file after opening it: OpenStore creates it owner-only.
func TestRun_CreatesTheDatabaseFile0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) && p != path {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s mode = %o, want owner-only", p, perm)
		}
	}
}

// TestSignalContext_CancelsOnASignal drives the watch with a fake notify
// instead of sending this process a real signal, and checks that the
// returned context is cancelled once one arrives, and not before.
func TestSignalContext_CancelsOnASignal(t *testing.T) {
	var sig chan<- os.Signal
	fakeNotify := func(c chan<- os.Signal, want ...os.Signal) {
		sig = c
		if len(want) != 2 || want[0] != os.Interrupt {
			t.Errorf("notify was asked for %v, want SIGINT first", want)
		}
	}
	ctx, stop := signalContext(context.Background(), fakeNotify)
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("context cancelled before any signal arrived")
	default:
	}
	sig <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled after the signal")
	}
}

// TestSignalContext_StopEndsTheWatchAndCancels checks the other way out:
// stop, called with no signal ever sent, still leaves the watching goroutine
// free to exit and still cancels the context, as a normal, uninterrupted
// exit needs.
func TestSignalContext_StopEndsTheWatchAndCancels(t *testing.T) {
	notified := false
	fakeNotify := func(c chan<- os.Signal, _ ...os.Signal) { notified = true }
	ctx, stop := signalContext(context.Background(), fakeNotify)
	stop()
	if !notified {
		t.Fatal("signalContext must register with notify unconditionally")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop must cancel the context")
	}
	stop() // calling it twice must not panic or hang
}

func TestResumable_FalseForARunNeverRecorded(t *testing.T) {
	store, err := agentrt.OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if resumable(context.Background(), store, newRunID()) {
		t.Fatal("an id nothing ever wrote must not be reported resumable")
	}
}

func TestNewRunID_LooksLikeTheDriversOwn(t *testing.T) {
	a, b := newRunID(), newRunID()
	if a == b || len(a) != 32 {
		t.Fatalf("ids = %q, %q, want distinct 32-char hex ids", a, b)
	}
}
