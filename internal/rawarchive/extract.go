package rawarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// Extract writes retained native files below their stable root IDs. Multiple
// contents for the same path require an explicit choice; never guess a winner.
func (a *Archive) Extract(ctx context.Context, target string) (report Report, retErr error) {
	if err := os.Mkdir(target, 0o700); err != nil {
		return report, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
	root, err := os.OpenRoot(target)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	var after int64
	for {
		files, err := a.database.ListRawArchiveFiles(ctx, after, pageSize)
		if err != nil {
			return report, err
		}
		for _, file := range files {
			if !filepath.IsLocal(file.RootID) || filepath.Base(file.RootID) != file.RootID || !filepath.IsLocal(file.Path) {
				return report, errors.New("invalid native file path")
			}
			path := filepath.Join(file.RootID, filepath.FromSlash(file.Path))
			if err := root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return report, err
			}
			output, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return report, fmt.Errorf("extracting %s (conflicting versions cannot be extracted together): %w", path, err)
			}
			_, copyErr := a.objects.CopyObject(ctx, a.tenant, rawsync.ObjectRef{SHA256: file.SHA256, Length: file.Size}, output)
			if err := errors.Join(copyErr, output.Sync(), output.Close()); err != nil {
				return report, err
			}
			mtime := time.Unix(0, file.ModTimeNS)
			if err := root.Chtimes(path, mtime, mtime); err != nil {
				return report, err
			}
			report.Files++
			report.Bytes += file.Size
			after = file.ID
		}
		if len(files) < pageSize {
			return report, nil
		}
	}
}
