//go:build !windows

package config

// RuntimeDir returns the directory for daemon records and locks.
func RuntimeDir(dataDir string) string {
	return dataDir
}
