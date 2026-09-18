package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mappingChangeRow(
	t *testing.T, db *DB, machine, prefix string,
) (revision int64, deleted int, found bool) {
	t.Helper()
	err := db.getReader().QueryRowContext(t.Context(),
		`SELECT revision, deleted FROM worktree_project_mapping_changes
		 WHERE machine = ? AND path_prefix = ?`, machine, prefix,
	).Scan(&revision, &deleted)
	if err != nil {
		return 0, 0, false
	}
	return revision, deleted, true
}

func TestWorktreeMappingPublicationRevisionLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := testDB(t)
	ctx := t.Context()

	rev0, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)
	assert.Equal(int64(0), rev0)

	created, err := db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/repos/sample",
		Layout: WorktreeMappingLayoutExplicit, Project: "sample",
	})
	require.NoError(err)

	rev1, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)
	assert.Greater(rev1, rev0)
	jrev, jdel, ok := mappingChangeRow(t, db, "workstation", "/work/repos/sample")
	require.True(ok)
	assert.Equal(rev1, jrev)
	assert.Equal(0, jdel)

	_, err = db.UpdateWorktreeProjectMapping(ctx, "workstation", created.ID,
		WorktreeProjectMapping{
			PathPrefix: "/work/repos/sample",
			Layout:     WorktreeMappingLayoutExplicit,
			Project:    "sample",
			Enabled:    false,
		})
	require.NoError(err)
	rev2, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)
	assert.Greater(rev2, rev1)

	require.NoError(db.DeleteWorktreeProjectMapping(ctx, "workstation", created.ID))
	rev3, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)
	assert.Greater(rev3, rev2)
	jrev, jdel, ok = mappingChangeRow(t, db, "workstation", "/work/repos/sample")
	require.True(ok)
	assert.Equal(rev3, jrev)
	assert.Equal(1, jdel)
}

func TestWorktreeMappingChangeJournalPrefixRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := testDB(t)
	ctx := t.Context()

	created, err := db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/old",
		Layout: WorktreeMappingLayoutExplicit, Project: "sample",
	})
	require.NoError(err)

	_, err = db.UpdateWorktreeProjectMapping(ctx, "workstation", created.ID,
		WorktreeProjectMapping{
			PathPrefix: "/work/new",
			Layout:     WorktreeMappingLayoutExplicit,
			Project:    "sample",
		})
	require.NoError(err)

	_, oldDel, ok := mappingChangeRow(t, db, "workstation", "/work/old")
	require.True(ok, "old key must be journaled")
	assert.Equal(1, oldDel, "old key must be a tombstone")
	_, newDel, ok := mappingChangeRow(t, db, "workstation", "/work/new")
	require.True(ok, "new key must be journaled")
	assert.Equal(0, newDel)
}

func TestWorktreeMappingChangeJournalSelfCompacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := testDB(t)
	ctx := t.Context()

	created, err := db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/repos/sample",
		Layout: WorktreeMappingLayoutExplicit, Project: "sample",
	})
	require.NoError(err)
	require.NoError(db.DeleteWorktreeProjectMapping(ctx, "workstation", created.ID))
	_, err = db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/repos/sample",
		Layout: WorktreeMappingLayoutExplicit, Project: "sample",
	})
	require.NoError(err)

	_, del, ok := mappingChangeRow(t, db, "workstation", "/work/repos/sample")
	require.True(ok)
	assert.Equal(0, del,
		"recreate after delete must leave a current row, not a tombstone")

	var count int
	require.NoError(db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM worktree_project_mapping_changes
		 WHERE machine = ? AND path_prefix = ?`,
		"workstation", "/work/repos/sample").Scan(&count))
	assert.Equal(1, count, "journal keeps one latest row per key")
}

func TestLoadWorktreeMappingPublicationDelta(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := testDB(t)
	ctx := t.Context()

	first, err := db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/a",
		Layout: WorktreeMappingLayoutExplicit, Project: "alpha",
	})
	require.NoError(err)
	revAfterFirst, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)

	_, err = db.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "workstation", PathPrefix: "/work/b",
		Layout: WorktreeMappingLayoutExplicit, Project: "beta",
	})
	require.NoError(err)
	require.NoError(db.DeleteWorktreeProjectMapping(ctx, "workstation", first.ID))
	through, err := db.WorktreeMappingPublicationRevision(ctx)
	require.NoError(err)

	delta, err := db.LoadWorktreeMappingPublicationDelta(
		ctx, revAfterFirst, through)
	require.NoError(err)
	require.Len(delta.Mappings, 1)
	assert.Equal("/work/b", delta.Mappings[0].PathPrefix)
	require.Len(delta.Deletes, 1)
	assert.Equal(WorktreeMappingKey{Machine: "workstation", PathPrefix: "/work/a"},
		delta.Deletes[0])

	empty, err := db.LoadWorktreeMappingPublicationDelta(ctx, through, through)
	require.NoError(err)
	assert.Empty(empty.Mappings)
	assert.Empty(empty.Deletes)

	_, err = db.LoadWorktreeMappingPublicationDelta(ctx, through, revAfterFirst)
	assert.Error(err, "inverted window must be rejected")
}

func TestListAllWorktreeProjectMappings(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	for _, m := range []WorktreeProjectMapping{
		{Machine: "workstation", PathPrefix: "/work/a",
			Layout: WorktreeMappingLayoutExplicit, Project: "alpha"},
		{Machine: "laptop", PathPrefix: "/work/b",
			Layout: WorktreeMappingLayoutExplicit, Project: "beta"},
	} {
		_, err := db.CreateWorktreeProjectMapping(ctx, m)
		require.NoError(t, err)
	}
	all, err := db.ListAllWorktreeProjectMappings(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}
