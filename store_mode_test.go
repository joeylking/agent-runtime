//go:build unix

package agentrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStore_ModesInAChildProcess is the child half of
// TestStore_FilesAreOwnerOnlyUnderAnyUmask: it sets the umask it is given,
// which is global to a process, writes a run, and reports the modes of the
// database and its WAL files while the store is open.
func TestStore_ModesInAChildProcess(t *testing.T) {
	path, mask := os.Getenv("AGENTRT_MODE_PATH"), os.Getenv("AGENTRT_MODE_UMASK")
	if path == "" {
		t.Skip("run by TestStore_FilesAreOwnerOnlyUnderAnyUmask")
	}
	m, _ := strconv.ParseUint(mask, 8, 32)
	syscall.Umask(int(m))
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := mustDriver(t, Config{Store: st, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete}}}})
	if _, err := d.Start(context.Background(), "g", leaseLimits()); err != nil {
		t.Fatal(err)
	}
	var modes []string
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		modes = append(modes, fmt.Sprintf("%o", fi.Mode().Perm()))
	}
	fmt.Println("MODES", strings.Join(modes, " "))
}

// A new database and the WAL files SQLite makes beside it are readable and
// writable by their owner only, whatever the process's umask: the approval
// hash has no secret in it, so anyone who can write the file could forge
// an approval, and the WAL holds every goal and argument.
func TestStore_FilesAreOwnerOnlyUnderAnyUmask(t *testing.T) {
	for _, mask := range []string{"022", "002", "000"} {
		path := filepath.Join(t.TempDir(), "runs.db")
		cmd := exec.Command(os.Args[0], "-test.run", "^TestStore_ModesInAChildProcess$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "AGENTRT_MODE_PATH="+path, "AGENTRT_MODE_UMASK="+mask)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("umask %s: %v\n%s", mask, err, out)
		}
		if !strings.Contains(string(out), "MODES 600 600 600\n") {
			t.Fatalf("umask %s:\n%s", mask, out)
		}
	}
}

// A database, or a WAL file beside it, that group or others can write is
// refused by both openers with ErrInsecureMode naming it; so is a WAL file
// another user owns. Nothing is changed.
func TestStore_RefusesADatabaseOthersCanWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	open := map[string]func() error{
		"OpenStore":              func() error { s, err := OpenStore(path); closeIf(s); return err },
		"OpenExisting read-only": func() error { s, err := OpenExisting(path, true); closeIf(s); return err },
		"OpenExisting":           func() error { s, err := OpenExisting(path, false); closeIf(s); return err },
	}
	for _, tc := range []struct {
		file string
		mode fs.FileMode
	}{{path, 0o664}, {path, 0o606}, {path + "-wal", 0o620}} {
		if tc.file != path {
			os.WriteFile(tc.file, nil, 0o600)
		}
		os.Chmod(tc.file, tc.mode)
		for name, fn := range open {
			var insecure ErrInsecureMode
			err := fn()
			if !errors.As(err, &insecure) || insecure.Path != tc.file || insecure.Mode.Perm() != tc.mode || !strings.Contains(err.Error(), "chmod 600 "+tc.file) {
				t.Fatalf("%s with %s at %o: %v", name, filepath.Base(tc.file), tc.mode, err)
			}
			if fi, _ := os.Stat(tc.file); fi.Mode().Perm() != tc.mode {
				t.Fatalf("%s changed a refused file to %v", name, fi.Mode())
			}
		}
		os.Chmod(tc.file, 0o600)
		if tc.file != path {
			os.Remove(tc.file)
		}
	}
}

func closeIf(s *Store) {
	if s != nil {
		s.Close()
	}
}

// A database others can only read, as every earlier release created one,
// is made owner-only, WAL files included, by a writer that owns it; a
// read-only open changes nothing.
func TestStore_TightensADatabaseOthersCanRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	live, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	completed := mustDriver(t, Config{Store: live, Agent: &listAgent{decisions: []Decision{{Kind: DecideComplete}}}})
	if _, err := completed.Start(context.Background(), "g", leaseLimits()); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal", path + "-shm"}
	for _, f := range files {
		os.Chmod(f, 0o644)
	}
	ro, err := OpenExisting(path, true)
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
	for _, f := range files {
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o644 {
			t.Fatalf("a read-only open changed %s to %v", f, fi.Mode())
		}
	}
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, f := range files {
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v", f, fi.Mode())
		}
	}
}

// OpenExisting opens the file the operator named, not what a link there
// points to.
func TestOpenExisting_RefusesASymbolicLink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{true, false} {
		if _, err := OpenExisting(link, ro); !errors.Is(err, ErrSymlink) || !strings.Contains(err.Error(), link) {
			t.Fatalf("readOnly=%v: %v", ro, err)
		}
	}
}

// A tool's input schema is compiled from itself and the standard
// metaschemas only: a reference to a file, a URL, or a pipe fails the
// registration at once with the reference named, and nothing is read or
// fetched; references within the schema, and the standard $schema
// values, work as before.
func TestNewDriver_SchemaReferencesOutsideItselfAreRefused(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.json")
	os.WriteFile(secret, []byte(`{"type":"string","const":"SECRET"}`), 0o600)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	hits := 0
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.Write([]byte(`{"type":"string"}`)) }))
	defer web.Close()
	register := func(schema string) error {
		done := make(chan error, 1)
		go func() {
			_, err := NewDriver(Config{Store: memStore(t), Agent: &listAgent{}, Policy: DefaultPolicy(),
				Tools: []Tool{&stubTool{spec: ToolSpec{Name: "t", InputSchema: json.RawMessage(schema), SideEffect: ReadOnly, Timeout: time.Second}}}})
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			f, _ := os.OpenFile(fifo, os.O_WRONLY, 0)
			f.Write([]byte(`{}`))
			f.Close()
			return errors.New("registration blocked")
		}
	}
	for name, ref := range map[string]string{
		"file":        "file://" + secret,
		"http":        web.URL + "/x.json",
		"pipe":        "file://" + fifo,
		"relative":    "secret.json",
		"$schema url": "",
	} {
		schema := `{"type":"object","properties":{"x":{"$ref":"` + ref + `"}}}`
		if name == "$schema url" {
			ref = web.URL + "/meta.json"
			schema = `{"$schema":"` + ref + `","type":"object"}`
		}
		err := register(schema)
		if err == nil || strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "outside itself") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if hits != 0 {
		t.Fatalf("%d fetches", hits)
	}
	for name, schema := range map[string]string{
		"$defs":   `{"type":"object","$defs":{"s":{"type":"string"}},"properties":{"x":{"$ref":"#/$defs/s"}}}`,
		"2020-12": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`,
		"draft-7": `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","definitions":{"s":{"type":"string"}},"properties":{"x":{"$ref":"#/definitions/s"}}}`,
		"$id":     `{"$id":"https://example.com/tool.json","type":"object","$defs":{"s":{"type":"string"}},"properties":{"x":{"$ref":"#/$defs/s"}}}`,
	} {
		if err := register(schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	cs, err := compileSchema("t", json.RawMessage(`{"type":"object","$defs":{"s":{"type":"string"}},"properties":{"x":{"$ref":"#/$defs/s"}}}`))
	if err != nil || cs.validate(json.RawMessage(`{"x":1}`)) == nil || cs.validate(json.RawMessage(`{"x":"a"}`)) != nil {
		t.Fatalf("an internal reference no longer validates: %v", err)
	}
}
