package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const (
	claudeReservationDirName = ".agentsview-capture-reservations"
	reservationGuardName     = "registry.lock"
	reservationGuardWait     = time.Second
)

type claudeSessionReservation struct {
	dir  string
	path string
	lock *flock.Flock
}

func reserveClaudeSession(
	ctx context.Context, providerRoot, sessionID string,
) (*claudeSessionReservation, error) {
	dir, err := prepareClaudeReservationDirectory(providerRoot)
	if err != nil {
		return nil, err
	}
	guard, err := lockReservationGuard(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = guard.Unlock() }()

	key := sha256.Sum256([]byte(sessionID))
	path := filepath.Join(dir, hex.EncodeToString(key[:])+".lock")
	reservation := flock.New(path)
	locked, err := reservation.TryLock()
	if err != nil {
		return nil, fmt.Errorf("reserving Claude session: %w", err)
	}
	if !locked {
		return nil, errors.New(
			"claude session is already reserved by another capture",
		)
	}
	if err := secureReservationFile(path); err != nil {
		_ = reservation.Unlock()
		return nil, err
	}
	return &claudeSessionReservation{dir: dir, path: path, lock: reservation}, nil
}

func prepareClaudeReservationDirectory(providerRoot string) (string, error) {
	if err := ensureClaudeProviderRoot(providerRoot); err != nil {
		return "", err
	}
	physicalRoot, err := filepath.EvalSymlinks(providerRoot)
	if err != nil {
		return "", fmt.Errorf("resolving Claude provider root: %w", err)
	}
	registry := filepath.Join(filepath.Dir(physicalRoot), claudeReservationDirName)
	if err := verifyCaptureParentSafety(registry); err != nil {
		return "", fmt.Errorf("validating Claude reservation parent: %w", err)
	}
	if err := ensureSecureReservationDirectory(registry); err != nil {
		return "", err
	}
	rootKey := sha256.Sum256([]byte(physicalRoot))
	dir := filepath.Join(registry, hex.EncodeToString(rootKey[:]))
	if err := ensureSecureReservationDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func ensureSecureReservationDirectory(dir string) error {
	err := createSecureCaptureDirectory(dir)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating Claude reservation directory: %w", err)
	}
	if err := secureCaptureDirectory(dir); err != nil {
		return fmt.Errorf("securing Claude reservation directory: %w", err)
	}
	return nil
}

func lockReservationGuard(ctx context.Context, dir string) (*flock.Flock, error) {
	guard := flock.New(filepath.Join(dir, reservationGuardName))
	guardCtx, cancel := context.WithTimeout(ctx, reservationGuardWait)
	defer cancel()
	locked, err := guard.TryLockContext(guardCtx, 10*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("locking Claude reservation registry: %w", err)
	}
	if !locked {
		return nil, errors.New("claude reservation registry is busy")
	}
	if err := secureReservationFile(guard.Path()); err != nil {
		_ = guard.Unlock()
		return nil, err
	}
	return guard, nil
}

func secureReservationFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking Claude reservation file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("claude reservation path is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing Claude reservation file: %w", err)
	}
	return nil
}

func (r *claudeSessionReservation) close() {
	if r == nil || r.lock == nil {
		return
	}
	guard, err := lockReservationGuard(context.Background(), r.dir)
	if err != nil {
		_ = r.lock.Unlock()
		r.lock = nil
		return
	}
	defer func() { _ = guard.Unlock() }()
	_ = r.lock.Unlock()
	r.lock = nil
	_ = os.Remove(r.path)
}
