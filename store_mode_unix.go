//go:build unix

package agentrt

import (
	"os"
	"syscall"
)

// modeRules reports that file modes mean what the store's rules assume:
// an owner, a group, and everyone else.
const modeRules = true

// fileOwner is the uid that owns a file.
func fileOwner(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
