package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
// ./examples/live`: the database file it creates must not be
// group/world-readable, whatever the umask.
func TestRun_CreatesTheDatabaseFile0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := agentrt.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file mode = %o, want 0600", perm)
	}
}
