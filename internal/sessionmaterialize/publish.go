package sessionmaterialize

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

type installFile struct {
	path string
	data []byte
}

// DestinationDiffersError identifies an existing file with different bytes.
type DestinationDiffersError struct {
	Path string
}

func (e *DestinationDiffersError) Error() string {
	return "destination differs: " + e.Path
}

// publishFiles preflights every destination before publishing in order. Callers
// put the primary transcript last, after all its dependencies.
func publishFiles(ctx context.Context, files []installFile) (changed bool, err error) {
	temps := make([]string, len(files))
	defer func() {
		for _, name := range temps {
			if name != "" {
				_ = os.Remove(name)
			}
		}
		if err != nil && changed {
			err = fmt.Errorf("some files installed; retry to reuse them: %w", err)
		}
	}()
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if same, err := identicalDestination(ctx, file); err != nil {
			return false, err
		} else if same {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file.path), 0o700); err != nil {
			return false, err
		}
		f, err := os.CreateTemp(filepath.Dir(file.path), ".materialize-*")
		if err != nil {
			return false, err
		}
		temps[i] = f.Name()
		_, err = f.Write(file.data)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return false, err
		}
	}
	for i, file := range files {
		if temps[i] == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		if err := atomicfile.RenameNoReplace(temps[i], file.path); err != nil {
			if same, cmp := identicalDestination(ctx, file); cmp != nil {
				return changed, cmp
			} else if !same {
				return changed, err
			}
			continue
		}
		changed = true
	}
	return changed, nil
}

func identicalDestination(ctx context.Context, file installFile) (bool, error) {
	info, err := os.Lstat(file.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("destination is not a regular file: %s", file.path)
	}
	if info.Size() != int64(len(file.data)) {
		return false, &DestinationDiffersError{Path: file.path}
	}
	old, _, err := readFile(ctx, file.path, int64(len(file.data)))
	if errors.Is(err, errSourceSize) {
		return false, &DestinationDiffersError{Path: file.path}
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(old, file.data) {
		return false, &DestinationDiffersError{Path: file.path}
	}
	return true, nil
}
