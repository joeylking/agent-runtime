//go:build !unix

package main

import "io/fs"

// ownedPrivately checks nothing where file modes do not describe an owner, a
// group, and everyone else; access control lists decide there.
func ownedPrivately(string, fs.FileInfo) error { return nil }
