package duckdb

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestUpsertSessionProjectIdentitySnapshotsBatchesStatements(t *testing.T) {
	snapshots := make([]export.ProjectIdentityObservation, 501)
	for i := range snapshots {
		snapshots[i] = export.ProjectIdentityObservation{
			SessionID: "session", Project: "app", Machine: "local",
		}
	}
	var argCounts []int
	err := upsertSessionProjectIdentitySnapshots(
		func(_ string, args ...any) error {
			argCounts = append(argCounts, len(args))
			return nil
		},
		"archive", "generation", snapshots,
	)
	require.NoError(t, err)
	assert.Equal(t, []int{500 * 20, 20}, argCounts)
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
	require.NoError(t, upsertSourceArchiveScope(
		exec, queryRow, "archive", "salt"))
	base := export.ProjectIdentityObservation{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
		Project: "app", Machine: "laptop", RootPath: "/repo/app",
		ObservedAt: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	}
	ambiguous := base
	ambiguous.RemoteResolution = export.ProjectResolutionAmbiguous
	ambiguous.RemoteCandidateCount = 2
	require.NoError(t, upsertProjectIdentityObservation(
		exec, queryRow, ambiguous, ""))
	unknown := base
	unknown.RemoteResolution = export.ProjectResolutionUnknown
	require.NoError(t, upsertProjectIdentityObservation(
		exec, queryRow, unknown, ""))

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

// TestDuckUpsertObservationArgsRenderAsRemoteLiterals drives every
// upsertProjectIdentityObservation branch through the same literal rendering
// the Quack remote transport uses, so an argument type the renderer rejects
// (such as a named string type) fails the test instead of aborting real
// remote pushes.
func TestDuckUpsertObservationArgsRenderAsRemoteLiterals(t *testing.T) {
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database))
	exec := func(query string, args ...any) error {
		rendered, err := duckSQLWithArgs(query, args...)
		if err != nil {
			return err
		}
		_, err = database.ExecContext(ctx, rendered)
		return err
	}
	queryRow := func(query string, args ...any) *sql.Row {
		rendered, err := duckSQLWithArgs(query, args...)
		require.NoError(t, err, "rendering remote read literals")
		return database.QueryRowContext(ctx, rendered)
	}
	require.NoError(t, upsertSourceArchiveScope(
		exec, queryRow, "archive", "salt"))

	withRemote := export.ProjectIdentityObservation{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
		Project: "app", Machine: "laptop", RootPath: "/repo/app",
		GitRemote:            "git@example.com:app.git",
		WorktreeRelationship: export.WorktreeMain,
		CheckoutState:        export.CheckoutBranch,
		RemoteResolution:     export.ProjectResolutionUnknown,
		ObservedAt:           time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t,
		upsertProjectIdentityObservation(exec, queryRow, withRemote, ""),
		"stale-fallback delete branch must render over the remote transport")

	rootFallback := withRemote
	rootFallback.GitRemote = ""
	require.NoError(t,
		upsertProjectIdentityObservation(exec, queryRow, rootFallback, ""),
		"root-fallback query branch must render over the remote transport")

	snapshot := withRemote
	snapshot.SessionID = "session"
	require.NoError(t, upsertSessionProjectIdentitySnapshots(
		exec, "archive", "generation",
		[]export.ProjectIdentityObservation{snapshot},
	), "snapshot batch must render over the remote transport")
}
