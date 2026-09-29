package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	err = dispatch([]string{"run"})
	if err == nil || !strings.Contains(err.Error(), "run `go run ./examples/mcp pin` first") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(cache); statErr != nil {
		t.Fatalf("userCacheDir was not created by dispatch: %v", statErr)
	}
}
