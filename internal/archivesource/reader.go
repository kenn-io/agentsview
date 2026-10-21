package archivesource

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank/docbank"
)

// OpenReader rechecks repository identity and reloads object placement for one
// copy window. The caller must close it before parsing the copied source.
func (i *Index) OpenReader(ctx context.Context, path string) (*docbank.BackupReader, error) {
	repository, err := docbank.OpenBackupRepository(path)
	if err != nil {
		return nil, err
	}
	if repository.ID() != i.Info.RepositoryID {
		return nil, fmt.Errorf("%w: configured repository has changed", db.ErrArchiveSourceConflict)
	}
	return repository.OpenSnapshot(ctx, i.Info.SnapshotID)
}
