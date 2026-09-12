package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/export"
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
			return InsertWorktreeProjectMappingRows(ctx, tx, rows)
		}))
	require.Error(t, InsertWorktreeProjectMappingRows(
		ctx, database.bunWriter, rows,
	))
	rows[0].PathPrefix = "/repo/.branches"
	changed, err := UpdateWorktreeProjectMappingRow(
		ctx, database.bunWriter, archiveID, 17, "machine", rows[0],
	)
	require.NoError(t, err)
	assert.True(t, changed)
	var afterUpdate []bunmodel.SourceWorktreeProjectMapping
	require.NoError(t, database.bunReader.NewSelect().Model(&afterUpdate).
		Where("source_archive_id = ?", archiveID).Scan(ctx))
	require.Len(t, afterUpdate, 1)
	assert.Equal(t, "/repo/.branches", afterUpdate[0].PathPrefix)

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
		Where("path_prefix = ?", "/repo/.branches").Scan(ctx))
	assert.Equal(t, int64(23), got.ID)
	assert.Equal(t, "beta", got.Project)
	assert.Equal(t, "source-beta", got.OriginalProject)
	assert.False(t, got.Enabled)
	assert.Equal(t, rows[0].CreatedAt, got.CreatedAt)

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			return DeleteWorktreeProjectMappingRows(ctx, tx, archiveID,
				[]WorktreeMappingKey{{Machine: "machine", PathPrefix: "/repo/.branches"}})
		}))
	count, err := database.bunReader.NewSelect().
		Model((*bunmodel.SourceWorktreeProjectMapping)(nil)).
		Where("source_archive_id = ?", archiveID).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestCanonicalProjectIdentityRowsReplacePortableState(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	observedAt := time.Date(2026, 8, 4, 12, 0, 0, 123456789, time.UTC)
	observation := export.ProjectIdentityObservation{
		SessionID: "session-1", Project: "project", Machine: "machine",
		RootPath:      "/repo/project",
		GitRemote:     "https://user:secret@example.com/acme/project.git?token=secret",
		GitRemoteName: "origin", RepositoryPath: "project",
		WorktreeName: "feature", WorktreeRootPath: "/repo/project",
		WorktreeRelationship: export.WorktreeLinked,
		CheckoutState:        export.CheckoutBranch, GitBranch: "feature",
		RemoteResolution:     export.ProjectResolutionResolved,
		RemoteCandidateCount: 1, ObservedAt: observedAt,
	}
	observationRows, err := CanonicalProjectIdentityObservationRows(
		"archive", "salt", []export.ProjectIdentityObservation{observation},
	)
	require.NoError(t, err)
	snapshotRows, err := CanonicalSessionProjectIdentitySnapshotRows(
		"archive", "generation", []export.ProjectIdentityObservation{observation},
	)
	require.NoError(t, err)
	require.Len(t, observationRows, 1)
	require.Len(t, snapshotRows, 1)
	assert.Equal(t, "https://example.com/acme/project.git", observationRows[0].GitRemote)
	assert.Equal(t, observedAt.Truncate(time.Microsecond),
		observationRows[0].ObservedAt.Time)
	assert.Equal(t, "session-1", snapshotRows[0].SourceSessionID)

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			if err := UpsertSourceArchiveRow(ctx, tx, "archive", "salt"); err != nil {
				return err
			}
			if err := UpsertProjectIdentityObservationRows(
				ctx, tx, observationRows,
			); err != nil {
				return err
			}
			return UpsertSessionProjectIdentitySnapshotRows(ctx, tx, snapshotRows)
		}))

	observationRows[0].GitBranch = "main"
	snapshotRows[0].GitBranch = "main"
	require.NoError(t, UpsertProjectIdentityObservationRows(
		ctx, database.bunWriter, observationRows,
	))
	require.NoError(t, UpsertSessionProjectIdentitySnapshotRows(
		ctx, database.bunWriter, snapshotRows,
	))
	var gotObservation bunmodel.SourceProjectIdentityObservation
	require.NoError(t, database.bunReader.NewSelect().Model(&gotObservation).
		Where("source_archive_id = ?", "archive").Scan(ctx))
	var gotSnapshot bunmodel.SourceSessionProjectIdentitySnapshot
	require.NoError(t, database.bunReader.NewSelect().Model(&gotSnapshot).
		Where("source_archive_id = ?", "archive").Scan(ctx))
	assert.Equal(t, "main", gotObservation.GitBranch)
	assert.Equal(t, "main", gotSnapshot.GitBranch)
	require.NoError(t, UpsertSourceArchiveRow(
		ctx, database.bunWriter, "archive", "salt",
	))
	require.ErrorContains(t, UpsertSourceArchiveRow(
		ctx, database.bunWriter, "archive", "different-salt",
	), "archive salt mismatch")
}

func TestCanonicalIdentityBatchWritesPreserveLaterNonDefaultFields(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	observedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	observations := []export.ProjectIdentityObservation{
		{
			SessionID: "minimal-session", Project: "minimal", Machine: "machine",
			RootPath: "/repo/minimal", ObservedAt: observedAt,
		},
		{
			SessionID: "rich-session", Project: "rich", Machine: "machine",
			RootPath: "/repo/rich", GitRemote: "https://example.com/acme/rich.git",
			GitRemoteName: "origin", RepositoryPath: "rich",
			WorktreeName: "feature", WorktreeRootPath: "/repo/rich",
			WorktreeRelationship: export.WorktreeLinked,
			CheckoutState:        export.CheckoutBranch, GitBranch: "feature",
			RemoteResolution:     export.ProjectResolutionResolved,
			RemoteCandidateCount: 2, ObservedAt: observedAt,
		},
	}
	observationRows, err := CanonicalProjectIdentityObservationRows(
		"archive", "salt", observations,
	)
	require.NoError(t, err)
	snapshotRows, err := CanonicalSessionProjectIdentitySnapshotRows(
		"archive", "generation", observations,
	)
	require.NoError(t, err)

	require.NoError(t, database.bunWriter.RunInTx(ctx, nil,
		func(ctx context.Context, tx bun.Tx) error {
			if err := UpsertSourceArchiveRow(ctx, tx, "archive", "salt"); err != nil {
				return err
			}
			if err := UpsertProjectIdentityObservationRows(ctx, tx, observationRows); err != nil {
				return err
			}
			return UpsertSessionProjectIdentitySnapshotRows(ctx, tx, snapshotRows)
		}))

	var gotObservation bunmodel.SourceProjectIdentityObservation
	require.NoError(t, database.bunReader.NewSelect().Model(&gotObservation).
		Where("source_archive_id = ?", "archive").
		Where("project = ?", "rich").Scan(ctx))
	assert.Equal(t, "feature", gotObservation.GitBranch)
	assert.Equal(t, 2, gotObservation.RemoteCandidateCount)
	assert.Equal(t, "origin", gotObservation.GitRemoteName)

	var gotSnapshot bunmodel.SourceSessionProjectIdentitySnapshot
	require.NoError(t, database.bunReader.NewSelect().Model(&gotSnapshot).
		Where("source_archive_id = ?", "archive").
		Where("source_session_id = ?", "rich-session").Scan(ctx))
	assert.Equal(t, "feature", gotSnapshot.GitBranch)
	assert.Equal(t, 2, gotSnapshot.RemoteCandidateCount)
	assert.Equal(t, "origin", gotSnapshot.GitRemoteName)
}
