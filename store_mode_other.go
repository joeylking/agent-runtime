//go:build !unix

package agentrt

import "os"

// modeRules is false where permission bits do not describe who may read
// and write a file, as on Windows: the store's mode rules are skipped.
const modeRules = false

func fileOwner(os.FileInfo) (int, bool) { return 0, false }
