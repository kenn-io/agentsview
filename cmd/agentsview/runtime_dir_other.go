//go:build !windows

package main

func runtimeDir(dataDir string) string {
	return dataDir
}
