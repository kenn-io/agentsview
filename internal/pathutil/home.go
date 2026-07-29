// Package pathutil provides shared normalization for user-supplied local paths.
package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExpandHome replaces a leading home shorthand ("~" or "~/") with the
// current user's home directory. Named-user shorthands and tildes elsewhere
// in the path are left unchanged.
func ExpandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if len(path) > 1 && !os.IsPathSeparator(path[1]) {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
