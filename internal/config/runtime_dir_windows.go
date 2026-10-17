//go:build windows

package config

import "path/filepath"

// RuntimeDir returns the directory for daemon records and locks.
func RuntimeDir(dataDir string) string {
	return filepath.Join(dataDir, "run")
}
