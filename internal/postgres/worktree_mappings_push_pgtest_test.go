//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushReplicatesWorktreeMappings verifies that a push publishes a
// worktree mapping to the PG mirror, even when there are no sessions to
// push (the mapping-only publication path).
func TestPushReplicatesWorktreeMappings(t *testing.T) {
	const schema = "agentsview_push_mapping_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	_, err := localDB.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	archiveID, err := localDB.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")
	var project string
	var enabled bool
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT project, enabled FROM source_worktree_project_mappings
		WHERE source_archive_id = $1 AND machine = $2 AND path_prefix = $3`,
		archiveID, "workstation", "/work/repos/sample",
	).Scan(&project, &enabled), "read back mirrored mapping")
	assert.Equal(t, "sample", project)
	assert.True(t, enabled)
}

func TestFilteredPushDoesNotPublishArchiveWideWorktreeMappings(t *testing.T) {
	const schema = "agentsview_push_mapping_filtered_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)
	sync.projects = []string{"included-project"}

	_, err := localDB.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/private/unrelated",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "unrelated-project",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "filtered Push")

	var count int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_worktree_project_mappings`,
	).Scan(&count), "count mirrored mappings")
	assert.Zero(t, count)
}

// TestPushMappingDeleteTombstones verifies that deleting a local mapping and
// pushing again removes the corresponding mirror row.
func TestPushMappingDeleteTombstones(t *testing.T) {
	const schema = "agentsview_push_mapping_delete_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	created, err := localDB.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	require.NoError(t, localDB.DeleteWorktreeProjectMapping(
		ctx, "workstation", created.ID), "DeleteWorktreeProjectMapping")
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")

	var count int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM source_worktree_project_mappings`,
	).Scan(&count), "count mirrored mappings")
	assert.Equal(t, 0, count)
}

// TestMappingReplicationIsArchiveScoped verifies that a full mapping
// publication (this archive's first push) does not touch rows published by
// another archive under the same (machine, path_prefix) natural key.
func TestMappingReplicationIsArchiveScoped(t *testing.T) {
	const schema = "agentsview_push_mapping_scope_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	// A foreign archive already published an identical (machine,
	// path_prefix) rule.
	_, err := pg.ExecContext(ctx, `
		INSERT INTO source_worktree_project_mappings
		(source_archive_id, machine, path_prefix, layout, project,
		 original_project, enabled, updated_at)
		VALUES ('foreign-archive', 'workstation', '/work/repos/sample',
		 'explicit', 'other', '', TRUE, '')`)
	require.NoError(t, err, "seed foreign archive mapping")

	_, err = localDB.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")
	// First push for this archive is always a full mapping publication
	// (empty cursor).
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")

	var projects []string
	rows, err := pg.QueryContext(ctx, `
		SELECT project FROM source_worktree_project_mappings
		WHERE machine = 'workstation' AND path_prefix = '/work/repos/sample'
		ORDER BY source_archive_id`)
	require.NoError(t, err, "query mirrored mappings")
	defer rows.Close()
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p), "scan mirrored mapping project")
		projects = append(projects, p)
	}
	require.NoError(t, rows.Err(), "iterate mirrored mappings")
	assert.Len(t, projects, 2,
		"both archives' rules coexist; no cross-archive overwrite")
	assert.Contains(t, projects, "other")
	assert.Contains(t, projects, "sample")
}

// TestMappingCursorNotAdvancedOnFailure verifies that a mapping publication
// failure inside syncWorktreeMappings leaves the mapping publication cursor
// unset, so the next push retries a full publication instead of silently
// skipping the mapping that failed to mirror.
func TestMappingCursorNotAdvancedOnFailure(t *testing.T) {
	const schema = "agentsview_push_mapping_fail_test"
	sync, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	_, err := localDB.CreateWorktreeProjectMapping(ctx,
		db.WorktreeProjectMapping{
			Machine: "workstation", PathPrefix: "/work/repos/sample",
			Layout: db.WorktreeMappingLayoutExplicit, Project: "sample",
			Enabled: true,
		})
	require.NoError(t, err, "CreateWorktreeProjectMapping")

	// Sabotage the mirror table so the mapping publication insert fails
	// after the other push finalization steps have already succeeded.
	_, err = pg.Exec(
		`ALTER TABLE source_worktree_project_mappings DROP COLUMN project`)
	require.NoError(t, err, "drop project column")

	_, err = sync.Push(ctx, false, nil)
	require.Error(t, err, "push should fail at mapping publication")

	databaseGeneration, err := localDB.GetDatabaseID(ctx)
	require.NoError(t, err, "GetDatabaseID")
	cursor, err := localDB.GetSyncState(
		worktreeMappingPublicationStateKey + ":" + databaseGeneration)
	require.NoError(t, err, "GetSyncState")
	assert.Empty(t, cursor,
		"failed push must not advance the mapping publication cursor")
}
