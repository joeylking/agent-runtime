//go:build !unix

package mcp

import "io/fs"

// OwnedPrivately checks nothing where file modes do not describe an owner, a
// group, and everyone else; access control lists decide there.
func OwnedPrivately(string, fs.FileInfo) error { return nil }
