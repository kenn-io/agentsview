//go:build !windows

package config

import "os"

func readInstallationIDFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
