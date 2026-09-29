package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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
// silently doing the job: pin and run must each ask for one.
func TestServer_SetsAConnectTimeoutExplicitly(t *testing.T) {
	s := server("index.js", t.TempDir())
	if s.ConnectTimeout <= 0 {
		t.Fatal("server() must set ConnectTimeout explicitly, not rely on the default")
	}
}

// TestServer_RunsThePinnedPackageWithNode: the example ran
// `npx -y @modelcontextprotocol/server-filesystem`, which executes whatever
// the registry serves that day. It now runs the installed entry point of the
// exact version package-lock.json pins, and nothing fetches at start.
func TestServer_RunsThePinnedPackageWithNode(t *testing.T) {
	dir := t.TempDir()
	s := server("/abs/index.js", dir)
	if want := []string{"node", "/abs/index.js", dir}; !slices.Equal(s.Command, want) {
		t.Fatalf("Command = %q, want %q", s.Command, want)
	}
	if filepath.ToSlash(serverEntry) != "examples/mcp/node_modules/@modelcontextprotocol/server-filesystem/dist/index.js" {
		t.Fatalf("serverEntry = %s", serverEntry)
	}
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version   string `json:"version"`
			Integrity string `json:"integrity"`
		} `json:"packages"`
	}
	for file, v := range map[string]any{"package.json": &pkg, "package-lock.json": &lock} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
	version := pkg.Dependencies["@modelcontextprotocol/server-filesystem"]
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		t.Fatalf("package.json pins %q, want an exact version", version)
	}
	locked := lock.Packages["node_modules/@modelcontextprotocol/server-filesystem"]
	if locked.Version != version || !strings.HasPrefix(locked.Integrity, "sha512-") {
		t.Fatalf("package-lock.json has %+v, want version %s with its sha512 integrity", locked, version)
	}
	for name, p := range lock.Packages {
		if name != "" && p.Integrity == "" {
			t.Errorf("package-lock.json: %s has no integrity hash", name)
		}
	}
	// From this directory the root-relative entry point is not there, and
	// the error says how to install it.
	if _, err := installedServer(); err == nil || !strings.Contains(err.Error(), "npm ci --ignore-scripts") {
		t.Fatalf("installedServer = %v, want the install command", err)
	}
}

// TestReadOwned_RefusesAFileOthersCanWrite: whoever can write the manifest
// or the rules chooses how every tool is classified, so neither is read when
// its group or anyone else can write it.
func TestReadOwned_RefusesAFileOthersCanWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	dir := t.TempDir()
	rules := filepath.Join(dir, "rules.json")
	manifest := filepath.Join(dir, "manifest.json")
	os.WriteFile(rules, []byte(`{"read_file":{"side_effect":"read_only"}}`), 0o600)
	os.WriteFile(manifest, []byte(`{"server":"fs","tools":{}}`), 0o600)
	for _, mode := range []os.FileMode{0o600, 0o644, 0o444} {
		os.Chmod(rules, mode)
		os.Chmod(manifest, mode)
		if _, err := readRules(rules); err != nil {
			t.Errorf("rules at %o: %v", mode, err)
		}
		if _, err := readManifest(manifest); err != nil {
			t.Errorf("manifest at %o: %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		os.Chmod(rules, mode)
		os.Chmod(manifest, mode)
		if _, err := readRules(rules); err == nil || !strings.Contains(err.Error(), rules) || !strings.Contains(err.Error(), mode.String()) {
			t.Errorf("rules at %o: %v, want a refusal naming the path and the mode", mode, err)
		}
		if _, err := readManifest(manifest); err == nil || !strings.Contains(err.Error(), manifest) {
			t.Errorf("manifest at %o: %v, want a refusal naming the path", mode, err)
		}
	}
	// The shipped rules must pass the check as a checkout leaves them.
	if _, err := readRules("rules.json"); err != nil {
		t.Errorf("the shipped rules.json: %v", err)
	}
}

// TestOneLine_EscapesWhatTheServerChose: the pin listing printed a server's
// descriptions raw, so an escape sequence in one could rewrite the terminal.
func TestOneLine_EscapesWhatTheServerChose(t *testing.T) {
	got := oneLine("d\x1b]8;;http://evil.example\x07click\x1b]8;;\x07 \x1b[2K\rfs: registered shell", 200)
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("oneLine = %q, want the controls escaped", got)
	}
}
