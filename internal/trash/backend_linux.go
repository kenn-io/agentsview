//go:build linux

package trash

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// freedesktopBackend implements the freedesktop.org Trash
// specification: trashed files live under <data home>/Trash/files/
// with matching <data home>/Trash/info/<name>.trashinfo metadata so
// file managers can restore them too.
type freedesktopBackend struct {
	filesDir string
	infoDir  string
}

func resolveBackend(dataDir string) backend {
	if b := freedesktopBackendFor(dataDir); b != nil {
		return *b
	}
	return fallbackBackend{dataDir: dataDir}
}

func freedesktopBackendFor(dataDir string) *freedesktopBackend {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	return &freedesktopBackend{
		filesDir: filepath.Join(dataHome, "Trash", "files"),
		infoDir:  filepath.Join(dataHome, "Trash", "info"),
	}
}

func (b freedesktopBackend) name() string { return BackendFreedesktop }

func (b freedesktopBackend) destRoot() string { return b.filesDir }

func (b freedesktopBackend) prepare() error {
	for _, dir := range []string{b.filesDir, b.infoDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (b freedesktopBackend) move(src, name string) (string, error) {
	dst := filepath.Join(b.filesDir, name)
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	info := filepath.Join(b.infoDir, name+".trashinfo")
	payload := fmt.Sprintf(
		"[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		trashinfoPath(src),
		time.Now().Format("2006-01-02T15:04:05"),
	)
	if err := os.WriteFile(info, []byte(payload), 0o600); err != nil {
		// The file is already trashed; restoring it is still
		// possible through the manifest, so surface the error to
		// the caller without losing the move.
		return "", fmt.Errorf("writing trashinfo: %w", err)
	}
	return dst, nil
}

func (b freedesktopBackend) cleanup(trashedPath string) error {
	info := filepath.Join(
		b.infoDir,
		filepath.Base(trashedPath)+".trashinfo",
	)
	if err := os.Remove(info); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
