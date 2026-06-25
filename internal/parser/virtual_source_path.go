package parser

import (
	"path/filepath"
	"strings"
)

// VirtualSourcePath identifies one logical source inside a shared physical
// container path, for example one conversation row inside a SQLite database.
func VirtualSourcePath(containerPath, sourceID string) string {
	return containerPath + "#" + sourceID
}

// ParseVirtualSourcePath splits a path created by VirtualSourcePath.
func ParseVirtualSourcePath(path string) (string, string, bool) {
	idx := strings.LastIndex(path, "#")
	if idx < 0 {
		return "", "", false
	}
	containerPath, sourceID := path[:idx], path[idx+1:]
	if containerPath == "" || sourceID == "" {
		return "", "", false
	}
	return containerPath, sourceID, true
}

// ParseVirtualSourcePathForBase splits a virtual source path and verifies that
// the physical container has the expected base filename.
func ParseVirtualSourcePathForBase(
	path string,
	baseName string,
) (string, string, bool) {
	containerPath, sourceID, ok := ParseVirtualSourcePath(path)
	if !ok || filepath.Base(containerPath) != baseName {
		return "", "", false
	}
	return containerPath, sourceID, true
}
