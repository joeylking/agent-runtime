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

// TestUserCacheDir_IsCreatedPerUserOnly: the sandbox, the manifest, and the
// database used to default to fixed names in the shared temporary
// directory, which let another account on the same machine pre-create or
// read any of the three. The per-user cache directory must not be.
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

// TestSandbox_CreatesA0700DirectoryAndA0600READMEOnly covers the
// non-network part of `pin`/`run`: the sandbox directory and the seed
// README they create must not be group/world-accessible.
func TestSandbox_CreatesA0700DirectoryAndA0600READMEOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	dir := filepath.Join(t.TempDir(), "sandbox")
	if err := sandbox(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("sandbox dir mode = %o, want 0700", perm)
	}
	rm, err := os.Stat(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := rm.Mode().Perm(); perm != 0o600 {
		t.Fatalf("README.md mode = %o, want 0600", perm)
	}

	// An existing sandbox is left exactly as it is: sandbox must not touch
	// a directory or README a previous run already created.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "custom" {
		t.Fatalf("sandbox overwrote an existing README.md: %q", got)
	}
}

// TestDispatch_DefaultPathsAreUnderTheCacheDirectory checks that pin and
// run resolve their -dir, -manifest, and -db defaults from the same
// per-user cache directory rather than the shared temporary one, without
// touching Node or Ollama.
func TestDispatch_DefaultPathsAreUnderTheCacheDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_CACHE_HOME", "")
	}
	cache, err := userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	// dispatch("run") resolves its flag defaults (which creates the cache
	// directory) and only then reads the manifest pin writes; with none on
	// disk it fails there, before ever touching Node or Ollama.
	err = dispatch(context.Background(), []string{"run"})
	if err == nil || !strings.Contains(err.Error(), "run `go run ./examples/mcp pin` first") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(cache); statErr != nil {
		t.Fatalf("userCacheDir was not created by dispatch: %v", statErr)
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

// TestServer_SetsAConnectTimeoutExplicitly guards against the default
// silently doing the job: pin and run must each ask for one, long enough for
// npx to fetch the package on a first run.
func TestServer_SetsAConnectTimeoutExplicitly(t *testing.T) {
	s := server(t.TempDir())
	if s.ConnectTimeout <= 0 {
		t.Fatal("server() must set ConnectTimeout explicitly, not rely on the default")
	}
	if s.ConnectTimeout < 30*time.Second {
		t.Fatalf("ConnectTimeout = %s, want it generous enough for npx's first fetch", s.ConnectTimeout)
	}
}
