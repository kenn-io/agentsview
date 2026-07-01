package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeProjectMappingsCRUDNormalizesAndScopesByMachine(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	prefix := filepath.Join(t.TempDir(), "my-app.worktrees")
	m, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: prefix + string(filepath.Separator),
		Project:    "my-app",
		Enabled:    true,
	})
	require.NoError(t, err, "create mapping")
	assert.Equal(t, "laptop", m.Machine, "machine")
	assert.Equal(t, prefix, m.PathPrefix, "path_prefix")
	assert.Equal(t, "my_app", m.Project, "project")

	got, err := d.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "list laptop mappings")
	require.Len(t, got, 1, "laptop mappings")
	assert.Equal(t, m.ID, got[0].ID, "laptop mapping ID")

	other, err := d.ListWorktreeProjectMappings(ctx, "server")
	require.NoError(t, err, "list server mappings")
	assert.Empty(t, other, "server mappings")
}

func TestWorktreeProjectMappingsRejectInvalidAndDuplicateRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	prefix := filepath.Join(t.TempDir(), "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: " ", Project: "repo", Enabled: true,
	})
	require.Error(t, err, "empty path prefix accepted")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: " ", Enabled: true,
	})
	require.Error(t, err, "empty project accepted")

	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create first mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo2", Enabled: true,
	})
	require.ErrorIs(t, err, ErrWorktreeMappingDuplicate)
}

func TestResolveWorktreeProjectMappingUsesLongestPrefixAndBoundaries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	broad := filepath.Join(root, "repo.worktrees")
	nested := filepath.Join(broad, "special")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: broad, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create broad mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: nested, Project: "special-repo", Enabled: true,
	})
	require.NoError(t, err, "create nested mapping")

	project, ok, err := d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(nested, "feat", "thing"), "leaf")
	require.NoError(t, err, "resolve nested")
	assert.True(t, ok, "nested resolve")
	assert.Equal(t, "special_repo", project, "nested resolve")

	project, ok, err = d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(broad, "feat", "thing"), "leaf")
	require.NoError(t, err, "resolve broad")
	assert.True(t, ok, "broad resolve")
	assert.Equal(t, "repo", project, "broad resolve")

	_, ok, err = d.ResolveWorktreeProjectMapping(ctx, "laptop", broad+"-other", "leaf")
	require.NoError(t, err, "resolve boundary miss")
	assert.False(t, ok,
		"path with shared string prefix matched across component boundary")

	project, ok = ResolveWorktreeProjectFromMappings(
		[]WorktreeProjectMapping{
			{PathPrefix: broad, Project: "repo"},
			{PathPrefix: nested, Project: "special_repo"},
		},
		filepath.Join(nested, "feat", "thing"),
		"leaf",
	)
	assert.True(t, ok, "unsorted resolve")
	assert.Equal(t, "special_repo", project, "unsorted resolve")
}

func TestResolveWorktreeProjectMappingMatchesRootPrefix(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: string(filepath.Separator),
		Project:    "root-project",
		Enabled:    true,
	})
	require.NoError(t, err, "create root mapping")

	project, ok, err := d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(string(filepath.Separator), "tmp", "worktree"), "leaf")
	require.NoError(t, err, "resolve root")
	assert.True(t, ok, "root resolve")
	assert.Equal(t, "root_project", project, "root resolve")
}

func TestApplyWorktreeProjectMappingsUpdatesOnlyCurrentMachineAndEnabledRows(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	prefix := filepath.Join(root, "repo.worktrees")
	disabledPrefix := filepath.Join(root, "disabled.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create enabled mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: disabledPrefix, Project: "disabled", Enabled: false,
	})
	require.NoError(t, err, "create disabled mapping")

	insert := func(id, machine, project, cwd string) {
		t.Helper()
		err := d.UpsertSession(Session{
			ID: id, Project: project, Machine: machine, Agent: "claude", Cwd: cwd,
		})
		require.NoError(t, err, "insert %s", id)
	}
	insert("match", "laptop", "leaf", filepath.Join(prefix, "feat", "thing"))
	insert("same-project", "laptop", "repo", filepath.Join(prefix, "bugfix"))
	insert("other-machine", "server", "leaf", filepath.Join(prefix, "feat", "thing"))
	insert("disabled", "laptop", "leaf", filepath.Join(disabledPrefix, "feat"))
	insert("trashed", "laptop", "leaf", filepath.Join(prefix, "trashed"))
	require.NoError(t, d.SoftDeleteSession("trashed"), "trash session")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "match", "repo")
	assertSessionProject(t, d, "same-project", "repo")
	assertSessionProject(t, d, "other-machine", "leaf")
	assertSessionProject(t, d, "disabled", "leaf")
	assertFullSessionProject(t, d, "trashed", "leaf")
}

func TestApplyWorktreeProjectMappingsBumpsLocalModifiedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	prefix := filepath.Join(t.TempDir(), "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID: "match", Project: "leaf", Machine: "laptop", Agent: "claude",
		Cwd: filepath.Join(prefix, "feat"),
	}), "insert match")

	before, err := d.GetSessionFull(ctx, "match")
	require.NoError(t, err, "GetSessionFull before")
	require.Nil(t, before.LocalModifiedAt, "local_modified_at before")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	require.Equal(t, 1, result.UpdatedSessions, "updated sessions")

	after, err := d.GetSessionFull(ctx, "match")
	require.NoError(t, err, "GetSessionFull after")
	assert.Equal(t, "repo", after.Project, "project")
	require.NotNil(t, after.LocalModifiedAt, "local_modified_at after")
	assert.NotEmpty(t, *after.LocalModifiedAt, "local_modified_at after")
}

func TestApplyWorktreeProjectMappingsToSessionUsesCurrentSessionState(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	stalePrefix := filepath.Join(root, "stale.worktrees")
	currentPrefix := filepath.Join(root, "current.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: stalePrefix, Project: "stale-repo", Enabled: true,
	})
	require.NoError(t, err, "create stale mapping")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: currentPrefix, Project: "current-repo", Enabled: true,
	})
	require.NoError(t, err, "create current mapping")

	staleCwd := filepath.Join(stalePrefix, "feat")
	currentCwd := filepath.Join(currentPrefix, "feat")
	require.NoError(t, d.UpsertSession(Session{
		ID: "match", Project: "leaf", Machine: "laptop", Agent: "claude",
		Cwd: staleCwd,
	}), "insert stale match")
	require.NoError(t, d.UpsertSession(Session{
		ID: "match", Project: "other_leaf", Machine: "laptop", Agent: "claude",
		Cwd: currentCwd,
	}), "move session before apply")

	updated, err := d.ApplyWorktreeProjectMappingToSession(
		ctx, "laptop", "match", staleCwd, "leaf",
	)
	require.NoError(t, err, "ApplyWorktreeProjectMappingToSession")
	require.True(t, updated, "updated")
	assertSessionProject(t, d, "match", "current_repo")
}

func TestApplyWorktreeProjectMappingToSessionFromSyncDoesNotBumpLocalModifiedAt(
	t *testing.T,
) {

	d := testDB(t)
	ctx := context.Background()
	prefix := filepath.Join(t.TempDir(), "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID: "match", Project: "leaf", Machine: "laptop", Agent: "claude",
		Cwd: filepath.Join(prefix, "feat"),
	}), "insert match")

	before, err := d.GetSessionFull(ctx, "match")
	require.NoError(t, err, "GetSessionFull before")
	require.Nil(t, before.LocalModifiedAt, "local_modified_at before")

	updated, err := d.ApplyWorktreeProjectMappingToSessionFromSync(
		ctx, "laptop", "match", before.Cwd, before.Project,
	)
	require.NoError(t, err, "ApplyWorktreeProjectMappingToSessionFromSync")
	require.True(t, updated, "updated")

	after, err := d.GetSessionFull(ctx, "match")
	require.NoError(t, err, "GetSessionFull after")
	assert.Equal(t, "repo", after.Project, "project")
	assert.Nil(t, after.LocalModifiedAt, "local_modified_at after")
}

func assertSessionProject(t *testing.T, d *DB, id, want string) {
	t.Helper()
	got, err := d.GetSession(context.Background(), id)
	require.NoError(t, err, "GetSession %s", id)
	assert.Equal(t, want, got.Project, "session %s project", id)
}

func TestWorktreeProjectMappingsFinalMetadataCopyRefreshesStalePrecopy(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	defer srcDB.Close()

	prefix := filepath.Join(dir, "app.worktrees")
	sourceMapping, err := srcDB.CreateWorktreeProjectMapping(
		ctx,
		WorktreeProjectMapping{
			Machine:    "laptop",
			PathPrefix: prefix,
			Project:    "old-project",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping src")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	require.NoError(
		t,
		dstDB.CopyWorktreeProjectMappingsFrom(srcPath),
		"CopyWorktreeProjectMappingsFrom",
	)

	_, err = srcDB.UpdateWorktreeProjectMapping(
		ctx,
		"laptop",
		sourceMapping.ID,
		WorktreeProjectMapping{
			PathPrefix: prefix,
			Project:    "new-project",
			Enabled:    false,
		},
	)
	require.NoError(t, err, "UpdateWorktreeProjectMapping src")
	require.NoError(t, srcDB.CloseConnections(), "CloseConnections src")

	_, err = dstDB.getWriter().ExecContext(ctx, `
		UPDATE worktree_project_mappings
		SET updated_at = '9999-12-31T23:59:59.999Z'
		WHERE machine = ? AND path_prefix = ?`,
		"laptop",
		prefix,
	)
	require.NoError(t, err, "force dst updated_at ahead")

	require.NoError(
		t,
		dstDB.CopySessionMetadataFrom(srcPath),
		"CopySessionMetadataFrom",
	)

	got, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "ListWorktreeProjectMappings")
	require.Len(t, got, 1, "mapping count")
	assert.Equal(t, "new_project", got[0].Project, "project")
	assert.False(t, got[0].Enabled, "mapping should reflect disabled source row")
}

func TestWorktreeProjectMappingsFinalMetadataCopyRemovesDeletedPrecopy(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	defer srcDB.Close()

	prefix := filepath.Join(dir, "app.worktrees")
	sourceMapping, err := srcDB.CreateWorktreeProjectMapping(
		ctx,
		WorktreeProjectMapping{
			Machine:    "laptop",
			PathPrefix: prefix,
			Project:    "old-project",
			Enabled:    true,
		},
	)
	require.NoError(t, err, "CreateWorktreeProjectMapping src")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	require.NoError(
		t,
		dstDB.CopyWorktreeProjectMappingsFrom(srcPath),
		"CopyWorktreeProjectMappingsFrom",
	)

	require.NoError(
		t,
		srcDB.DeleteWorktreeProjectMapping(
			ctx, "laptop", sourceMapping.ID,
		),
		"DeleteWorktreeProjectMapping src",
	)
	require.NoError(t, srcDB.CloseConnections(), "CloseConnections src")

	require.NoError(
		t,
		dstDB.CopySessionMetadataFrom(srcPath),
		"CopySessionMetadataFrom",
	)

	got, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "ListWorktreeProjectMappings")
	assert.Empty(t, got, "mapping count")
}

func assertFullSessionProject(t *testing.T, d *DB, id, want string) {
	t.Helper()
	got, err := d.GetSessionFull(context.Background(), id)
	require.NoError(t, err, "GetSessionFull %s", id)
	assert.Equal(t, want, got.Project, "session %s project", id)
}
