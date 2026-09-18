package db

import (
	"fmt"
	"testing"

	"go.kenn.io/agentsview/internal/export"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeReclassificationPreviewUsesBoundaryAndPortablePaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	seedReclassificationSession(t, d, "unix-root", "archive.example", "/worktrees/service", "branch")
	seedReclassificationSession(t, d, "unix-child", "archive.example", "/worktrees/service/cmd", "branch")
	seedReclassificationSession(t, d, "unix-neighbor", "archive.example", "/worktrees/service-old", "neighbor")
	seedReclassificationSession(t, d, "windows-child", "windows.example", `C:\worktrees\service\subdir`, "branch")

	unixPreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service-name", Enabled: true,
	})
	require.NoError(err)
	assert.Equal(2, unixPreview.MatchedSessions)
	assert.Equal(2, unixPreview.UpdatedSessions)
	assert.Equal(1, unixPreview.DistinctProjects)
	assert.Equal("service_name", unixPreview.NormalizedProject)

	windowsPreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "windows.example", PathPrefix: `C:\worktrees\service`,
		Project: "service", Enabled: true,
	})
	require.NoError(err)
	assert.Equal(1, windowsPreview.MatchedSessions)
	assert.Equal(1, windowsPreview.UpdatedSessions)
}

func TestWorktreeReclassificationPreviewCountsAlreadyTargetSessionsByProject(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	seedReclassificationSession(
		t, d, "already-target", "archive.example",
		"/worktrees/service/main", "service",
	)
	seedReclassificationSession(
		t, d, "changes-project", "archive.example",
		"/worktrees/service/branch", "branch",
	)

	preview, err := d.PreviewWorktreeReclassification(
		ctx,
		WorktreeReclassificationDraft{
			Machine: "archive.example", PathPrefix: "/worktrees/service",
			Project: "service", Enabled: true,
		},
	)
	require.NoError(err)

	assert.Equal(2, preview.MatchedSessions)
	assert.Equal(1, preview.UpdatedSessions)
	assert.ElementsMatch([]string{"already-target", "changes-project"}, preview.MatchedSessionIDs)
	assert.Equal([]string{"changes-project"}, preview.UpdatedSessionIDs)
	assert.Equal(2, preview.DistinctProjects)
	assert.Equal([]WorktreeReclassificationProjectSample{
		{Project: "branch", Count: 1},
		{Project: "service", Count: 1},
	}, preview.ProjectSamples)
	require.Len(preview.SessionSamples, 1)
	assert.Equal("changes-project", preview.SessionSamples[0].ID)
}

func TestWorktreeReclassificationPreviewHonorsSpecificRuleAndBoundsSamples(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	for i := range 14 {
		seedReclassificationSession(t, d, fmt.Sprintf("session-%02d", i),
			"archive.example", fmt.Sprintf("/worktrees/service/branch-%02d", i),
			fmt.Sprintf("branch_%02d", i))
	}
	_, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: "/worktrees/service/branch-09",
		Project: "specific", Enabled: true,
	})
	require.NoError(err)

	preview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	})
	require.NoError(err)
	assert.Equal(14, preview.MatchedSessions)
	assert.Equal(14, preview.UpdatedSessions)
	assert.Equal(14, preview.DistinctProjects)
	assert.Equal([]string{
		"branch_00", "branch_01", "branch_02", "branch_03", "branch_04",
		"branch_05", "branch_06", "branch_07", "branch_08", "branch_09",
		"branch_10", "branch_11", "branch_12", "branch_13",
	}, preview.MatchedProjects)
	assert.Len(preview.ProjectSamples, 10)
	assert.Len(preview.SessionSamples, 10)
	assert.Equal("branch_00", preview.ProjectSamples[0].Project)
	assert.Equal("session-00", preview.SessionSamples[0].ID)
	assert.Equal("specific", preview.SessionSamples[9].NextProject,
		"the specific mapping must remain authoritative")
}

func TestWorktreeReclassificationTokenBindsDraftAndAffectedSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	seedReclassificationSession(t, d, "one", "archive.example", "/worktrees/service/one", "branch")
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "service", OriginalProject: "branch", Enabled: true,
	}

	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	changedDraft := draft
	changedDraft.Project = "different-service"
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, changedDraft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.ErrorIs(err, ErrWorktreeMappingSetChanged,
		"a preview for one normalized draft must not authorize another")

	seedReclassificationSession(t, d, "two", "archive.example", "/worktrees/service/two", "branch")
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.ErrorIs(err, ErrWorktreeMappingSetChanged,
		"a newly affected session must invalidate the accepted preview")

	current, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	mapping, applied, err := d.ApplyWorktreeReclassification(
		ctx, draft, current.MappingToken, current.ExistingMappingID,
	)
	require.NoError(err)
	assert.Equal("branch", mapping.OriginalProject)
	assert.Equal(2, applied.UpdatedSessions)
	afterSave, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	assert.NotEqual(current.MappingSetToken, applied.MappingSetToken)
	assert.Equal(afterSave.MappingSetToken, applied.MappingSetToken,
		"batch continuation must identify precisely the rules committed by this save")

	stalePreview, err := d.PreviewWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "other.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	})
	require.NoError(err)
	_, err = d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "other.example", PathPrefix: "/worktrees/other",
		Project: "other", Enabled: true,
	})
	require.NoError(err)
	_, _, err = d.ApplyWorktreeReclassification(ctx, WorktreeReclassificationDraft{
		Machine: "other.example", PathPrefix: "/worktrees/service",
		Project: "service", Enabled: true,
	}, stalePreview.MappingToken, stalePreview.ExistingMappingID)
	require.ErrorIs(err, ErrWorktreeMappingSetChanged)
}

func TestWorktreeReclassificationExactCollisionIsServerResolved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	existing, err := d.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "old-target", Enabled: true,
	})
	require.NoError(err)
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service/",
		Project: "new-target", OriginalProject: "branch", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	require.NotNil(preview.ExistingMappingID)
	assert.Equal(existing.ID, *preview.ExistingMappingID)

	unrelated := existing.ID + 1000
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, &unrelated,
	)
	require.ErrorIs(err, ErrWorktreeMappingSetChanged)
	updated, _, err := d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(err)
	assert.Equal(existing.ID, updated.ID)
	assert.Equal("new_target", updated.Project)
	assert.Equal("branch", updated.OriginalProject)
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
			assert := assert.New(t)
			require := require.New(t)

			d := testDB(t)
			ctx := t.Context()
			existing, err := d.CreateWorktreeProjectMapping(
				ctx,
				WorktreeProjectMapping{
					Machine: "portable.example", PathPrefix: tt.storedPrefix,
					Project: "old-target", Enabled: true,
				},
			)
			require.NoError(err)

			preview, err := d.PreviewWorktreeReclassification(
				ctx,
				WorktreeReclassificationDraft{
					Machine: "portable.example", PathPrefix: tt.draftPrefix,
					Project: "new-target", Enabled: true,
				},
			)
			require.NoError(err)
			if tt.wantCollisionID {
				require.NotNil(preview.ExistingMappingID)
				assert.Equal(existing.ID, *preview.ExistingMappingID)
			} else {
				assert.Nil(preview.ExistingMappingID)
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
			assert := assert.New(t)
			require := require.New(t)

			d := testDB(t)
			ctx := t.Context()
			seedIdentityReclassificationSession(
				t, d, "one", "branch", "/worktrees/service/one",
			)
			draft := WorktreeReclassificationDraft{
				Machine: "archive.example", PathPrefix: "/worktrees/service",
				Project: "service", OriginalProject: "branch", Enabled: true,
			}
			preview, err := d.PreviewWorktreeReclassification(ctx, draft)
			require.NoError(err)
			_, err = d.getWriter().ExecContext(ctx, tt.triggerSQL)
			require.NoError(err)

			_, _, err = d.ApplyWorktreeReclassification(
				ctx, draft, preview.MappingToken, preview.ExistingMappingID,
			)
			require.Error(err)
			mappings, listErr := d.ListWorktreeProjectMappings(ctx, "archive.example")
			require.NoError(listErr)
			assert.Empty(mappings, "mapping must roll back")
			session, getErr := d.GetSession(ctx, "one")
			require.NoError(getErr)
			require.NotNil(session)
			assert.Equal("branch", session.Project, "session must roll back")
			observations, obsErr := d.ListProjectIdentityObservations(
				ctx, []string{"branch"},
			)
			require.NoError(obsErr)
			assert.Len(observations, 1, "identity aggregate must roll back")
		})
	}
}

func TestProjectIdentityReclassificationReconcilesAggregatesAndPreservesSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	seedIdentityReclassificationSession(t, d, "gone", "old_gone", "/worktrees/service/gone")
	seedIdentityReclassificationSession(t, d, "move", "old_keep", "/worktrees/service/move")
	seedIdentityReclassificationSession(t, d, "stay", "old_keep", "/other/service/stay")
	_, err := d.getWriter().ExecContext(ctx, `
		UPDATE sessions SET local_modified_at = '2000-01-01T00:00:00Z'
		WHERE id IN ('gone', 'move')`)
	require.NoError(err)

	before, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(err)
	revision, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(err)
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "new_service", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(err)

	after, err := d.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(err)
	assert.Equal(before, after, "source snapshots must remain immutable")
	newObs, err := d.ListProjectIdentityObservations(ctx, []string{"new_service"})
	require.NoError(err)
	assert.Len(newObs, 2, "target receives evidence from both moved sessions")
	oldObs, err := d.ListProjectIdentityObservations(ctx, []string{"old_keep"})
	require.NoError(err)
	assert.Len(oldObs, 1, "supported former evidence remains")
	goneObs, err := d.ListProjectIdentityObservations(ctx, []string{"old_gone"})
	require.NoError(err)
	assert.Empty(goneObs, "unsupported former evidence is removed")
	afterRevision, err := d.ProjectIdentityPublicationRevision(ctx)
	require.NoError(err)
	delta, err := d.LoadProjectIdentityPublicationDelta(
		ctx, revision, afterRevision, []string{"old_gone"}, nil,
	)
	require.NoError(err)
	require.Len(delta.ObservationDeletes, 1)
	assert.Equal("old_gone", delta.ObservationDeletes[0].Project)

	for _, id := range []string{"gone", "move"} {
		session, getErr := d.GetSession(ctx, id)
		require.NoError(getErr)
		assert.Equal("new_service", session.Project)
		var localModifiedAt string
		require.NoError(d.getReader().QueryRowContext(ctx,
			`SELECT COALESCE(local_modified_at, '') FROM sessions WHERE id = ?`,
			id).Scan(&localModifiedAt))
		assert.NotEqual("2000-01-01T00:00:00Z", localModifiedAt)
	}
}

func TestProjectIdentityReclassificationPreservesSourceMissingEvidence(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()
	const (
		legacyProject = "legacy_project"
		legacyRoot    = "/archives/legacy/repository"
		legacyRemote  = "https://example.com/legacy/repository.git"
	)
	seedIdentityReclassificationSession(
		t, d, "move", legacyProject, "/worktrees/service/move",
	)
	seedReclassificationSession(
		t, d, "legacy-missing", "archive.example", legacyRoot, legacyProject,
	)
	require.NoError(d.UpsertProjectIdentityObservation(
		ctx,
		export.ProjectIdentityObservation{
			SessionID: "legacy-missing", Project: legacyProject,
			Machine: "archive.example", RootPath: legacyRoot,
			GitRemote:        legacyRemote,
			RemoteResolution: export.ProjectResolutionResolved,
		},
	))
	require.NoError(d.SetSessionDataVersion("legacy-missing", 75))
	_, err := d.getWriter().ExecContext(ctx, `
		DELETE FROM session_project_identity_snapshots
		WHERE session_id = 'legacy-missing'`)
	require.NoError(err)
	_, err = d.getWriter().ExecContext(ctx, `
		UPDATE sessions
		SET source_missing_at = '2026-07-30T12:00:00Z'
		WHERE id = 'legacy-missing'`)
	require.NoError(err)

	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "current_project", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	_, _, err = d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(err)

	observations, err := d.ListProjectIdentityObservations(
		ctx, []string{legacyProject},
	)
	require.NoError(err)
	require.Len(observations, 1)
	assert.Equal(legacyRoot, observations[0].RootPath)
	assert.Equal(legacyRemote, observations[0].GitRemote)
}

func TestWorktreeReclassificationSucceedsAcrossProjectsAboveVariableLimit(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	const sessionCount = 20
	for i := range sessionCount {
		seedReclassificationSession(
			t, d, fmt.Sprintf("session-%02d", i), "archive.example",
			fmt.Sprintf("/worktrees/service/%02d", i),
			fmt.Sprintf("project-%02d", i),
		)
	}
	draft := WorktreeReclassificationDraft{
		Machine: "archive.example", PathPrefix: "/worktrees/service",
		Project: "combined_project", Enabled: true,
	}
	preview, err := d.PreviewWorktreeReclassification(ctx, draft)
	require.NoError(err)
	require.Equal(sessionCount, preview.DistinctProjects)
	conn, err := d.getWriter().Conn(ctx)
	require.NoError(err)
	require.NoError(conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite driver connection %T", driverConn)
		}
		sqliteConn.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 16)
		return nil
	}))
	require.NoError(conn.Close())
	_, applied, err := d.ApplyWorktreeReclassification(
		ctx, draft, preview.MappingToken, preview.ExistingMappingID,
	)
	require.NoError(err)
	assert.Equal(sessionCount, applied.UpdatedSessions)

	var moved int
	require.NoError(d.getReader().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions WHERE project = 'combined_project'`,
	).Scan(&moved))
	assert.Equal(sessionCount, moved)
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
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(),
		export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: "archive.example",
			RootPath: cwd, GitRemote: "https://example.com/org/repository.git",
		}), "seed identity evidence")
}
