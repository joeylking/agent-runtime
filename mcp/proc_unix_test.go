//go:build unix

package mcp

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	agentrt "github.com/joeylking/agent-runtime"
)

// grandparent is a stdio server that first starts a grandchild which ignores
// the end of its stdin and SIGTERM, and records the grandchild's pid in the
// file AGENTRT_MCP_TEST_PIDFILE names.
func grandparent() {
	gc := exec.Command(os.Args[0])
	gc.Env = append(os.Environ(), childMode+"=grandchild")
	if err := gc.Start(); err != nil {
		os.Exit(4)
	}
	os.WriteFile(os.Getenv("AGENTRT_MCP_TEST_PIDFILE"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	childServer(false)
}

func grandchild() {
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	time.Sleep(10 * time.Minute)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestStdio_CloseKillsTheServersDescendants: Close ended the server the SDK
// started and nothing else, so a descendant such as the node process under
// npx outlived the session. The server now has a process group of its own,
// and Close kills it after the SDK's shutdown.
func TestStdio_CloseKillsTheServersDescendants(t *testing.T) {
	skipWithoutUnix(t)
	pidfile := filepath.Join(t.TempDir(), "pid")
	server := Server{Name: "child", Command: []string{os.Args[0]},
		Env:            []string{childMode + "=grandparent", "AGENTRT_MCP_TEST_PIDFILE=" + pidfile},
		DefaultTimeout: 5 * time.Second, ConnectTimeout: 20 * time.Second}
	for _, phase := range []string{"pin", "load"} {
		os.Remove(pidfile)
		var closeConn func()
		if phase == "pin" {
			if _, err := Pin(context.Background(), server); err != nil {
				t.Fatalf("Pin: %v", err)
			}
			closeConn = func() {}
		} else {
			m, err := Pin(context.Background(), server)
			if err != nil {
				t.Fatalf("Pin: %v", err)
			}
			os.Remove(pidfile)
			_, rep, err := Load(context.Background(), server, m, Rules{"env": {SideEffect: agentrt.ReadOnly}})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			closeConn = func() { rep.Connection.Close() }
		}
		raw, err := os.ReadFile(pidfile)
		if err != nil {
			t.Fatalf("%s: the grandchild's pid: %v", phase, err)
		}
		pid, _ := strconv.Atoi(string(raw))
		closeConn()
		deadline := time.Now().Add(5 * time.Second)
		for alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s: grandchild %d outlived Close", phase, pid)
		}
	}
}
