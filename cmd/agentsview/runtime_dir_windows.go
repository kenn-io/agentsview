//go:build windows

package main

import "path/filepath"

func runtimeDir(dataDir string) string {
	return filepath.Join(dataDir, "run")
}
