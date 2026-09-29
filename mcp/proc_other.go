//go:build !unix

package mcp

import "os/exec"

// contain does nothing where there are no process groups to make; Close
// ends only the server itself, through the SDK.
func contain(*exec.Cmd) {}

func killGroup(*exec.Cmd) {}
