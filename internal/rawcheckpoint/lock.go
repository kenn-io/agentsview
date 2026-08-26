package rawcheckpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/gofrs/flock"
)

var ErrStoreLocked = errors.New("rawcheckpoint: checkpoint or spool is already in use")

func acquireStoreLocks(
	ctx context.Context,
	checkpointPath, spoolDir string,
) ([]*flock.Flock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checkpointTarget, err := canonicalLockTarget(checkpointPath)
	if err != nil {
		return nil, err
	}
	spoolTarget, err := filepath.EvalSymlinks(spoolDir)
	if err != nil {
		return nil, fmt.Errorf("rawcheckpoint: resolve spool lock target: %s",
			checkpointFilesystemError(err))
	}
	paths := []string{
		checkpointTarget + ".rawcheckpoint.lock",
		filepath.Join(spoolTarget, ".rawcheckpoint.lock"),
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	locks := make([]*flock.Flock, 0, len(paths))
	for _, path := range paths {
		lock := flock.New(path)
		locked, err := lock.TryLock()
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("rawcheckpoint: acquire process lock: %s",
					checkpointFilesystemError(err)),
				releaseStoreLocks(locks),
			)
		}
		if !locked {
			return nil, errors.Join(ErrStoreLocked, releaseStoreLocks(locks))
		}
		locks = append(locks, lock)
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, errors.Join(
				fmt.Errorf("rawcheckpoint: secure process lock: %s",
					checkpointFilesystemError(err)),
				releaseStoreLocks(locks),
			)
		}
	}
	return locks, nil
}

func canonicalLockTarget(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: resolve checkpoint lock target: %s",
			checkpointFilesystemError(err))
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("rawcheckpoint: resolve checkpoint lock parent: %s",
			checkpointFilesystemError(err))
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func releaseStoreLocks(locks []*flock.Flock) error {
	var errs []error
	for _, lock := range slices.Backward(locks) {
		if err := lock.Unlock(); err != nil {
			errs = append(errs, fmt.Errorf("rawcheckpoint: release process lock: %s",
				checkpointFilesystemError(err)))
		}
	}
	return errors.Join(errs...)
}
