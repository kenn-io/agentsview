package artifact

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

const folderExchangeLockRetryDelay = 100 * time.Millisecond

type folderExchangeLock struct {
	file *os.File
}

func (t *folderTransport) acquireExchangeLockLocked(
	ctx context.Context,
) (*folderExchangeLock, error) {
	file, err := openFolderExchangeLockFile(t.root)
	if err != nil {
		return nil, fmt.Errorf("acquiring artifact folder exchange lock: %w", err)
	}
	for {
		locked, lockErr := tryLockFolderFile(file)
		if lockErr != nil {
			return nil, errors.Join(
				fmt.Errorf("acquiring artifact folder exchange lock: %w", lockErr),
				file.Close(),
			)
		}
		if locked {
			return &folderExchangeLock{file: file}, nil
		}

		timer := time.NewTimer(folderExchangeLockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-timer.C:
		}
	}
}

func openFolderExchangeLockFile(root *os.Root) (*os.File, error) {
	for range 8 {
		before, err := root.Lstat(folderExchangeLockName)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if err == nil && !before.Mode().IsRegular() {
			return nil, errors.New("artifact folder exchange lock is not a regular file")
		}

		flags := os.O_RDWR
		if errors.Is(err, fs.ErrNotExist) {
			before = nil
			flags |= os.O_CREATE | os.O_EXCL
		}
		file, openErr := root.OpenFile(folderExchangeLockName, flags, 0o600)
		if errors.Is(openErr, fs.ErrExist) || errors.Is(openErr, fs.ErrNotExist) {
			continue
		}
		if openErr != nil {
			return nil, openErr
		}

		opened, statErr := file.Stat()
		if statErr != nil {
			return nil, errors.Join(statErr, file.Close())
		}
		after, statErr := root.Lstat(folderExchangeLockName)
		if statErr != nil {
			return nil, errors.Join(statErr, file.Close())
		}
		if !opened.Mode().IsRegular() ||
			!after.Mode().IsRegular() ||
			(before != nil && !os.SameFile(before, opened)) ||
			!os.SameFile(opened, after) {
			return nil, errors.Join(
				errors.New("artifact folder exchange lock changed while opening"),
				file.Close(),
			)
		}
		return file, nil
	}
	return nil, errors.New("artifact folder exchange lock changed while opening")
}

func (l *folderExchangeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return errors.Join(unlockFolderFile(file), file.Close())
}
