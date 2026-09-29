//go:build !unix

package mcp

// The descendant test is Unix-only; these keep TestMain's modes defined.
func grandparent() {}
func grandchild()  {}
