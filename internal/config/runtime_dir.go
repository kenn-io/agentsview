package config

import (
	"path/filepath"
	"runtime"
)

// RuntimeDir returns the directory for daemon records and locks.
func RuntimeDir(dataDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dataDir, "run")
	}
	return dataDir
}
