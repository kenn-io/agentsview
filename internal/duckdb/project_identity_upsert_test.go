package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"go.kenn.io/agentsview/internal/duckdb/bundialect"
	"go.kenn.io/agentsview/internal/export"
)

type identityInsertQueryCounter struct {
	inserts int
}

func (*identityInsertQueryCounter) BeforeQuery(
	ctx context.Context, _ *bun.QueryEvent,
) context.Context {
	return ctx
}

func (h *identityInsertQueryCounter) AfterQuery(
	_ context.Context, event *bun.QueryEvent,
) {
	if event.Operation() == "INSERT" {
		h.inserts++
	}
}

func TestUpsertSessionProjectIdentitySnapshotsPersistsEveryBatch(t *testing.T) {
	ctx := t.Context()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database))
	hook := new(identityInsertQueryCounter)
	store := bun.NewDB(database, bundialect.New()).WithQueryHook(hook)
	snapshots := make([]export.ProjectIdentityObservation, 501)
	for i := range snapshots {
		snapshots[i] = export.ProjectIdentityObservation{
			SessionID: fmt.Sprintf("session-%03d", i),
			Project:   "app", Machine: "local",
		}
	}
	require.NoError(t, upsertSessionProjectIdentitySnapshots(
		ctx, store, "archive", "generation", snapshots,
	))
	assert.Equal(t, 2, hook.inserts,
		"identity publication must keep 500-row bounded batches")
	var count int
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_session_project_identity_snapshots
		WHERE source_archive_id = ? AND source_database_generation = ?`,
		"archive", "generation",
	).Scan(&count))
	assert.Equal(t, len(snapshots), count)

	snapshots[500].GitBranch = "main"
	require.NoError(t, upsertSessionProjectIdentitySnapshots(
		ctx, store, "archive", "generation", snapshots[500:],
	))
	var branch string
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT git_branch FROM source_session_project_identity_snapshots
		WHERE source_archive_id = ? AND source_database_generation = ?
		  AND source_session_id = ?`,
		"archive", "generation", "session-500",
	).Scan(&branch))
	assert.Equal(t, "main", branch)
}

func TestDuckUpsertUnknownDoesNotReplaceAmbiguousEvidence(t *testing.T) {
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database))
	exec := func(query string, args ...any) error {
		_, err := database.ExecContext(ctx, query, args...)
		return err
	}
	queryRow := func(query string, args ...any) *sql.Row {
		return database.QueryRowContext(ctx, query, args...)
	}
	store := bun.NewDB(database, bundialect.New())
	require.NoError(t, upsertSourceArchiveScope(
		ctx, store, "archive", "salt"))
	base := export.ProjectIdentityObservation{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
		Project: "app", Machine: "laptop", RootPath: "/repo/app",
		ObservedAt: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	}
	ambiguous := base
	ambiguous.RemoteResolution = export.ProjectResolutionAmbiguous
	ambiguous.RemoteCandidateCount = 2
	require.NoError(t, upsertProjectIdentityObservation(
		ctx, store, exec, queryRow, ambiguous, ""))
	unknown := base
	unknown.RemoteResolution = export.ProjectResolutionUnknown
	require.NoError(t, upsertProjectIdentityObservation(
		ctx, store, exec, queryRow, unknown, ""))

	var resolution string
	require.NoError(t, database.QueryRowContext(ctx, `
		SELECT remote_resolution
		FROM source_project_identity_observations
		WHERE source_archive_id = ? AND project = ? AND machine = ?
		  AND root_path = ? AND git_remote = ''`,
		"archive", "app", "laptop", "/repo/app",
	).Scan(&resolution))
	assert.Equal(t, string(export.ProjectResolutionAmbiguous), resolution)
}
