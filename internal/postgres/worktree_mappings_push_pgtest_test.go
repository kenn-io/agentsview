//go:build pgtest

package postgres

import (
	"testing"
	"time"

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

	created, err := localDB.CreateWorktreeProjectMapping(ctx,
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
	var id int64
	var project string
	var enabled bool
	var createdAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT id, project, enabled, created_at
		FROM source_worktree_project_mappings
		WHERE source_archive_id = $1 AND machine = $2 AND path_prefix = $3`,
		archiveID, "workstation", "/work/repos/sample",
	).Scan(&id, &project, &enabled, &createdAt), "read back mirrored mapping")
	wantCreatedAt, err := time.Parse(time.RFC3339Nano, created.CreatedAt)
	require.NoError(t, err)
	assert.Equal(t, created.ID, id)
	assert.Equal(t, "sample", project)
	assert.True(t, enabled)
	assert.Equal(t, wantCreatedAt.Truncate(time.Microsecond), createdAt.UTC())
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

func TestFilteredMappingPublicationOmitsOutOfScopeMetadata(t *testing.T) {
	const schema = "agentsview_push_mapping_filter_test"
	_, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	inScope, err := localDB.CreateWorktreeProjectMapping(
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
	_, err = localDB.CreateWorktreeProjectMapping(
		ctx, db.WorktreeProjectMapping{
			Machine:    "secret-host",
			PathPrefix: "/private/repos/beta",
			Layout:     db.WorktreeMappingLayoutExplicit,
			Project:    "beta",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "create out-of-scope mapping")
	_, err = localDB.CreateWorktreeProjectMapping(
		ctx, db.WorktreeProjectMapping{
			Machine:    "dynamic-host",
			PathPrefix: "/private/dynamic",
			Layout:     db.WorktreeMappingLayoutRepoDotWorktrees,
			Enabled:    true,
		},
	)
	require.NoError(t, err, "create dynamic mapping")

	archiveID, err := localDB.GetArchiveID(ctx)
	require.NoError(t, err, "GetArchiveID")
	filtered := &Sync{
		pg: pg, local: localDB, archiveID: archiveID,
		projects: []string{"alpha"},
	}
	require.NoError(t, filtered.syncWorktreeMappings(ctx, false))

	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE source_archive_id = $1`, archiveID,
	).Scan(&count))
	require.Equal(t, 1, count, "only the in-scope explicit rule is published")
	var project, originalProject, machine, pathPrefix string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT project, original_project, machine, path_prefix
		FROM source_worktree_project_mappings
		WHERE source_archive_id = $1`, archiveID,
	).Scan(&project, &originalProject, &machine, &pathPrefix))
	assert.Equal(t, "alpha", project)
	assert.Empty(t, originalProject, "out-of-scope historical label is redacted")
	assert.Equal(t, "workstation", machine)
	assert.Equal(t, "/work/repos/alpha", pathPrefix)

	_, err = localDB.UpdateWorktreeProjectMapping(
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
	require.NoError(t, filtered.syncWorktreeMappings(ctx, false))

	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM source_worktree_project_mappings
		WHERE source_archive_id = $1`, archiveID,
	).Scan(&count))
	assert.Zero(t, count, "a rule that leaves scope must be removed")
}

func TestFilteredMappingPublicationPreservesUnfilteredMappings(t *testing.T) {
	const schema = "agentsview_push_mapping_unfiltered_owner_test"
	unfiltered, localDB, pg, ctx := newSessionProvenancePushSync(t, schema)

	for _, project := range []string{"alpha", "beta"} {
		originalProject := ""
		if project == "alpha" {
			originalProject = "private-source"
		}
		_, err := localDB.CreateWorktreeProjectMapping(
			ctx, db.WorktreeProjectMapping{
				Machine:         "workstation",
				PathPrefix:      "/work/repos/" + project,
				Layout:          db.WorktreeMappingLayoutExplicit,
				Project:         project,
				OriginalProject: originalProject,
				Enabled:         true,
			},
		)
		require.NoError(t, err)
	}
	_, err := unfiltered.Push(ctx, false, nil)
	require.NoError(t, err)

	archiveID, err := localDB.GetArchiveID(ctx)
	require.NoError(t, err)
	filtered := &Sync{
		pg: pg, local: localDB, archiveID: archiveID,
		projects: []string{"alpha"},
	}
	require.NoError(t, filtered.syncWorktreeMappings(ctx, false))

	rows, err := pg.QueryContext(ctx, `
		SELECT project
		FROM source_worktree_project_mappings
		WHERE source_archive_id = $1
		ORDER BY project`, archiveID)
	require.NoError(t, err)
	defer rows.Close()
	var projects []string
	for rows.Next() {
		var project string
		require.NoError(t, rows.Scan(&project))
		projects = append(projects, project)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"alpha", "beta"}, projects)

	var originalProject string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT original_project
		FROM source_worktree_project_mappings
		WHERE source_archive_id = $1 AND project = 'alpha'`, archiveID,
	).Scan(&originalProject))
	assert.Equal(t, "private-source", originalProject,
		"filtered redaction must not overwrite broader-scope metadata")
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
