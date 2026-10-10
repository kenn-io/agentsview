package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestAssignSessionProjectOverridesSyncAndFolderRules(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()

	insertSession(t, database, "session-a", "temp_project", func(session *Session) {
		session.Machine = "host-a.example"
		session.Cwd = "/tmp/agent-run"
	})
	_, err := database.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "host-a.example", PathPrefix: "/tmp",
		Project: "folder_project", Enabled: true,
	})
	require.NoError(t, err)

	assignment, err := database.AssignSessionProject(ctx, "session-a", "target-project")
	require.NoError(t, err)
	assert.Equal(t, "target_project", assignment.Project)

	result, err := database.ApplyWorktreeProjectMappings(ctx, "host-a.example")
	require.NoError(t, err)
	assert.Zero(t, result.MatchedSessions,
		"folder rules must not claim sessions with explicit assignments")

	insertSession(t, database, "session-a", "temp_project", func(session *Session) {
		session.Machine = "host-a.example"
		session.Cwd = "/tmp/agent-run"
	})
	stored, err := database.GetSession(ctx, "session-a")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "target_project", stored.Project,
		"a parser upsert must preserve the explicit assignment")
	assert.True(t, stored.ProjectAssigned)
}

func TestClearSessionProjectAssignmentRestoresAutomaticFolderMapping(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()

	insertSession(t, database, "session-a", "temporary", func(session *Session) {
		session.Machine = "host-a.example"
		session.Cwd = "/work/project/run"
	})
	_, err := database.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "host-a.example", PathPrefix: "/work/project",
		Project: "folder-project", Enabled: true,
	})
	require.NoError(t, err)

	first, err := database.AssignSessionProject(ctx, "session-a", "first-project")
	require.NoError(t, err)
	assert.Equal(t, "temporary", first.OriginalProject)
	second, err := database.AssignSessionProject(ctx, "session-a", "second-project")
	require.NoError(t, err)
	assert.Equal(t, "temporary", second.OriginalProject,
		"reassigning must preserve the initial automatic project")

	cleared, err := database.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(t, err)
	assert.Equal(t, "folder_project", cleared.Project)
	stored, err := database.GetSession(ctx, "session-a")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "folder_project", stored.Project)
	assert.False(t, stored.ProjectAssigned)
}

func TestSessionProjectAssignmentMigrationBackfillsAutomaticProject(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := Open(ctx, path)
	require.NoError(t, err)
	insertSession(t, database, "session-a", "automatic_project", func(session *Session) {
		session.Machine = "host-a.example"
	})
	_, err = database.AssignSessionProject(ctx, "session-a", "manual-project")
	require.NoError(t, err)
	require.NoError(t, database.Close())

	execRawSQLite(t, path,
		`ALTER TABLE session_project_assignments DROP COLUMN original_project`)
	database, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	cleared, err := database.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(t, err)
	assert.Equal(t, "automatic_project", cleared.Project)
}

func TestAssignedSessionProvidesSiblingFolderEvidence(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	sharedPath := filepath.Join(t.TempDir(), "sessions.jsonl")

	insertSession(t, database, "assigned-reference", "temporary", func(session *Session) {
		session.Machine = "host-a.example"
		session.Cwd = "/work/project/run"
		session.FilePath = &sharedPath
	})
	insertSession(t, database, "empty-cwd-sibling", "temporary", func(session *Session) {
		session.Machine = "host-a.example"
		session.FilePath = &sharedPath
	})
	_, err := database.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "host-a.example", PathPrefix: "/work/project",
		Project: "folder-project", Enabled: true,
	})
	require.NoError(t, err)
	_, err = database.AssignSessionProject(
		ctx, "assigned-reference", "assigned-project",
	)
	require.NoError(t, err)

	result, err := database.ApplyWorktreeProjectMappings(ctx, "host-a.example")
	require.NoError(t, err)
	assert.Equal(t, 1, result.MatchedSessions)
	assert.Equal(t, 1, result.UpdatedSessions)
	assertSessionProject(t, database, "assigned-reference", "assigned_project")
	assertSessionProject(t, database, "empty-cwd-sibling", "folder_project")
}

func TestCopySessionMetadataFromPreservesSessionProjectAssignment(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(ctx, sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	insertSession(t, source, "session-a", "temporary", func(session *Session) {
		session.Machine = "host-a.example"
	})
	require.NoError(t, source.UpsertProjectIdentityObservation(
		ctx, export.ProjectIdentityObservation{
			SessionID: "session-a", Project: "temporary", Machine: "host-a.example",
			RootPath: "/work/project", GitRemote: "https://example.com/repository.git",
			ObservedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		},
	))
	_, err = source.AssignSessionProject(ctx, "session-a", "target-project")
	require.NoError(t, err)

	destinationPath := filepath.Join(dir, "destination.db")
	destination, err := Open(ctx, destinationPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = destination.Close() })
	insertSession(t, destination, "session-a", "reparsed", func(session *Session) {
		session.Machine = "host-a.example"
	})
	require.NoError(t, destination.UpsertProjectIdentityObservation(
		ctx, export.ProjectIdentityObservation{
			SessionID: "session-a", Project: "reparsed", Machine: "host-a.example",
			RootPath: "/work/project", GitRemote: "https://example.com/repository.git",
			ObservedAt: time.Date(2026, 8, 25, 12, 5, 0, 0, time.UTC),
		},
	))

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))
	assertSessionProject(t, destination, "session-a", "target_project")
	observations, err := destination.ListProjectIdentityObservations(
		ctx, []string{"reparsed", "target_project"},
	)
	require.NoError(t, err)
	require.Len(t, observations, 1)
	assert.Equal(t, "target_project", observations[0].Project)

	insertSession(t, destination, "session-a", "reparsed")
	assertSessionProject(t, destination, "session-a", "target_project")
	cleared, err := destination.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(t, err)
	assert.Equal(t, "temporary", cleared.Project)
}

func TestCodexPageUpgradePreservesProjectAssignment(t *testing.T) {
	const thread = "codex:11111111-1111-4111-8111-111111111111"
	const page = thread + "_22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name       string
		reparsed   bool
		headExists bool
		trashed    bool
	}{
		{name: "reparsed_page", reparsed: true},
		{name: "archived_page"},
		{name: "trashed_page", trashed: true},
		{name: "surviving_original", reparsed: true, headExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "old.db")
			source := testDBAtPath(t, sourcePath, "source")
			t.Cleanup(func() { _ = source.Close() })
			pagePath := filepath.Join(dir, "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl")
			withSource := func(session *Session) {
				session.Agent, session.Machine = "codex", "host-a.example"
				session.FilePath, session.Cwd = &pagePath, "/work/project/run"
			}
			insertSession(t, source, thread, "automatic_project", withSource)
			observation := export.ProjectIdentityObservation{
				SessionID: thread, Project: "automatic_project", Machine: "host-a.example",
				RootPath: "/work/project", GitRemote: "https://example.com/repository.git",
				ObservedAt: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC),
			}
			require.NoError(t, source.UpsertProjectIdentityObservation(ctx, observation))
			_, err := source.AssignSessionProject(ctx, thread, "saved_project")
			require.NoError(t, err)
			const created = "2026-09-25T12:00:00.000Z"
			const updated = "2026-09-26T12:00:00.000Z"
			_, err = source.getWriter().Exec(ctx, `UPDATE session_project_assignments SET created_at=?, updated_at=? WHERE session_id=?`, created, updated, thread)
			require.NoError(t, err)
			if tc.trashed {
				require.NoError(t, source.SoftDeleteSession(ctx, thread))
			}
			_, err = source.getWriter().Exec(ctx, "PRAGMA user_version=126")
			require.NoError(t, err)
			require.NoError(t, source.Close())

			destination := testDB(t)
			require.NoError(t, destination.CopyArchiveIdentityFrom(sourcePath))
			_, err = destination.CopyTrashedDataFrom(sourcePath)
			require.NoError(t, err)
			if tc.headExists {
				insertSession(t, destination, thread, "head_project", func(session *Session) {
					session.Agent = "codex"
					session.FilePath = new(filepath.Join(dir, "rollout-2026-09-25T11-00-00-"+thread[len("codex:"):]+".jsonl"))
				})
			}
			if tc.reparsed {
				insertSession(t, destination, page, "reparsed_project", withSource)
				observation.SessionID, observation.Project = page, "reparsed_project"
				require.NoError(t, destination.UpsertProjectIdentityObservation(ctx, observation))
			}
			_, err = destination.CopyOrphanedDataFromExcluding(sourcePath, nil)
			require.NoError(t, err)
			require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))
			assignedID := page
			if tc.headExists {
				assignedID = thread
			}
			var assignment SessionProjectAssignment
			require.NoError(t, destination.getReader().QueryRowContext(ctx, `
				SELECT session_id, project, original_project, created_at, updated_at
				FROM session_project_assignments WHERE session_id=?`, assignedID).Scan(
				&assignment.SessionID, &assignment.Project, &assignment.OriginalProject,
				&assignment.CreatedAt, &assignment.UpdatedAt,
			))
			assert.Equal(t, SessionProjectAssignment{
				SessionID: assignedID, Project: "saved_project", OriginalProject: "automatic_project",
				CreatedAt: created, UpdatedAt: updated,
			}, assignment)
			stored, err := destination.GetSessionFull(ctx, assignedID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, "saved_project", stored.Project)
			assert.True(t, stored.ProjectAssigned)
			if tc.headExists {
				storedPage, err := destination.GetSession(ctx, page)
				require.NoError(t, err)
				require.NotNil(t, storedPage)
				assert.False(t, storedPage.ProjectAssigned, "a surviving original keeps its own assignment")
				return
			}
			head, err := destination.GetSessionFull(ctx, thread)
			require.NoError(t, err)
			if tc.trashed {
				require.NotNil(t, head)
				assert.Nil(t, head.FilePath, "the surviving thread ID is only an empty trash anchor")
				assert.False(t, head.ProjectAssigned)
			} else {
				assert.Nil(t, head)
			}
			observations, err := destination.ListProjectIdentityObservations(ctx,
				[]string{"automatic_project", "reparsed_project", "saved_project"})
			require.NoError(t, err)
			if tc.trashed {
				assert.Empty(t, observations, "a trashed page must not contribute active project evidence")
				restored, err := destination.RestoreSession(ctx, page)
				require.NoError(t, err)
				require.EqualValues(t, 1, restored)
			} else {
				require.Len(t, observations, 1, "the obsolete automatic project must lose the page's evidence")
				assert.Equal(t, "saved_project", observations[0].Project)
			}

			// Resync restores source identities after copying metadata, then later
			// parsing and folder rules must still honor the explicit assignment.
			_, err = destination.RestoreSessionProjectsFromIdentitySnapshots(ctx)
			require.NoError(t, err)
			assertSessionProject(t, destination, page, "saved_project")
			insertSession(t, destination, page, "reparsed_project", withSource)
			_, err = destination.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
				Machine: "host-a.example", PathPrefix: "/work/project",
				Project: "folder_project", Enabled: true,
			})
			require.NoError(t, err)
			applied, err := destination.ApplyWorktreeProjectMappings(ctx, "host-a.example")
			require.NoError(t, err)
			assert.Zero(t, applied.MatchedSessions)
			assertSessionProject(t, destination, page, "saved_project")
		})
	}
}
