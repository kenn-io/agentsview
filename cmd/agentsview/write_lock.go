package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

const writeOwnerLockFile = "db.write.lock"

// vectorsWriteLockFile guards direct (non-daemon) writes to vectors.db —
// `embeddings build/activate/retire` take it via tryAcquireNamedLock so two
// concurrent direct-mode invocations cannot race each other. It is separate
// from writeOwnerLockFile because the two resources (sessions.db vs
// vectors.db) are written independently.
const vectorsWriteLockFile = "vectors.write.lock"

type writeOwnerLock struct {
	path string
	lock *flock.Flock
}

func writeOwnerLockPath(dataDir string) string {
	return filepath.Join(dataDir, writeOwnerLockFile)
}

func acquireWriteOwnerLock(
	ctx context.Context,
	dataDir string,
) (*writeOwnerLock, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return tryAcquireWriteOwnerLock(dataDir)
}

func tryAcquireWriteOwnerLock(dataDir string) (*writeOwnerLock, error) {
	return tryAcquireNamedLock(dataDir, writeOwnerLockFile)
}

// tryAcquireNamedLock acquires an exclusive flock named filename inside
// dataDir. It backs both tryAcquireWriteOwnerLock (db.write.lock, guarding
// direct sessions.db writes) and the embeddings CLI's direct path
// (vectors.write.lock, guarding direct vectors.db writes), so two direct
// (non-daemon) writers targeting the same resource cannot race each other.
// OS flock semantics release the lock when the owning process exits or
// crashes, which is the direct-writer recovery path after stale runtime
// records are ignored.
func tryAcquireNamedLock(dataDir, filename string) (*writeOwnerLock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir for write lock: %w", err)
	}

	path := filepath.Join(dataDir, filename)
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring write lock %s: %w", path, err)
	}
	if !locked {
		return nil, writeOwnerLockHeldError{path: path}
	}
	return &writeOwnerLock{path: path, lock: lock}, nil
}

func (l *writeOwnerLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("releasing write lock %s: %w", l.path, err)
	}
	return nil
}

// Release yields the write lock for a worker maintenance pass while keeping the
// lock handle so Reacquire can retake the same path. Unlike Close, the owner is
// expected to reacquire.
func (l *writeOwnerLock) Release() error {
	if l == nil || l.lock == nil {
		return errors.New("release on nil write lock")
	}
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("releasing write lock %s: %w", l.path, err)
	}
	return nil
}

// Reacquire retakes the write lock after a worker maintenance pass. It fails if
// another process grabbed the lock while it was released.
func (l *writeOwnerLock) Reacquire() error {
	if l == nil || l.lock == nil {
		return errors.New("reacquire on nil write lock")
	}
	locked, err := l.lock.TryLock()
	if err != nil {
		return fmt.Errorf("reacquiring write lock %s: %w", l.path, err)
	}
	if !locked {
		return writeOwnerLockHeldError{path: l.path}
	}
	return nil
}

type writeOwnerLockHeldError struct {
	path string
}

func (e writeOwnerLockHeldError) Error() string {
	return fmt.Sprintf(
		"write lock %s is held by another process; run `agentsview daemon stop`, "+
			"wait for the daemon idle timeout, or retry after the offline "+
			"operation finishes",
		e.path,
	)
}
