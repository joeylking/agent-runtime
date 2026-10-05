//go:build unix

package mcp

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// OwnedPrivately refuses a file another user owns or that its group or
// anyone else can write. path is only named in the error; info is the
// file's, from a Stat of the file as opened.
func OwnedPrivately(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s has mode %s, which lets its group or others write it: chmod go-w it", path, info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot tell who owns it", path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s (mode %s) is owned by uid %d, not by the current user", path, info.Mode().Perm(), st.Uid)
	}
	return nil
}
