package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestCanonicalWorktreeMappingRowsReplaceCompletePortableState(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	archiveID, err := database.GetArchiveID(ctx)
	require.NoError(t, err)
	const (
		createdAt = "2026-08-04T10:00:00.123456789Z"
		updatedAt = "2026-08-04T11:00:00.987654321Z"
	)
	rows, err := CanonicalWorktreeProjectMappingRows(archiveID, []WorktreeProjectMapping{{
		ID: 17, Machine: "machine", PathPrefix: "/repo/.worktrees",
		Layout: WorktreeMappingLayoutExplicit, Project: "alpha",
		OriginalProject: "source-alpha", Enabled: true,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, time.Date(2026, 8, 4, 10, 0, 0, 123456000, time.UTC),
		rows[0].CreatedAt.Time)
	assert.Equal(t, time.Date(2026, 8, 4, 11, 0, 0, 987654000, time.UTC),
		rows[0].UpdatedAt.Time)

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return UpsertWorktreeProjectMappingRows(ctx, tx, rows, nil)
		}))

	rows[0].ID = 23
	rows[0].Project = "beta"
	rows[0].OriginalProject = "source-beta"
	rows[0].Enabled = false
	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return UpsertWorktreeProjectMappingRows(ctx, tx, rows, nil)
		}))
	var got bunmodel.SourceWorktreeProjectMapping
	require.NoError(t, database.bunReader.NewSelect().Model(&got).
		Where("source_archive_id = ?", archiveID).
		Where("machine = ?", "machine").
		Where("path_prefix = ?", "/repo/.worktrees").Scan(ctx))
	assert.Equal(t, int64(23), got.ID)
	assert.Equal(t, "beta", got.Project)
	assert.Equal(t, "source-beta", got.OriginalProject)
	assert.False(t, got.Enabled)
	assert.Equal(t, rows[0].CreatedAt, got.CreatedAt)

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return DeleteWorktreeProjectMappingRows(ctx, tx, archiveID,
				[]WorktreeMappingKey{{Machine: "machine", PathPrefix: "/repo/.worktrees"}})
		}))
	count, err := database.bunReader.NewSelect().
		Model((*bunmodel.SourceWorktreeProjectMapping)(nil)).
		Where("source_archive_id = ?", archiveID).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}
