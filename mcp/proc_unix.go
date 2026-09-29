//go:build unix && !linux

package mcp

import (
	"os/exec"
	"syscall"
)

// contain starts a stdio server in a process group of its own, so that Close
// can kill its descendants too.
func contain(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills what is left of a started server's process group.
func killGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
