package remotesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// canonicalMirrorLockPath returns the canonical path used for mirror locking.
// It creates the lock parent and lock file before resolving identity. Those are
// the same harmless artifacts lock acquisition already needs, and creating them
// here lets preflight identify aliases even when the configured data directory
// did not previously exist. Mirror extraction continues to use the configured
// root; only lock identity is canonicalized.
func canonicalMirrorLockPath(mirrorRoot string) (string, error) {
	absRoot, err := filepath.Abs(mirrorRoot)
	if err != nil {
		return "", fmt.Errorf("make mirror lock path absolute: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	lockPath := absRoot + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return "", fmt.Errorf("create mirror lock parent: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("open mirror lock identity %s: %w", lockPath, err)
	}
	resolved, resolveErr := filepath.EvalSymlinks(lockPath)
	if resolveErr != nil {
		resolveErr = fmt.Errorf("resolve mirror lock path %s: %w", lockPath, resolveErr)
	} else {
		resolved, resolveErr = platformCanonicalLockPath(lockFile, resolved)
	}
	closeErr := lockFile.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close mirror lock identity %s: %w", lockPath, closeErr)
	}
	if err := errors.Join(resolveErr, closeErr); err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
