package segfile

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/ledger"
)

// tmpAttempts is jilog's unique-tmp retry bound (segment.rs:247).
const tmpAttempts = 16

var tmpCounter atomic.Uint64

// PublishNew ports publish_new (segment.rs:328-384): write a fsynced,
// uniquely named tmp sibling, hard-link it to path (which fails instead of
// replacing), remove the tmp and fsync the directory. An existing file is
// never replaced: identical content is AlreadyIdentical, different content
// is an integrity error and both files are left for the operator. The
// target filesystem must support hard links.
func PublishNew(path string, seg ledger.Segment) (ledger.PublishOutcome, error) {
	data, err := ledger.MarshalSegmentFile(seg)
	if err != nil {
		return ledger.Published, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ledger.Published, err
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ledger.Published, err
	}
	fsyncDirBestEffort(dir)
	fsyncDirBestEffort(filepath.Dir(dir))

	tmp, f, err := createNewWithRetry(func(int) string {
		return fmt.Sprintf("%s.%d.%d.%d.tmp", abs, os.Getpid(), tmpCounter.Add(1)-1, time.Now().Nanosecond())
	}, tmpAttempts)
	if err != nil {
		return ledger.Published, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return ledger.Published, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return ledger.Published, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return ledger.Published, err
	}

	linkErr := os.Link(tmp, abs)
	switch {
	case linkErr == nil:
		cleanup := os.Remove(tmp)
		if err := syncDir(dir); err != nil {
			return ledger.Published, err
		}
		if cleanup != nil {
			return ledger.Published, fmt.Errorf(
				"segment published to %s but its tmp alias %s could not be removed: %w — remove the alias manually",
				abs, tmp, cleanup)
		}
		return ledger.Published, nil
	case errors.Is(linkErr, fs.ErrExist):
		_ = os.Remove(tmp)
		existing, err := ReadFile(abs)
		if err != nil {
			return ledger.Published, err
		}
		if existing.ContentMatches(seg) {
			return ledger.AlreadyIdentical, nil
		}
		return ledger.Published, fmt.Errorf(
			"%w: no-clobber publish: %s already exists with DIFFERENT content — not overwriting",
			ledger.ErrIntegrity, abs)
	default:
		_ = os.Remove(tmp)
		return ledger.Published, fmt.Errorf(
			"no-clobber publish of %s failed at hard_link: %w — publishing requires a filesystem with hard-link support (exFAT/FAT and some SMB/NFS/FUSE mounts do not have it)",
			abs, linkErr)
	}
}

// createNewWithRetry ports create_new_with_retry (segment.rs:189-209): an
// existing candidate is skipped, never opened or truncated.
func createNewWithRetry(namegen func(attempt int) string, attempts int) (string, *os.File, error) {
	for attempt := range attempts {
		candidate := namegen(attempt)
		f, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return candidate, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not create a unique tmp file after %d attempts: %w", attempts, fs.ErrExist)
}

// syncDir makes a directory entry durable. Windows cannot fsync a
// directory, so there it is a no-op.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func fsyncDirBestEffort(dir string) {
	if err := syncDir(dir); err != nil {
		log.Printf("ledger: failed to fsync directory %s: %v", dir, err)
	}
}
