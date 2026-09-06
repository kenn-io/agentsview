// Package pathutil provides shared normalization for user-supplied local paths.
package pathutil

import (
	"errors"
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

// ResolveAbsolute resolves a local path before it enters runtime configuration.
// Unlike EvalSymlinks, it retains the destination of a link whose target has
// not been created yet, including missing children below a linked directory.
func ResolveAbsolute(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %w", path, err)
	}
	links := 0
	var resolve func(string) (string, error)
	resolve = func(path string) (string, error) {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		parent, err = resolve(parent)
		if err != nil {
			return "", err
		}
		path = filepath.Join(parent, filepath.Base(path))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		links++
		if links > 255 {
			return "", fmt.Errorf("resolving path %q: too many symbolic links", path)
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(parent, target)
		}
		return resolve(target)
	}
	return resolve(absolute)
}
