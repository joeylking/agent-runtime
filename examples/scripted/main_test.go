package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestUserCacheDir_IsCreatedPerUserOnly: a fixed or predictable name in the
// shared temporary directory let another account on the same machine
// pre-create or read the database first; the per-user cache directory must
// not be reachable that way.
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

func TestDefaultDBPath_IsUnderTheCacheDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if runtime.GOOS != "windows" {
		t.Setenv("XDG_CACHE_HOME", "")
	}
	cache, err := userCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := defaultDBPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != cache {
		t.Fatalf("default path %q is not under the cache directory %q", path, cache)
	}
}

// TestRun_EndToEndCreatesA0600DatabaseUnderTheCacheDirectory runs the whole
// example (it makes no network calls) and checks the database it created
// on the way is neither group- nor world-readable.
func TestRun_EndToEndCreatesA0600DatabaseUnderTheCacheDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	path := filepath.Join(t.TempDir(), "scripted-test.db")
	if err := run(path); err != nil {
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
