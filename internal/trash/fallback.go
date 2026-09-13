package trash

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// fallbackBackend keeps trashed files inside the agentsview data dir.
// It backs the Windows platform (where the recycle bin needs
// non-stdlib APIs), data dirs whose system trash is unavailable, and
// cross-device moves that the system backend cannot perform.
type fallbackBackend struct {
	dataDir string
}

func (b fallbackBackend) name() string { return BackendFallback }

func (b fallbackBackend) destRoot() string { return filepath.Join(b.dataDir, "trash", "files") }

func (b fallbackBackend) prepare() error {
	return os.MkdirAll(b.destRoot(), 0o755)
}

func (b fallbackBackend) move(src, name string) (string, error) {
	dst := filepath.Join(b.destRoot(), name)
	if err := os.Rename(src, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return "", err
		}
		// Same-process copy + delete keeps the trash usable when the
		// source lives on another volume.
		if err := copyPath(src, dst); err != nil {
			return "", err
		}
		if err := os.RemoveAll(src); err != nil {
			os.Remove(dst)
			return "", err
		}
	}
	return dst, nil
}

func (b fallbackBackend) cleanup(trashedPath string) error { return nil }

// Store helpers shared by all backends live below.

func appendManifest(path string, item Item) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := marshalItem(item)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// readManifest parses every well-formed line. A corrupt tail (for
// example after a crash mid-append) is tolerated: lines that do not
// parse are skipped so the rest of the manifest stays usable.
func readManifest(path string) ([]Item, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return parseManifest(data), nil
}

func rewriteManifest(path string, entries []Item) error {
	var buf []byte
	for _, item := range entries {
		line, err := marshalItem(item)
		if err != nil {
			return err
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// strconvFormatInt avoids importing strconv for one call site.
func strconvFormatInt(v int64) string {
	return fmt.Sprintf("%d", v)
}
