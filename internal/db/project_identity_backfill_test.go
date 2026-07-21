package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestEnsureProjectIdentityBackfillRequeuesUnverifiedCompletedGap(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{
		ID: "missing-snapshot", Project: "app", Machine: "local",
		Agent: "codex",
	}))
	_, err := d.getWriter().ExecContext(ctx,
		`DELETE FROM session_project_identity_snapshots WHERE session_id = ?`,
		"missing-snapshot")
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `
		INSERT INTO background_migrations (
			name, state, total_items, completed_items, completed_at
		) VALUES (?, 'completed', 1, 1, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		ON CONFLICT(name) DO UPDATE SET
			state = excluded.state,
			total_items = excluded.total_items,
			completed_items = excluded.completed_items,
			completed_at = excluded.completed_at`,
		ProjectIdentityBackfillName)
	require.NoError(t, err)

	require.NoError(t, d.EnsureProjectIdentityBackfillQueued(ctx))
	status, err := d.ProjectIdentityBackfillStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pending", status.State)
	assert.Equal(t, 1, status.TotalItems)
}

func TestEnsureProjectIdentityBackfillVerifiesCompleteSnapshotSetOnce(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	_, err := d.getWriter().ExecContext(ctx, `
		INSERT INTO background_migrations (
			name, state, total_items, completed_items, completed_at
		) VALUES (?, 'completed', 0, 0, strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		ProjectIdentityBackfillName)
	require.NoError(t, err)

	require.NoError(t, d.EnsureProjectIdentityBackfillQueued(ctx))
	var verified string
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		`SELECT value FROM stats WHERE key = ?`,
		projectIdentityBackfillVerifiedKey,
	).Scan(&verified))
	assert.Equal(t, "1", verified)
}

func TestSessionInsertCreatesUnknownProjectIdentitySnapshot(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{
		ID: "atomic-snapshot", Project: "app", Machine: "local",
		Agent: "codex", Cwd: "/tmp/app", GitBranch: "feature/test",
	}))

	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "atomic-snapshot", snapshots[0].SessionID)
	assert.Equal(t, export.ProjectResolutionUnknown, snapshots[0].RemoteResolution)
	assert.Equal(t, export.CheckoutBranch, snapshots[0].CheckoutState)
	assert.Equal(t, "feature/test", snapshots[0].GitBranch)

	require.NoError(t, d.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "atomic-snapshot", Project: "app", Machine: "local",
			RootPath: "/tmp/app", ObservedAt: time.Now().UTC(),
		}))
	snapshots, err = d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, snapshots[0].KeySource)
	assert.NotEmpty(t, snapshots[0].Key)
}

func TestResyncOrphanCopyLeavesLegacySnapshotGapEligibleForBackfill(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()
	sourcePath := filepath.Join(dir, "source.db")

	source, err := Open(sourcePath)
	require.NoError(t, err)
	for _, session := range []Session{
		{ID: "live", Project: "app", Machine: "local", Agent: "codex"},
		{ID: "legacy-orphan", Project: "app", Machine: "local", Agent: "codex"},
		{ID: "known-orphan", Project: "app", Machine: "local", Agent: "codex"},
	} {
		require.NoError(t, source.UpsertSession(session))
	}
	require.NoError(t, source.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID: "known-orphan", Project: "app", Machine: "local",
			RootPath:   "/historical/app",
			GitRemote:  "https://example.com/example/app.git",
			ObservedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		}))
	_, err = source.getWriter().ExecContext(ctx, `
		DELETE FROM session_project_identity_snapshots
		WHERE session_id IN ('live', 'legacy-orphan')`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	destination := testDB(t)
	require.NoError(t, destination.UpsertSession(Session{
		ID: "live", Project: "app", Machine: "local", Agent: "codex",
		Cwd: "/fresh/app",
	}))

	copied, err := destination.CopyOrphanedDataFrom(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, 2, copied)
	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	snapshots, err := destination.listSessionProjectIdentitySnapshots(
		ctx, []string{"known-orphan", "legacy-orphan", "live"},
	)
	require.NoError(t, err)
	require.Contains(t, snapshots, "known-orphan")
	assert.Equal(t, "https://example.com/example/app.git",
		snapshots["known-orphan"].GitRemote)
	assert.NotContains(t, snapshots, "legacy-orphan",
		"a legacy orphan without source evidence must remain backfill-eligible")
	require.Contains(t, snapshots, "live")
	assert.Equal(t, "/fresh/app", snapshots["live"].RootPath,
		"orphan cleanup must not touch sessions outside the copied batch")

	status, err := destination.ProjectIdentityBackfillStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pending", status.State)
	assert.Equal(t, 1, status.TotalItems)
}

func TestResyncTrashedCopyLeavesLegacySnapshotGapEligibleForBackfill(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()
	sourcePath := filepath.Join(dir, "source.db")

	source, err := Open(sourcePath)
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(Session{
		ID: "legacy-trashed", Project: "app", Machine: "local", Agent: "codex",
	}))
	_, err = source.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = 'legacy-trashed';
		DELETE FROM session_project_identity_snapshots
		WHERE session_id = 'legacy-trashed'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	destination := testDB(t)
	copied, err := destination.CopyTrashedDataFrom(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, 1, copied)
	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	snapshots, err := destination.listSessionProjectIdentitySnapshots(
		ctx, []string{"legacy-trashed"},
	)
	require.NoError(t, err)
	assert.NotContains(t, snapshots, "legacy-trashed")
	status, err := destination.ProjectIdentityBackfillStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pending", status.State)
	assert.Equal(t, 1, status.TotalItems)
}

func TestProjectIdentityBackfillBatchUsesKeysetAndAdvancesAtomically(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	for _, id := range []string{"batch-a", "batch-b", "batch-c"} {
		require.NoError(t, d.UpsertSession(Session{
			ID: id, Project: "app", Machine: "local", Agent: "codex",
		}))
	}
	_, err := d.getWriter().ExecContext(ctx,
		`DELETE FROM session_project_identity_snapshots`)
	require.NoError(t, err)
	require.NoError(t, d.EnsureProjectIdentityBackfillQueued(ctx))
	require.NoError(t, d.StartProjectIdentityBackfill(ctx))

	candidates, err := d.ProjectIdentityBackfillCandidatesAfter(ctx, "batch-a")
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.Equal(t, "batch-b", candidates[0].ID)
	assert.Equal(t, "batch-c", candidates[1].ID)

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	batch := []export.ProjectIdentityObservation{
		{
			SessionID: "batch-b", Project: "app", Machine: "local",
			RootPath: "/tmp/app", ObservedAt: now,
		},
		{
			SessionID: "batch-c", Project: "app", Machine: "local",
			RootPath: "/tmp/app", ObservedAt: now,
		},
	}
	require.NoError(t, d.ApplyProjectIdentityBackfillBatch(ctx, batch))

	status, err := d.ProjectIdentityBackfillStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, status.CompletedItems)
	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	assert.Equal(t, "batch-b", snapshots[0].SessionID)
	assert.Equal(t, "batch-c", snapshots[1].SessionID)
}

func TestProjectIdentityBackfillPersistsUnknownSnapshotForEmptyProject(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{
		ID: "unresolved", Machine: "local", Agent: "antigravity-cli",
	}))
	_, err := d.getWriter().ExecContext(ctx,
		`DELETE FROM session_project_identity_snapshots WHERE session_id = ?`,
		"unresolved")
	require.NoError(t, err)
	require.NoError(t, d.EnsureProjectIdentityBackfillQueued(ctx))
	require.NoError(t, d.StartProjectIdentityBackfill(ctx))

	require.NoError(t, d.ApplyProjectIdentityBackfillBatch(ctx,
		[]export.ProjectIdentityObservation{{
			SessionID: "unresolved", Machine: "local",
			ObservedAt: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		}}))
	require.NoError(t, d.CompleteProjectIdentityBackfill(ctx))

	snapshots, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "unresolved", snapshots[0].SessionID)
	assert.Empty(t, snapshots[0].Project)
	assert.Equal(t, export.ProjectResolutionUnknown,
		snapshots[0].RemoteResolution)
	status, err := d.ProjectIdentityBackfillStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, "completed", status.State)
}

func TestCopySessionMetadataMigratesLegacyTraeProjectIdentitySnapshot(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()
	sourcePath := filepath.Join(dir, "source.db")

	source, err := Open(sourcePath)
	require.NoError(t, err)
	legacyPath := filepath.Join(
		dir, "workspaceStorage", "legacy-trae", "transcript.jsonl",
	)
	require.NoError(t, source.UpsertSession(Session{
		ID:               "trae:legacy-trae",
		Project:          "app",
		Machine:          "local",
		Agent:            "trae",
		SourceSessionID:  "legacy-trae",
		FilePath:         &legacyPath,
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-07-21T00:00:00Z",
	}))
	require.NoError(t, source.UpsertProjectIdentityObservation(ctx,
		export.ProjectIdentityObservation{
			SessionID:     "trae:legacy-trae",
			Project:       "app",
			Machine:       "local",
			RootPath:      "/historical/app",
			GitRemote:     "https://github.com/example/app.git",
			ObservedAt:    time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
			GitBranch:     "feature/migrated",
			CheckoutState: export.CheckoutBranch,
		}))
	require.NoError(t, source.Close())

	destination := testDB(t)
	require.NoError(t, destination.UpsertSession(Session{
		ID:               "trae:workspaceStorage:legacy-trae",
		Project:          "app",
		Machine:          "local",
		Agent:            "trae",
		SourceSessionID:  "legacy-trae",
		FilePath:         &legacyPath,
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-07-21T00:01:00Z",
	}))

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))

	snapshots, err := destination.listSessionProjectIdentitySnapshots(
		ctx, []string{
			"trae:legacy-trae",
			"trae:workspaceStorage:legacy-trae",
		},
	)
	require.NoError(t, err)
	assert.NotContains(t, snapshots, "trae:legacy-trae")
	require.Contains(t, snapshots, "trae:workspaceStorage:legacy-trae")
	assert.Equal(t,
		"/historical/app",
		snapshots["trae:workspaceStorage:legacy-trae"].RootPath,
	)
	assert.Equal(t,
		"feature/migrated",
		snapshots["trae:workspaceStorage:legacy-trae"].GitBranch,
	)
}
