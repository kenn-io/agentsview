//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestDuckPushReplicatesWorktreeMappings verifies that a push publishes a
// worktree mapping to the DuckDB mirror.
func TestDuckPushReplicatesWorktreeMappings(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)

	created, err := local.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err, "Push")

	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")
	conn, err := Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var project string
	var createdAt, updatedAt time.Time
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT project, created_at, updated_at
		FROM source_worktree_project_mappings
		WHERE source_archive_id = ? AND machine = ? AND path_prefix = ?`,
		archiveID, "workstation", "/work/repos/sample",
	).Scan(&project, &createdAt, &updatedAt), "read back mirrored mapping")
	assert.Equal(t, "sample", project)
	wantCreatedAt, err := time.Parse(time.RFC3339Nano, created.CreatedAt)
	require.NoError(t, err)
	wantUpdatedAt, err := time.Parse(time.RFC3339Nano, created.UpdatedAt)
	require.NoError(t, err)
	assert.True(t, wantCreatedAt.Equal(createdAt),
		"created_at must retain the archive value: want %s, got %s",
		wantCreatedAt, createdAt)
	assert.True(t, wantUpdatedAt.Equal(updatedAt),
		"updated_at must retain the archive value: want %s, got %s",
		wantUpdatedAt, updatedAt)
}

func TestDuckFilteredMappingPublicationOmitsOutOfScopeMetadata(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)

	inScope, err := local.CreateWorktreeProjectMapping(
		ctx, db.WorktreeProjectMapping{
			Machine:         "workstation",
			PathPrefix:      "/work/repos/alpha",
			Layout:          db.WorktreeMappingLayoutExplicit,
			Project:         "alpha",
			OriginalProject: "private-source",
			Enabled:         true,
		},
	)
	require.NoError(t, err, "create in-scope mapping")
	_, err = local.CreateWorktreeProjectMapping(
		ctx, db.WorktreeProjectMapping{
			Machine:    "secret-host",
			PathPrefix: "/private/repos/beta",
			Layout:     db.WorktreeMappingLayoutExplicit,
			Project:    "beta",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "create out-of-scope mapping")
	_, err = local.CreateWorktreeProjectMapping(
		ctx, db.WorktreeProjectMapping{
			Machine:    "dynamic-host",
			PathPrefix: "/private/dynamic",
			Layout:     db.WorktreeMappingLayoutRepoDotWorktrees,
			Enabled:    true,
		},
	)
	require.NoError(t, err, "create dynamic mapping")

	opts := SyncOptions{Projects: []string{"alpha"}}
	_, err = Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err, "initial filtered Push")

	conn, err := Open(path)
	require.NoError(t, err)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings`,
	).Scan(&count))
	require.Equal(t, 1, count, "only the in-scope explicit rule is published")
	var project, originalProject, machine, pathPrefix string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT project, original_project, machine, path_prefix
		FROM source_worktree_project_mappings`,
	).Scan(&project, &originalProject, &machine, &pathPrefix))
	assert.Equal(t, "alpha", project)
	assert.Empty(t, originalProject, "out-of-scope historical label is redacted")
	assert.Equal(t, "workstation", machine)
	assert.Equal(t, "/work/repos/alpha", pathPrefix)
	require.NoError(t, conn.Close())

	_, err = local.UpdateWorktreeProjectMapping(
		ctx, inScope.Machine, inScope.ID,
		db.WorktreeProjectMapping{
			PathPrefix:      inScope.PathPrefix,
			Layout:          db.WorktreeMappingLayoutExplicit,
			Project:         "beta",
			OriginalProject: inScope.OriginalProject,
			Enabled:         true,
		},
	)
	require.NoError(t, err, "move mapping out of scope")
	result, err := Push(ctx, path, local, "m", opts, false, nil)
	require.NoError(t, err, "incremental filtered Push")
	assert.False(t, result.Diagnostics.Full)

	conn, err = Open(path)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings`,
	).Scan(&count))
	assert.Zero(t, count, "a rule that leaves scope must be removed")
}

// TestDuckFullPublicationClearsOnlyOwnArchive verifies that a full mapping
// publication (forced here by zeroing the mirror's mapping-revision cursor)
// clears only this archive's stale rows in the mirror, leaving other
// archives' rows under the same natural key untouched.
func TestDuckFullPublicationClearsOnlyOwnArchive(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)

	_, err := local.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")
	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err, "initial Push")

	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")
	conn, err := Open(path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, created_at, updated_at)
		VALUES ('foreign-archive', 'workstation', '/work/stale',
			'explicit', 'other', '', TRUE, current_timestamp, current_timestamp)`)
	require.NoError(t, err, "seed foreign archive mapping")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, created_at, updated_at)
		VALUES (?, 'workstation', '/work/stale', 'explicit', 'stale',
			'', TRUE, current_timestamp, current_timestamp)`, archiveID)
	require.NoError(t, err, "seed own stale mapping")
	// Zero the mirror-resident cursor so the next incremental push runs a
	// full mapping publication against a mirror that already holds rows.
	_, err = conn.ExecContext(ctx,
		`UPDATE sync_metadata SET value = '0' WHERE key = ?`,
		mappingRevisionMetadataKey)
	require.NoError(t, err, "zero mapping revision cursor")
	require.NoError(t, conn.Close())

	result, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err, "Push")
	require.False(t, result.Diagnostics.Full,
		"push must stay incremental so the full publication path, not a "+
			"mirror rebuild, clears the stale rows")

	conn, err = Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE source_archive_id = 'foreign-archive'`,
	).Scan(&count), "count foreign archive rows")
	assert.Equal(t, 1, count, "foreign archive rows must survive")
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE source_archive_id = ? AND path_prefix = '/work/stale'`,
		archiveID,
	).Scan(&count), "count own stale rows")
	assert.Equal(t, 0, count,
		"own stale rows must be cleared by full publication")
}

// TestDuckMappingDeleteTombstones verifies that deleting a local mapping and
// pushing again removes the corresponding mirror row via the incremental
// delta path (LoadWorktreeMappingPublicationDelta plus the archive-scoped
// per-key DELETE), not full publication: the first push records a nonzero
// MappingRevision cursor in mirror metadata, so the second push takes the
// delta branch. A second mapping created between pushes proves the delta
// branch also upserts, and a sentinel mirror row owned by the same archive
// but absent from the local archive proves the delta path really ran: a
// full archive-scoped republication would clear the sentinel, while the
// per-key delta leaves it untouched.
func TestDuckMappingDeleteTombstones(t *testing.T) {
	ctx := context.Background()
	local, path := newPushFixture(t, 1)

	created, err := local.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")
	_, err = Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err, "first Push")

	probe, err := ProbeMirror(ctx, path)
	require.NoError(t, err, "ProbeMirror")
	require.Positive(t, probe.MappingRevision,
		"first push must record a mapping publication cursor so the "+
			"second push exercises the delta path")

	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")
	conn, err := Open(path)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, created_at, updated_at)
		VALUES (?, 'workstation', '/work/sentinel', 'explicit', 'sentinel',
			'', TRUE, current_timestamp, current_timestamp)`, archiveID)
	require.NoError(t, err, "seed same-archive sentinel row")
	require.NoError(t, conn.Close())

	require.NoError(t, local.DeleteWorktreeProjectMapping(
		ctx, "workstation", created.ID), "DeleteWorktreeProjectMapping")
	_, err = local.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/other",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "other",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping second mapping")
	result, err := Push(ctx, path, local, "m", SyncOptions{}, false, nil)
	require.NoError(t, err, "second Push")
	assert.False(t, result.Diagnostics.Full,
		"second push must be incremental to exercise the delta path")

	conn, err = Open(path)
	require.NoError(t, err)
	defer conn.Close()
	var deletedCount int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE path_prefix = '/work/repos/sample'`,
	).Scan(&deletedCount), "count deleted mapping")
	assert.Equal(t, 0, deletedCount,
		"deleted mapping must be removed by the tombstone delta")

	var newProject string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT project FROM source_worktree_project_mappings
		WHERE path_prefix = '/work/repos/other'`,
	).Scan(&newProject), "read back new mapping")
	assert.Equal(t, "other", newProject,
		"mapping created between pushes must be upserted by the delta")

	var sentinelCount int
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE source_archive_id = ? AND path_prefix = '/work/sentinel'`,
		archiveID,
	).Scan(&sentinelCount), "count sentinel row")
	assert.Equal(t, 1, sentinelCount,
		"same-archive sentinel must survive the per-key delta; a full "+
			"archive-scoped republication would have cleared it")
}
