//go:build darwin

package trash

import (
	"os"
	"path/filepath"
)

// darwinBackend moves files into ~/.Trash.
type darwinBackend struct {
	root string
}

func resolveBackend(dataDir string) backend {
	if b := darwinBackendFor(dataDir); b != nil {
		return *b
	}
	return fallbackBackend{dataDir: dataDir}
}

func darwinBackendFor(dataDir string) *darwinBackend {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".Trash")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil
	}
	return &darwinBackend{root: root}
}

func (b darwinBackend) name() string { return BackendDarwin }

func (b darwinBackend) prepare() error { return nil }

func (b darwinBackend) move(src, name string) (string, error) {
	dst := filepath.Join(b.root, name)
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (b darwinBackend) cleanup(trashedPath string) error { return nil }
