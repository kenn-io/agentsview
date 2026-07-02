// ABOUTME: Root-containment checks used to skip provider changed-path
// ABOUTME: classification for agents whose roots cannot contain the path.
package sync

import (
	"path/filepath"
	"strings"
)

// changedPathWithinAnyRoot reports whether path lies within at least
// one of roots (see changedPathWithinRoot).
func changedPathWithinAnyRoot(path string, roots []string) bool {
	for _, root := range roots {
		if changedPathWithinRoot(path, root) {
			return true
		}
	}
	return false
}

// changedPathWithinRoot reports whether path is root itself or lies
// under it, including the "root#member" virtual form used for
// sessions stored inside container files. It mirrors the containment
// clauses of storedSourcePathHintQuery.
func changedPathWithinRoot(path, root string) bool {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return false
	}
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return strings.HasPrefix(path, root+string(filepath.Separator)) ||
		strings.HasPrefix(path, root+"#")
}
