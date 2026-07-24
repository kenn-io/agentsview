package db

import (
	"context"
	"fmt"
	"testing"

	"go.kenn.io/agentsview/internal/export"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeReclassificationPreviewUsesBoundaryAndPortablePaths(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedReclassificationSession(t, d, "unix-root", "archive.example", "/worktrees/service", "branch")
	seedReclassificationSession(t, d, "unix-child", "archive.example", "/worktrees/service/cmd", "branch")
	seedReclassificationSession(t, d, "unix-neighbor", "archive.example", "/worktrees/service-old", "neighbor")
	seedReclassificationSession(t, d, "windows-child", "windows.example", `C:\worktrees\service\subdir`, "branch")

	unixPreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service-name", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, unixPreview.MatchedSessions)
	assert.Equal(t, 2, unixPreview.UpdatedSessions)
	assert.Equal(t, 1, unixPreview.DistinctProjects)
	assert.Equal(t, "service_name", unixPreview.NormalizedProject)

	windowsPreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "windows.example", PathPrefix: `C:\worktrees\service`,
		Project: "service", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, windowsPreview.MatchedSessions)
	assert.Equal(t, 1, windowsPreview.UpdatedSessions)
}

func TestWorktreeMappingSetTokenIncludesOriginalProject(t *testing.T) {
	base := WorktreeProjectMapping{
		ID: 1, Machine: "machine", PathPrefix: "/work/repo",
		Project: "target", Enabled: true, UpdatedAt: "2026-01-01T00:00:00Z",
	}
	withOriginal := base
	withOriginal.OriginalProject = "source"
	assert.NotEqual(t,
		worktreeMappingSetToken([]WorktreeProjectMapping{base}),
		worktreeMappingSetToken([]WorktreeProjectMapping{withOriginal}),
	)
}

func TestReclassificationLegacyUpdateMapsDuplicateConstraint(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	first, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "machine", PathPrefix: "/work/first",
		Project: "first", Enabled: true,
	})
	require.NoError(t, err)
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "machine", PathPrefix: "/work/second",
		Project: "second", Enabled: true,
	})
	require.NoError(t, err)

	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = upsertWorktreeReclassificationMappingTx(
		ctx, tx,
		WorktreeProjectMapping{
			Machine: "machine", PathPrefix: "/work/second",
			Project: "replacement", Enabled: true,
		},
		&first,
	)
	require.ErrorIs(t, err, ErrWorktreeMappingDuplicate)
}

func TestWorktreeReclassificationPreviewHonorsSpecificRuleAndBoundsSamples(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	for i := range 14 {
		seedReclassificationSession(t, d, fmt.Sprintf("session-%02d", i),
			"archive.example", fmt.Sprintf("/worktrees/service/branch-%02d", i),
			fmt.Sprintf("branch_%02d", i))
	}
	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: "/worktrees/service/branch-09",
		Project: "specific", Enabled: true,
	})
	require.NoError(t, err)

	preview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 14, preview.MatchedSessions)
	assert.Equal(t, 14, preview.UpdatedSessions)
	assert.Equal(t, 14, preview.DistinctProjects)
	assert.Len(t, preview.ProjectSamples, 10)
	assert.Len(t, preview.SessionSamples, 10)
	assert.Equal(t, "branch_00", preview.ProjectSamples[0].Project)
	assert.Equal(t, "session-00", preview.SessionSamples[0].ID)
	assert.Equal(t, "specific", preview.SessionSamples[9].NextProject,
		"the specific mapping must remain authoritative")
}

func TestWorktreeReclassificationTokenTracksMappingsButNotSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedReclassificationSession(t, d, "one", "archive.example", "/worktrees/service/one", "branch")
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service", OriginalProject: "branch", Enabled: true,
	}

	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(t, err)
	seedReclassificationSession(t, d, "two", "archive.example", "/worktrees/service/two", "branch")
	mapping, applied, err := d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(t, err)
	assert.Equal(t, "branch", mapping.OriginalProject)
	assert.Equal(t, 2, applied.UpdatedSessions,
		"sessions arriving after preview are included")

	stalePreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "other.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	})
	require.NoError(t, err)
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "other.example", PathPrefix: "/worktrees/other",
		Project: "other", Enabled: true,
	})
	require.NoError(t, err)
	_, _, err = d.ApplyWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "other.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	}, stalePreview.MappingToken, stalePreview.ExistingMappingID)
	require.ErrorIs(t, err, ErrWorktreeMappingSetChanged)
}

func TestWorktreeReclassificationExactCollisionIsServerResolved(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	existing, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "old-target", Enabled: true,
	})
	require.NoError(t, err)
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service/",
		Project: "new-target", OriginalProject: "branch", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(t, err)
	require.NotNil(t, preview.ExistingMappingID)
	assert.Equal(t, existing.ID, *preview.ExistingMappingID)

	unrelated := existing.ID + 1000
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, &unrelated,
	)
	require.ErrorIs(t, err, ErrWorktreeMappingSetChanged)
	updated, _, err := d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(t, err)
	assert.Equal(t, existing.ID, updated.ID)
	assert.Equal(t, "new_target", updated.Project)
	assert.Equal(t, "branch", updated.OriginalProject)
}

func TestWorktreeReclassificationExactCollisionPreservesPortableRootIdentity(
	t *testing.T,
) {
	tests := []struct {
		name            string
		storedPrefix    string
		draftPrefix     string
		wantCollisionID bool
	}{
		{
			name: "drive root alternate separators", storedPrefix: `C:\`,
			draftPrefix: `C:/`, wantCollisionID: true,
		},
		{
			name: "drive absolute and relative differ", storedPrefix: `C:\`,
			draftPrefix: `C:`,
		},
		{
			name: "UNC root alternate separators", storedPrefix: `\\server\share\`,
			draftPrefix: `//server/share/`, wantCollisionID: true,
		},
		{
			name: "UNC and POSIX roots differ", storedPrefix: `\\server\share\`,
			draftPrefix: `/server/share/`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			existing, err := d.CreateWorktreeProjectMapping(
				ctx,
				WorktreeProjectMapping{
					Machine: "portable.example", PathPrefix: tt.storedPrefix,
					Project: "old-target", Enabled: true,
				},
			)
			require.NoError(t, err)

			preview, err := d.PreviewWorktreeReclassification(
				ctx,
				WorktreeReclassificationDraft{
					Machine: "portable.example", PathPrefix: tt.draftPrefix,
					Project: "new-target", Enabled: true,
				},
			)
			require.NoError(t, err)
			if tt.wantCollisionID {
				require.NotNil(t, preview.ExistingMappingID)
				assert.Equal(t, existing.ID, *preview.ExistingMappingID)
			} else {
				assert.Nil(t, preview.ExistingMappingID)
			}
		})
	}
}

func TestWorktreeReclassificationApplyRollsBackEveryWriteStage(t *testing.T) {
	tests := []struct {
		name       string
		triggerSQL string
	}{
		{
			name: "mapping",
			triggerSQL: `CREATE TEMP TRIGGER fail_mapping_write
				BEFORE INSERT ON worktree_project_mappings
				BEGIN SELECT RAISE(ABORT, 'injected mapping write failure'); END`,
		},
		{
			name: "session",
			triggerSQL: `CREATE TEMP TRIGGER fail_session_write
				BEFORE UPDATE OF project ON sessions
				BEGIN SELECT RAISE(ABORT, 'injected session write failure'); END`,
		},
		{
			name: "identity",
			triggerSQL: `CREATE TEMP TRIGGER fail_identity_write
				BEFORE DELETE ON project_identity_observations
				BEGIN SELECT RAISE(ABORT, 'injected identity write failure'); END`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			ctx := context.Background()
			seedIdentityReclassificationSession(
				t, d, "one", "branch", "/worktrees/service/one",
			)
			draft := WorktreeReclassificationDraft{
				Machine: "archive.example", PathPrefix: "/worktrees/service",
				Project: "service", OriginalProject: "branch", Enabled: true,
			}
			preview, err := d.PreviewWorktreeReclassification(ctx, draft)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(ctx, tt.triggerSQL)
			require.NoError(t, err)

			_, _, err = d.ApplyWorktreeReclassification(
				ctx, draft, preview.MappingToken, preview.ExistingMappingID,
			)
			require.Error(t, err)
			mappings, listErr := d.ListWorktreeProjectMappings(ctx, "archive.example")
			require.NoError(t, listErr)
			assert.Empty(t, mappings, "mapping must roll back")
			session, getErr := d.GetSession(ctx, "one")
			require.NoError(t, getErr)
			require.NotNil(t, session)
			assert.Equal(t, "branch", session.Project, "session must roll back")
			observations, obsErr := d.ListProjectIdentityObservations(
				ctx, []string{"branch"},
			)
			require.NoError(t, obsErr)
			assert.Len(t, observations, 1, "identity aggregate must roll back")
		})
	}
}

func TestProjectIdentityReclassificationRebuildsAggregatesAndPreservesSnapshots(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedIdentityReclassificationSession(t, d, "gone", "old_gone", "/worktrees/service/gone")
	seedIdentityReclassificationSession(t, d, "move", "old_keep", "/worktrees/service/move")
	seedIdentityReclassificationSession(t, d, "stay", "old_keep", "/other/service/stay")
	_, err := d.getWriter().ExecContext(ctx, `
		UPDATE sessions SET local_modified_at = '2000-01-01T00:00:00Z'
		WHERE id IN ('gone', 'move')`)
	require.NoError(t, err)

	before, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	revision, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "new_service", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(t, err)
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(t, err)

	after, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after, "source snapshots must remain immutable")
	newObs, err := d.ListProjectIdentityObservations(ctx, []string{"new_service"})
	require.NoError(t, err)
	assert.Len(t, newObs, 2, "target receives evidence from both moved sessions")
	oldObs, err := d.ListProjectIdentityObservations(ctx, []string{"old_keep"})
	require.NoError(t, err)
	assert.Len(t, oldObs, 1, "supported former evidence remains")
	goneObs, err := d.ListProjectIdentityObservations(ctx, []string{"old_gone"})
	require.NoError(t, err)
	assert.Empty(t, goneObs, "unsupported former evidence is removed")
	afterRevision, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(t, err)
	delta, err := d.LoadProjectIdentityPublicationDelta(
		ctx, revision, afterRevision, []string{"old_gone"}, nil,
	)
	require.NoError(t, err)
	require.Len(t, delta.ObservationDeletes, 1)
	assert.Equal(t, "old_gone", delta.ObservationDeletes[0].Project)

	for _, id := range []string{"gone", "move"} {
		session, getErr := d.GetSession(ctx, id)
		require.NoError(t, getErr)
		assert.Equal(t, "new_service", session.Project)
		var localModifiedAt string
		require.NoError(t, d.getReader().QueryRowContext(ctx,
			`SELECT COALESCE(local_modified_at, '') FROM sessions WHERE id = ?`,
			id).Scan(&localModifiedAt))
		assert.NotEqual(t, "2000-01-01T00:00:00Z", localModifiedAt)
	}
}

func seedReclassificationSession(
	t *testing.T, d *DB, id, machine, cwd, project string,
) {
	t.Helper()
	require.NoError(t, d.UpsertSession(Session{
		ID: id, Machine: machine, Agent: "claude", Cwd: cwd, Project: project,
	}))
}

func seedIdentityReclassificationSession(
	t *testing.T, d *DB, id, project, cwd string,
) {
	t.Helper()
	seedReclassificationSession(t, d, id, "archive.example", cwd, project)
	require.NoError(t, d.UpsertProjectIdentityObservation(context.Background(),
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: "archive.example",
			RootPath: cwd, GitRemote: "https://example.com/org/repository.git",
		}), "seed identity evidence")
}
