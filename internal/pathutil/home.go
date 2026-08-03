// Package pathutil provides shared normalization for user-supplied local paths.
package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// LocalComparisonKey returns an absolute path key for comparing local paths.
// It preserves case-sensitive platforms and folds case on Windows.
func LocalComparisonKey(path string) (string, error) {
	key, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %w", path, err)
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key, nil
}
