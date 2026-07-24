package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
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
	assert.Equal(t, normalizedMappingPath(prefix), m.PathPrefix, "path_prefix")
	assert.Equal(t, WorktreeMappingLayoutExplicit, m.Layout, "layout")
	assert.Equal(t, "my_app", m.Project, "project")

	got, err := d.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "list laptop mappings")
	require.Len(t, got, 1, "laptop mappings")
	assert.Equal(t, m.ID, got[0].ID, "laptop mapping ID")
	assert.Equal(t, WorktreeMappingLayoutExplicit, got[0].Layout, "listed layout")

	other, err := d.ListWorktreeProjectMappings(ctx, "server")
	require.NoError(t, err, "list server mappings")
	assert.Empty(t, other, "server mappings")
}

func TestWorktreeProjectMappingOriginalProjectIsSetOnce(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	prefix := filepath.Join(t.TempDir(), "service.worktrees")

	created, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:         "host-a.example",
		PathPrefix:      prefix,
		Project:         "service",
		OriginalProject: "branch-label",
		Enabled:         true,
	})
	require.NoError(t, err, "create mapping with original project")
	assert.Equal(t, "branch-label", created.OriginalProject)

	edited, err := d.UpdateWorktreeProjectMapping(
		ctx,
		created.Machine,
		created.ID,
		WorktreeProjectMapping{
			PathPrefix:      prefix,
			Project:         "renamed-service",
			OriginalProject: "replacement-label",
			Enabled:         true,
		},
	)
	require.NoError(t, err, "edit mapping with an existing original project")
	assert.Equal(t, "branch-label", edited.OriginalProject,
		"non-empty original_project cannot be overwritten")

	settingsCreated, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "host-a.example",
		PathPrefix: filepath.Join(t.TempDir(), "other.worktrees"),
		Project:    "other-service",
		Enabled:    true,
	})
	require.NoError(t, err, "create Settings-style mapping")
	assert.Empty(t, settingsCreated.OriginalProject)

	filled, err := d.UpdateWorktreeProjectMapping(
		ctx,
		settingsCreated.Machine,
		settingsCreated.ID,
		WorktreeProjectMapping{
			PathPrefix:      settingsCreated.PathPrefix,
			Project:         settingsCreated.Project,
			OriginalProject: "activity-label",
			Enabled:         true,
		},
	)
	require.NoError(t, err, "fill original project once")
	assert.Equal(t, "activity-label", filled.OriginalProject,
		"an empty Settings-created value may be filled once")
}

func TestWorktreeProjectMappingMachinesIncludeSessionsAndMappings(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertSession(Session{
		ID: "remote-session", Machine: "host-a.example", Agent: "claude",
		Project: "service",
	}), "insert remote session")
	require.NoError(t, d.UpsertSession(Session{
		ID: "deleted-session", Machine: "deleted.example", Agent: "claude",
		Project: "service",
	}), "insert deleted remote session")
	_, err := d.getWriter().ExecContext(ctx,
		`UPDATE sessions SET deleted_at = ? WHERE id = ?`,
		"2026-07-16T00:00:00Z", "deleted-session")
	require.NoError(t, err, "mark remote session deleted")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "host-b.example", PathPrefix: filepath.Join(t.TempDir(), "service"),
		Project: "service", Enabled: true,
	})
	require.NoError(t, err, "create remote mapping")

	machines, err := d.ListWorktreeProjectMappingMachines(ctx)
	require.NoError(t, err, "list mapping machines")
	assert.Equal(t, []string{"host-a.example", "host-b.example"}, machines)
}

func TestSchemaColumnMigrationAddsWorktreeOriginalProject(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(path)
	require.NoError(t, err, "open current archive")
	require.NoError(t, d.Close(), "close current archive")

	legacy, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open archive as legacy sqlite")
	_, err = legacy.Exec(
		`ALTER TABLE worktree_project_mappings DROP COLUMN original_project`,
	)
	require.NoError(t, err, "remove post-legacy column")
	_, err = legacy.Exec(`
		INSERT INTO worktree_project_mappings
			(machine, path_prefix, layout, project, enabled)
		VALUES ('host-a.example', '/srv/worktrees/service', 'explicit', 'service', 1)`)
	require.NoError(t, err, "seed legacy mapping")
	require.NoError(t, legacy.Close(), "close legacy sqlite")

	migrated, err := Open(path)
	require.NoError(t, err, "open and migrate archive")
	defer migrated.Close()
	mappings, err := migrated.ListWorktreeProjectMappings(ctx, "host-a.example")
	require.NoError(t, err, "list migrated mapping")
	require.Len(t, mappings, 1)
	assert.Empty(t, mappings[0].OriginalProject,
		"legacy mappings default original project to empty")
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
		Machine: "laptop", PathPrefix: prefix, Layout: "bogus", Project: "repo",
		Enabled: true,
	})
	require.Error(t, err, "invalid layout accepted")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo2", Enabled: true,
	})
	require.ErrorIs(t, err, ErrWorktreeMappingDuplicate)
}

func TestWorktreeProjectMappingsLayoutResolution(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	layoutRoot := filepath.Join(root, "service")
	layoutPrefix := filepath.Join(layoutRoot, "service.worktrees")

	explicitMapping, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: filepath.Join(layoutPrefix, "feature"),
		Project:    "feature-service",
		Enabled:    true,
	})
	require.NoError(t, err, "create explicit mapping")
	require.Equal(t, WorktreeMappingLayoutExplicit, explicitMapping.Layout)

	layoutMapping, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: layoutRoot,
		Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err, "create layout mapping")
	assert.Equal(t, WorktreeMappingLayoutRepoDotWorktrees, layoutMapping.Layout)
	assert.Empty(t, layoutMapping.Project, "layout rows should not persist a project")

	project, ok, err := d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(layoutPrefix, "feature", "src"), "leaf")
	require.NoError(t, err, "resolve explicit")
	assert.True(t, ok, "explicit resolve")
	assert.Equal(t, "feature_service", project, "explicit resolve")

	project, ok, err = d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(layoutRoot, "service.worktrees", "main"), "leaf")
	require.NoError(t, err, "resolve layout")
	assert.True(t, ok, "layout resolve")
	assert.Equal(t, "service", project, "layout resolve")

	_, ok, err = d.ResolveWorktreeProjectMapping(ctx, "laptop",
		filepath.Join(layoutRoot, "service.worktrees-other", "main"), "leaf")
	require.NoError(t, err, "resolve boundary miss")
	assert.False(t, ok, "shared string prefix must not match")
}

func TestResolveWorktreeProjectFromSortedMappings(t *testing.T) {
	root := t.TempDir()
	layoutRoot := filepath.Join(root, "service")
	layoutPrefix := filepath.Join(layoutRoot, "service.worktrees")
	cwd := filepath.Join(layoutPrefix, "feature", "src")

	mappings := []WorktreeProjectMapping{
		{
			PathPrefix: filepath.Join(layoutPrefix, "feature"),
			Layout:     WorktreeMappingLayoutExplicit,
			Project:    "feature-service",
		},
		{
			PathPrefix: layoutRoot,
			Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		},
	}

	project, ok := ResolveWorktreeProjectFromSortedMappings(mappings, cwd, "leaf")
	assert.True(t, ok, "longest prefix resolve")
	assert.Equal(t, "feature-service", project, "longest prefix resolve")

	project, ok = ResolveWorktreeProjectFromMappings([]WorktreeProjectMapping{
		{
			PathPrefix: layoutRoot,
			Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		},
		{
			PathPrefix: filepath.Join(layoutPrefix, "feature"),
			Layout:     WorktreeMappingLayoutExplicit,
			Project:    "feature-service",
		},
	}, cwd, "leaf")
	assert.True(t, ok, "unsorted resolve")
	assert.Equal(t, "feature-service", project, "unsorted resolve")

	project, ok = ResolveWorktreeProjectFromSortedMappings([]WorktreeProjectMapping{
		{
			PathPrefix: layoutRoot,
			Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		},
	}, filepath.Join(layoutRoot, "service.worktrees", "main"), "leaf")
	assert.True(t, ok, "layout resolve")
	assert.Equal(t, "service", project, "layout resolve")

	project, ok = ResolveWorktreeProjectFromSortedMappings([]WorktreeProjectMapping{
		{
			PathPrefix: layoutRoot,
			Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		},
	}, filepath.Join(layoutRoot, "service.worktrees"), "leaf")
	assert.False(t, ok, "branchless layout root should not resolve")
	assert.Equal(t, "leaf", project, "branchless layout root project")

	project, ok = ResolveWorktreeProjectFromSortedMappings([]WorktreeProjectMapping{
		{
			PathPrefix: layoutRoot,
			Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		},
		{
			PathPrefix: root,
			Layout:     WorktreeMappingLayoutExplicit,
			Project:    "root-fallback",
		},
	}, filepath.Join(layoutRoot, "plain-worktree", "src"), "leaf")
	assert.True(t, ok, "fallback resolve")
	assert.Equal(t, "root-fallback", project, "fallback resolve")
}

func TestApplyWorktreeProjectMappingsLayout(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	layoutRoot := filepath.Join(root, "service")
	layoutPrefix := filepath.Join(layoutRoot, "service.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: layoutRoot,
		Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err, "create layout mapping")

	require.NoError(t, d.UpsertSession(Session{
		ID:      "bulk",
		Project: "leaf",
		Machine: "laptop",
		Agent:   "claude",
		Cwd:     filepath.Join(layoutPrefix, "feature"),
	}), "insert bulk session")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 1, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "bulk", "service")

	require.NoError(t, d.UpsertSession(Session{
		ID:      "single",
		Project: "leaf",
		Machine: "laptop",
		Agent:   "claude",
		Cwd:     filepath.Join(layoutPrefix, "single"),
	}), "insert single session")
	updated, err := d.ApplyWorktreeProjectMappingToSession(
		ctx, "laptop", "single", filepath.Join(layoutPrefix, "single"), "leaf",
	)
	require.NoError(t, err, "ApplyWorktreeProjectMappingToSession")
	assert.True(t, updated, "single session updated")
	assertSessionProject(t, d, "single", "service")

	filePath := filepath.Join(root, "session.jsonl")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "path",
		Project:  "leaf",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(layoutPrefix, "path"),
		FilePath: &filePath,
	}), "insert path session")
	result, err = d.ApplyWorktreeProjectMappingsToSessionsByPath(ctx, filePath)
	require.NoError(t, err, "apply mappings by path")
	assert.Equal(t, 1, result.MatchedSessions, "matched sessions by path")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions by path")
	assertSessionProject(t, d, "path", "service")
}

func TestWorktreeMappingEmptyCWD(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	layoutRoot := filepath.Join(root, "service")
	layoutPrefix := filepath.Join(layoutRoot, "service.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine:    "laptop",
		PathPrefix: layoutRoot,
		Layout:     WorktreeMappingLayoutRepoDotWorktrees,
		Enabled:    true,
	})
	require.NoError(t, err, "create layout mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(layoutPrefix, "feature"),
		FilePath: &filePath,
	}), "insert reference row")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 2, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "service")
	assertSessionProject(t, d, "reference", "service")
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

func TestResolveWorktreeProjectMappingPreservesPortableRootIdentity(t *testing.T) {
	tests := []struct {
		name        string
		prefix      string
		cwd         string
		project     string
		wantProject string
		wantMatch   bool
		wantStored  string
	}{
		{
			name: "windows drive root", prefix: `C:\`, cwd: `C:\worktrees\service`,
			project: "drive-root", wantProject: "drive_root",
			wantMatch: true, wantStored: "C:/",
		},
		{
			name: "drive relative does not match drive absolute", prefix: `C:`,
			cwd: `C:\worktrees\service`, project: "drive-relative", wantStored: "C:",
		},
		{
			name: "UNC share root", prefix: `\\server\share\`,
			cwd: `\\server\share\worktrees\service`, project: "unc-root",
			wantProject: "unc_root", wantMatch: true, wantStored: "//server/share/",
		},
		{
			name: "POSIX path does not match UNC path", prefix: `/server/share`,
			cwd: `\\server\share\worktrees\service`, project: "posix-root",
			wantStored: "/server/share",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			mapping, err := d.CreateWorktreeProjectMapping(
				context.Background(),
				WorktreeProjectMapping{
					Machine: "portable.example", PathPrefix: tt.prefix,
					Project: tt.project, Enabled: true,
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStored, mapping.PathPrefix)

			project, matched, err := d.ResolveWorktreeProjectMapping(
				context.Background(), "portable.example", tt.cwd, "leaf",
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantMatch, matched)
			if tt.wantMatch {
				assert.Equal(t, tt.wantProject, project)
			} else {
				assert.Equal(t, "leaf", project)
			}
		})
	}
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

func TestApplyWorktreeProjectMappings_EmptyCwdSiblingFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefix := filepath.Join(root, "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefix, "feat", "task"),
		FilePath: &filePath,
	}), "insert fallback reference row")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 2, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "repo")
	assertSessionProject(t, d, "reference", "repo")

	emptyRow, err := d.GetSession(ctx, "empty-cwd")
	require.NoError(t, err, "GetSession empty-cwd")
	require.NotNil(t, emptyRow, "empty-cwd session")
	assert.Equal(t, "", emptyRow.Cwd, "stored cwd unchanged")
}

func TestApplyWorktreeProjectMappings_EmptyCwdConflictingSiblingsNoFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefixA := filepath.Join(root, "repo-a.worktrees")
	prefixB := filepath.Join(root, "repo-b.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixA, Project: "repo-a", Enabled: true,
	})
	require.NoError(t, err, "create mapping A")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixB, Project: "repo-b", Enabled: true,
	})
	require.NoError(t, err, "create mapping B")

	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-a",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixA, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping A")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-b",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixB, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping B")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 2, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "stale")
	assertSessionProject(t, d, "reference-a", "repo_a")
	assertSessionProject(t, d, "reference-b", "repo_b")
}

func TestApplyWorktreeProjectMappings_EmptyCwdUnmappedSiblingNoFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefixA := filepath.Join(root, "repo-a.worktrees")
	unmappedCwd := filepath.Join(root, "unrelated", "dir")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixA, Project: "repo-a", Enabled: true,
	})
	require.NoError(t, err, "create mapping A")

	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-mapped",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixA, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping A")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-unmapped",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      unmappedCwd,
		FilePath: &filePath,
	}), "insert reference row with no matching mapping")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 1, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "stale")
	assertSessionProject(t, d, "reference-mapped", "repo_a")
	assertSessionProject(t, d, "reference-unmapped", "stale")
}

func TestApplyWorktreeProjectMappings_EmptyCwdNoSiblingNoUpdate(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	prefix := filepath.Join(root, "repo.worktrees")
	filePath := filepath.Join(root, "session.jsonl")
	otherFilePath := filepath.Join(root, "other-session.jsonl")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd-no-sibling",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-unrelated",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefix, "feat"),
		FilePath: &otherFilePath,
	}), "insert unrelated non-empty row")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 1, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd-no-sibling", "stale")
	assertSessionProject(t, d, "reference-unrelated", "repo")

	emptyRow, err := d.GetSession(ctx, "empty-cwd-no-sibling")
	require.NoError(t, err, "GetSession empty-cwd-no-sibling")
	require.NotNil(t, emptyRow, "empty-cwd-no-sibling session")
	assert.Equal(t, "", emptyRow.Cwd, "stored cwd unchanged")
}

func TestApplyWorktreeProjectMappings_EmptyCwdSameProjectNoUpdate(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefix := filepath.Join(root, "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd-already-matched",
		Project:  "repo",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd same project row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefix, "feat"),
		FilePath: &filePath,
	}), "insert reference row")

	result, err := d.ApplyWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "apply mappings")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd-already-matched", "repo")
	assertSessionProject(t, d, "reference", "repo")
}

func TestApplyWorktreeProjectMappingsToSessionsByPath_EmptyCwdSiblingFallback(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefix := filepath.Join(root, "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefix, "feat"),
		FilePath: &filePath,
	}), "insert reference row")

	result, err := d.ApplyWorktreeProjectMappingsToSessionsByPath(ctx, filePath)
	require.NoError(t, err, "apply mappings by path")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 2, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "repo")
	assertSessionProject(t, d, "reference", "repo")
}

func TestApplyWorktreeProjectMappingsToSessionsByPath_EmptyCwdConflictingSiblingsNoFallback(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefixA := filepath.Join(root, "repo-a.worktrees")
	prefixB := filepath.Join(root, "repo-b.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixA, Project: "repo-a", Enabled: true,
	})
	require.NoError(t, err, "create mapping A")
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixB, Project: "repo-b", Enabled: true,
	})
	require.NoError(t, err, "create mapping B")

	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-a",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixA, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping A")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-b",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixB, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping B")

	result, err := d.ApplyWorktreeProjectMappingsToSessionsByPath(ctx, filePath)
	require.NoError(t, err, "apply mappings by path")
	assert.Equal(t, 2, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 2, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "stale")
	assertSessionProject(t, d, "reference-a", "repo_a")
	assertSessionProject(t, d, "reference-b", "repo_b")
}

func TestApplyWorktreeProjectMappingsToSessionsByPath_EmptyCwdUnmappedSiblingNoFallback(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefixA := filepath.Join(root, "repo-a.worktrees")
	unmappedCwd := filepath.Join(root, "unrelated", "dir")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefixA, Project: "repo-a", Enabled: true,
	})
	require.NoError(t, err, "create mapping A")

	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd row")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-mapped",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      filepath.Join(prefixA, "feat"),
		FilePath: &filePath,
	}), "insert reference row resolving to mapping A")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "reference-unmapped",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      unmappedCwd,
		FilePath: &filePath,
	}), "insert reference row with no matching mapping")

	result, err := d.ApplyWorktreeProjectMappingsToSessionsByPath(ctx, filePath)
	require.NoError(t, err, "apply mappings by path")
	assert.Equal(t, 1, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 1, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd", "stale")
	assertSessionProject(t, d, "reference-mapped", "repo_a")
	assertSessionProject(t, d, "reference-unmapped", "stale")
}

func TestApplyWorktreeProjectMappingsToSessionsByPath_EmptyCwdOnlyEmptySiblingsNoMatch(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	root := t.TempDir()
	filePath := filepath.Join(root, "session.jsonl")
	prefix := filepath.Join(root, "repo.worktrees")

	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: prefix, Project: "repo", Enabled: true,
	})
	require.NoError(t, err, "create mapping")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd-a",
		Project:  "stale",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd sibling A")
	require.NoError(t, d.UpsertSession(Session{
		ID:       "empty-cwd-b",
		Project:  "other",
		Machine:  "laptop",
		Agent:    "claude",
		Cwd:      "",
		FilePath: &filePath,
	}), "insert empty cwd sibling B")

	result, err := d.ApplyWorktreeProjectMappingsToSessionsByPath(ctx, filePath)
	require.NoError(t, err, "apply mappings by path")
	assert.Equal(t, 0, result.MatchedSessions, "matched sessions")
	assert.Equal(t, 0, result.UpdatedSessions, "updated sessions")
	assertSessionProject(t, d, "empty-cwd-a", "stale")
	assertSessionProject(t, d, "empty-cwd-b", "other")
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

func TestApplyWorktreeProjectMappingToSessionReconcilesOnlyMovedIdentityKey(
	t *testing.T,
) {
	t.Run("former key keeps another contributor", func(t *testing.T) {
		d := testDB(t)
		ctx := context.Background()
		prefix := filepath.Join(t.TempDir(), "service.worktrees")
		rootPath := "/srv/repos/service"
		gitRemote := "https://example.com/example/service.git"

		_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
			Machine: "test-host", PathPrefix: prefix,
			Project: "target-project", Enabled: true,
		})
		require.NoError(t, err)
		seedMappingIdentitySession(t, d, Session{
			ID: "retained", Machine: "test-host", Agent: "claude",
			Project: "source_project", Cwd: "/srv/elsewhere/service",
		}, export.ProjectIdentityObservation{
			RootPath: rootPath, GitRemote: gitRemote, GitBranch: "retained",
			ObservedAt: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC),
		})
		seedMappingIdentitySession(t, d, Session{
			ID: "moved", Machine: "test-host", Agent: "claude",
			Project: "source_project", Cwd: filepath.Join(prefix, "feature"),
		}, export.ProjectIdentityObservation{
			RootPath: rootPath, GitRemote: gitRemote, GitBranch: "moved",
			ObservedAt: time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC),
		})

		updated, err := d.ApplyWorktreeProjectMappingToSessionFromSync(
			ctx, "test-host", "moved", filepath.Join(prefix, "feature"),
			"source_project",
		)
		require.NoError(t, err)
		require.True(t, updated)

		observations, err := d.ListProjectIdentityObservations(
			ctx, []string{"source_project", "target_project"},
		)
		require.NoError(t, err)
		require.Len(t, observations, 2)
		assert.Equal(t, "retained",
			findIdentityObservation(t, observations, "source_project").GitBranch)
		assert.Equal(t, "moved",
			findIdentityObservation(t, observations, "target_project").GitBranch)

		var sourceSnapshots int
		require.NoError(t, d.getReader().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM session_project_identity_snapshots
			WHERE project = ? AND session_id IN (?, ?)`,
			"source_project", "retained", "moved",
		).Scan(&sourceSnapshots))
		assert.Equal(t, 2, sourceSnapshots,
			"immutable snapshots keep their parser-time project")
	})

	t.Run("unsupported former key publishes tombstone", func(t *testing.T) {
		d := testDB(t)
		ctx := context.Background()
		prefix := filepath.Join(t.TempDir(), "service.worktrees")
		_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
			Machine: "test-host", PathPrefix: prefix,
			Project: "target-project", Enabled: true,
		})
		require.NoError(t, err)
		seedMappingIdentitySession(t, d, Session{
			ID: "moved", Machine: "test-host", Agent: "claude",
			Project: "source_project", Cwd: filepath.Join(prefix, "feature"),
		}, export.ProjectIdentityObservation{
			RootPath:   "/srv/repos/service",
			GitRemote:  "https://example.com/example/service.git",
			ObservedAt: time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC),
		})
		before, err := d.ProjectIdentityPublicationRevision(ctx)
		require.NoError(t, err)

		updated, err := d.ApplyWorktreeProjectMappingToSessionFromSync(
			ctx, "test-host", "moved", filepath.Join(prefix, "feature"),
			"source_project",
		)
		require.NoError(t, err)
		require.True(t, updated)
		after, err := d.ProjectIdentityPublicationRevision(ctx)
		require.NoError(t, err)

		observations, err := d.ListProjectIdentityObservations(
			ctx, []string{"source_project", "target_project"},
		)
		require.NoError(t, err)
		require.Len(t, observations, 1)
		assert.Equal(t, "target_project", observations[0].Project)

		delta, err := d.LoadProjectIdentityPublicationDelta(
			ctx, before, after, nil, nil,
		)
		require.NoError(t, err)
		require.Len(t, delta.ObservationDeletes, 1)
		assert.Equal(t, ProjectIdentityObservationKey{
			Project: "source_project", Machine: "test-host",
			RootPath:  "/srv/repos/service",
			GitRemote: "https://example.com/example/service.git",
		}, delta.ObservationDeletes[0])
	})
}

func TestApplyWorktreeProjectMappingToSessionIdentityWorkIsCardinalityBounded(
	t *testing.T,
) {
	measure := func(t *testing.T, unrelatedKeys int) int64 {
		t.Helper()
		d := testDB(t)
		ctx := context.Background()
		prefix := filepath.Join(t.TempDir(), "service.worktrees")
		_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
			Machine: "test-host", PathPrefix: prefix,
			Project: "target-project", Enabled: true,
		})
		require.NoError(t, err)
		seedMappingIdentitySession(t, d, Session{
			ID: "moved", Machine: "test-host", Agent: "claude",
			Project: "source_project", Cwd: filepath.Join(prefix, "feature"),
		}, export.ProjectIdentityObservation{
			RootPath:   "/srv/repos/service",
			GitRemote:  "https://example.com/example/service.git",
			ObservedAt: time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC),
		})
		for i := range unrelatedKeys {
			id := fmt.Sprintf("unrelated-%04d", i)
			seedMappingIdentitySession(t, d, Session{
				ID: id, Machine: "test-host", Agent: "claude",
				Project: "source_project", Cwd: "/srv/elsewhere/" + id,
			}, export.ProjectIdentityObservation{
				RootPath:   "/srv/repos/" + id,
				GitRemote:  "https://example.com/example/" + id + ".git",
				ObservedAt: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC),
			})
		}
		before, err := d.ProjectIdentityPublicationRevision(ctx)
		require.NoError(t, err)
		updated, err := d.ApplyWorktreeProjectMappingToSessionFromSync(
			ctx, "test-host", "moved", filepath.Join(prefix, "feature"),
			"source_project",
		)
		require.NoError(t, err)
		require.True(t, updated)
		after, err := d.ProjectIdentityPublicationRevision(ctx)
		require.NoError(t, err)
		return after - before
	}

	small := measure(t, 0)
	large := measure(t, 200)
	assert.Equal(t, small, large,
		"one session event must not rewrite unrelated identity keys")
}

func seedMappingIdentitySession(
	t *testing.T,
	d *DB,
	session Session,
	identity export.ProjectIdentityObservation,
) {
	t.Helper()
	require.NoError(t, d.UpsertSession(session))
	identity.SessionID = session.ID
	identity.Project = session.Project
	identity.Machine = session.Machine
	require.NoError(t, d.UpsertProjectIdentityObservation(
		context.Background(), identity,
	))
}

func findIdentityObservation(
	t *testing.T,
	observations []export.ProjectIdentityObservation,
	project string,
) export.ProjectIdentityObservation {
	t.Helper()
	for _, observation := range observations {
		if observation.Project == project {
			return observation
		}
	}
	require.FailNow(t, "identity observation not found", project)
	return export.ProjectIdentityObservation{}
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
			Machine:         "laptop",
			PathPrefix:      prefix,
			Project:         "old-project",
			OriginalProject: "activity-label",
			Enabled:         true,
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
	preCopied, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "list pre-copied mappings")
	require.Len(t, preCopied, 1)
	assert.Equal(t, "activity-label", preCopied[0].OriginalProject,
		"pre-copy preserves original project")

	_, err = srcDB.UpdateWorktreeProjectMapping(
		ctx,
		"laptop",
		sourceMapping.ID,
		WorktreeProjectMapping{
			PathPrefix:      prefix,
			Project:         "new-project",
			OriginalProject: "replacement-label",
			Enabled:         false,
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
	assert.Equal(t, "activity-label", got[0].OriginalProject, "original project")
	assert.False(t, got[0].Enabled, "mapping should reflect disabled source row")
}

func TestCopySessionMetadataFromPreservesExistingMappingOriginalProject(
	t *testing.T,
) {
	ctx := context.Background()
	tests := []struct {
		name         string
		createSource func(*testing.T, string, string)
		wantProject  string
		wantEnabled  bool
	}{
		{
			name: "different non-empty source",
			createSource: func(t *testing.T, path, prefix string) {
				t.Helper()
				src, err := Open(path)
				require.NoError(t, err, "open source")
				_, err = src.CreateWorktreeProjectMapping(
					ctx,
					WorktreeProjectMapping{
						Machine:         "host-a.example",
						PathPrefix:      prefix,
						Project:         "source-service",
						OriginalProject: "source-label",
						Enabled:         false,
					},
				)
				require.NoError(t, err, "create source mapping")
				require.NoError(t, src.Close(), "close source")
			},
			wantProject: "source_service",
			wantEnabled: false,
		},
		{
			name: "legacy source without original project",
			createSource: func(t *testing.T, path, prefix string) {
				t.Helper()
				createWorktreeMappingDBWithoutOriginalProject(t, path, prefix)
			},
			wantProject: "service",
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			prefix := filepath.Join(dir, "service.worktrees")
			srcPath := filepath.Join(dir, "src.db")
			tt.createSource(t, srcPath, prefix)

			dst, err := Open(filepath.Join(dir, "dst.db"))
			require.NoError(t, err, "open destination")
			defer dst.Close()
			_, err = dst.CreateWorktreeProjectMapping(
				ctx,
				WorktreeProjectMapping{
					Machine:         "host-a.example",
					PathPrefix:      prefix,
					Project:         "destination-service",
					OriginalProject: "destination-label",
					Enabled:         true,
				},
			)
			require.NoError(t, err, "create destination mapping")

			require.NoError(
				t,
				dst.CopySessionMetadataFrom(srcPath),
				"copy source metadata",
			)

			mappings, err := dst.ListWorktreeProjectMappings(
				ctx, "host-a.example",
			)
			require.NoError(t, err, "list destination mappings")
			require.Len(t, mappings, 1)
			assert.Equal(t, "destination-label", mappings[0].OriginalProject)
			assert.Equal(t, tt.wantProject, mappings[0].Project,
				"other source fields are still reconciled")
			assert.Equal(t, tt.wantEnabled, mappings[0].Enabled,
				"other source fields are still reconciled")
		})
	}
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

func TestCopyWorktreeProjectMappingsFromOldSchemaDefaultsLayout(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	srcPath := filepath.Join(dir, "old-src.db")
	prefix := filepath.Join(dir, "app.worktrees")
	createOldWorktreeMappingDB(t, srcPath, prefix)

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	require.NoError(
		t,
		dstDB.CopyWorktreeProjectMappingsFrom(srcPath),
		"CopyWorktreeProjectMappingsFrom",
	)

	got, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "ListWorktreeProjectMappings")
	require.Len(t, got, 1, "mapping count")
	assert.Equal(t, WorktreeMappingLayoutExplicit, got[0].Layout, "layout")
	assert.Equal(t, "old_project", got[0].Project, "project")
	assert.Empty(t, got[0].OriginalProject, "legacy original project")
	assert.True(t, got[0].Enabled, "enabled")
}

func TestCopyWorktreeProjectMappingsFromFillsOnlyEmptyOriginalProject(
	t *testing.T,
) {
	ctx := context.Background()
	tests := []struct {
		name                string
		destinationOriginal string
		wantOriginal        string
	}{
		{
			name:         "fills empty destination",
			wantOriginal: "source-label",
		},
		{
			name:                "preserves non-empty destination",
			destinationOriginal: "destination-label",
			wantOriginal:        "destination-label",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			prefix := filepath.Join(dir, "service.worktrees")
			sourcePath := filepath.Join(dir, "source.db")
			source, err := Open(sourcePath)
			require.NoError(t, err, "open source")
			_, err = source.CreateWorktreeProjectMapping(
				ctx,
				WorktreeProjectMapping{
					Machine:         "host-a.example",
					PathPrefix:      prefix,
					Layout:          WorktreeMappingLayoutRepoDotWorktrees,
					OriginalProject: "source-label",
					Enabled:         true,
				},
			)
			require.NoError(t, err, "create source mapping")
			require.NoError(t, source.Close(), "close source")

			destination, err := Open(filepath.Join(dir, "destination.db"))
			require.NoError(t, err, "open destination")
			defer destination.Close()
			owned, err := destination.CreateWorktreeProjectMapping(
				ctx,
				WorktreeProjectMapping{
					Machine:         "host-a.example",
					PathPrefix:      prefix,
					Project:         "destination-service",
					OriginalProject: tt.destinationOriginal,
					Enabled:         false,
				},
			)
			require.NoError(t, err, "create destination mapping")

			require.NoError(
				t,
				destination.CopyWorktreeProjectMappingsFrom(sourcePath),
				"copy source mappings",
			)

			mappings, err := destination.ListWorktreeProjectMappings(
				ctx, "host-a.example",
			)
			require.NoError(t, err, "list destination mappings")
			require.Len(t, mappings, 1)
			assert.Equal(t, tt.wantOriginal, mappings[0].OriginalProject)
			assert.Equal(t, owned.ID, mappings[0].ID, "destination owns the row")
			assert.Equal(t, "host-a.example", mappings[0].Machine)
			assert.Equal(t, normalizedMappingPath(prefix), mappings[0].PathPrefix)
			assert.Equal(t, WorktreeMappingLayoutExplicit, mappings[0].Layout)
			assert.Equal(t, "destination_service", mappings[0].Project)
			assert.False(t, mappings[0].Enabled)
			assert.Equal(t, owned.CreatedAt, mappings[0].CreatedAt)
			assert.Equal(t, owned.UpdatedAt, mappings[0].UpdatedAt)
		})
	}
}

func TestCopySessionMetadataFromOldWorktreeMappingSchemaDefaultsLayout(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	srcPath := filepath.Join(dir, "old-src.db")
	prefix := filepath.Join(dir, "app.worktrees")
	createOldWorktreeMappingDB(t, srcPath, prefix)

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	require.NoError(
		t,
		dstDB.CopySessionMetadataFrom(srcPath),
		"CopySessionMetadataFrom",
	)

	got, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	require.NoError(t, err, "ListWorktreeProjectMappings")
	require.Len(t, got, 1, "mapping count")
	assert.Equal(t, WorktreeMappingLayoutExplicit, got[0].Layout, "layout")
	assert.Equal(t, "old_project", got[0].Project, "project")
	assert.Empty(t, got[0].OriginalProject, "legacy original project")
	assert.True(t, got[0].Enabled, "enabled")
}

func TestCopyWorktreeMappingFromSchemaWithoutOriginalProjectDefaultsEmpty(
	t *testing.T,
) {
	dir := t.TempDir()
	ctx := context.Background()
	srcPath := filepath.Join(dir, "legacy-with-layout.db")
	prefix := filepath.Join(dir, "service.worktrees")
	createWorktreeMappingDBWithoutOriginalProject(t, srcPath, prefix)

	tests := []struct {
		name string
		copy func(*DB, string) error
	}{
		{
			name: "resync pre-copy",
			copy: func(dst *DB, source string) error {
				return dst.CopyWorktreeProjectMappingsFrom(source)
			},
		},
		{
			name: "final metadata reconciliation",
			copy: func(dst *DB, source string) error {
				return dst.CopySessionMetadataFrom(source)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst, err := Open(filepath.Join(dir, tt.name+".db"))
			require.NoError(t, err, "open destination")
			defer dst.Close()
			require.NoError(t, tt.copy(dst, srcPath), "copy legacy mapping")

			mappings, err := dst.ListWorktreeProjectMappings(ctx, "host-a.example")
			require.NoError(t, err, "list copied mappings")
			require.Len(t, mappings, 1)
			assert.Equal(t, WorktreeMappingLayoutExplicit, mappings[0].Layout)
			assert.Empty(t, mappings[0].OriginalProject)
		})
	}
}

func createWorktreeMappingDBWithoutOriginalProject(
	t *testing.T,
	path string,
	prefix string,
) {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open legacy sqlite")
	defer conn.Close()
	_, err = conn.Exec(`
		CREATE TABLE worktree_project_mappings (
			id INTEGER PRIMARY KEY,
			machine TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			layout TEXT NOT NULL DEFAULT 'explicit',
			project TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(machine, path_prefix)
		);
		INSERT INTO worktree_project_mappings (
			machine, path_prefix, layout, project, enabled
		) VALUES (
			'host-a.example', ?, 'explicit', 'service', 1
		);`, normalizedMappingPath(prefix))
	require.NoError(t, err, "seed legacy worktree mapping")
}

func createOldWorktreeMappingDB(t *testing.T, path string, prefix string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "open old sqlite")
	defer conn.Close()
	_, err = conn.Exec(`
		CREATE TABLE worktree_project_mappings (
			id INTEGER PRIMARY KEY,
			machine TEXT NOT NULL,
			path_prefix TEXT NOT NULL,
			project TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(machine, path_prefix)
		);
		INSERT INTO worktree_project_mappings (
			machine, path_prefix, project, enabled
		) VALUES (
			'laptop', ?, 'old_project', 1
		);`,
		normalizedMappingPath(prefix),
	)
	require.NoError(t, err, "seed old worktree mapping db")
}

func assertFullSessionProject(t *testing.T, d *DB, id, want string) {
	t.Helper()
	got, err := d.GetSessionFull(context.Background(), id)
	require.NoError(t, err, "GetSessionFull %s", id)
	assert.Equal(t, want, got.Project, "session %s project", id)
}
