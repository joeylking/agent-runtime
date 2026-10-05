package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// TestReadRules_ConvertsTheOperatorsFormat: every field of the file format
// reaches the Rule, and a timeout is a duration string.
func TestReadRules_ConvertsTheOperatorsFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(path, []byte(`{"write_file":{"side_effect":"local_mutation","timeout":"10s","terminal":true,
		"description":"Write a file.","deny":["mode"],"fixed":{"encoding":"\"utf-8\""},"rename":"write","allow_hint_mismatch":true}}`), 0o600)
	rules, err := ReadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	r := rules["write_file"]
	if r.SideEffect != agentrt.LocalMutation || r.Timeout != 10*time.Second || !r.Terminal || r.Description != "Write a file." ||
		len(r.Deny) != 1 || r.Deny[0] != "mode" || string(r.Fixed["encoding"]) != `"\"utf-8\""` || r.Rename != "write" || !r.AllowHintMismatch {
		t.Fatalf("rule = %+v", r)
	}
}

// TestReadRules_RefusesWhatItDoesNotKnow: a misspelled field or a timeout
// that is not a duration is an error naming the file, never ignored.
func TestReadRules_RefusesWhatItDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": `{"read_file":{"side_effect":"read_only","denny":["path"]}}`,
		"bad timeout":   `{"read_file":{"side_effect":"read_only","timeout":"5"}}`,
		"not JSON":      `{"read_file":`,
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := ReadRules(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: %v, want an error naming the file", name, err)
		}
	}
}

// TestReadOwned_RefusesAFileOthersCanWrite: whoever can write a manifest or
// a rules file chooses how every tool is classified, so neither is read
// when its group or anyone else can write it, and a directory is not a
// file.
func TestReadOwned_RefusesAFileOthersCanWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes only")
	}
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	raw, _ := json.Marshal(Manifest{Server: "fs", Tools: map[string]PinnedTool{}})
	os.WriteFile(manifest, raw, 0o600)
	for _, mode := range []os.FileMode{0o600, 0o644, 0o444} {
		os.Chmod(manifest, mode)
		if m, err := ReadManifest(manifest); err != nil || m.Server != "fs" {
			t.Errorf("manifest at %o: %+v %v", mode, m, err)
		}
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o666} {
		os.Chmod(manifest, mode)
		if _, err := ReadManifest(manifest); err == nil || !strings.Contains(err.Error(), manifest) || !strings.Contains(err.Error(), mode.String()) {
			t.Errorf("manifest at %o: %v, want a refusal naming the path and the mode", mode, err)
		}
	}
	if _, err := ReadOwned(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory: %v", err)
	}
}
