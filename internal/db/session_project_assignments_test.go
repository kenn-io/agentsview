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
	t.Run("cron automatic project refresh", func(t *testing.T) {
		for _, tc := range []struct{ agent, original, restored string }{
			{"hermes", "hermes-cron", "hermes-cron/job-b"},
			{"augure-desktop", "augure-desktop-cron", "augure-desktop-cron/job-b"},
			{"codex", "codex-cron/job-a", "codex-cron/job-a"},
		} {
			agent := tc.agent
			database := testDB(t)
			id := agent + ":tip"
			insertSession(t, database, id, tc.original, func(s *Session) { s.Agent = agent })
			assignment, err := database.AssignSessionProject(t.Context(), id, "manual")
			require.NoError(t, err)
			insertSession(t, database, id, agent+"-cron/job-a", func(s *Session) { s.Agent = agent })
			require.NoError(t, database.UpsertProjectIdentityObservationWithSnapshotProject(t.Context(), export.ProjectIdentityObservation{
				SessionID: id, Project: assignment.Project, Machine: defaultMachine,
			}, agent+"-cron/job-a"))
			insertSession(t, database, id, agent+"-cron/job-b", func(s *Session) { s.Agent = agent })
			stored, err := database.GetSession(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, assignment.Project, stored.Project)
			cleared, err := database.ClearSessionProjectAssignment(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, tc.restored, cleared.Project)
		}
	})
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
	for _, agent := range []string{"hermes", "augure-desktop"} {
		id := agent + ":tip"
		insertSession(t, source, id, agent+"-cron/job-a", func(s *Session) { s.Agent = agent })
		_, err := source.AssignSessionProject(ctx, id, "manual")
		require.NoError(t, err)
		insertSession(t, destination, id, agent+"-cron/job-b", func(s *Session) { s.Agent = agent })
		insertSession(t, source, agent+":unassigned", agent+"-cron", func(s *Session) { s.Agent = agent })
		insertSession(t, destination, agent+":unassigned", agent+"-cron/job-b", func(s *Session) { s.Agent = agent })
		insertSession(t, source, agent+":unchanged", agent+"-cron/job-b", func(s *Session) { s.Agent = agent })
		require.NoError(t, source.UpsertProjectIdentityObservationWithSnapshotProject(ctx, export.ProjectIdentityObservation{
			SessionID: agent + ":unchanged", Project: agent + "-cron/job-b", Machine: defaultMachine,
		}, agent+"-cron"))
		insertSession(t, destination, agent+":unchanged", agent+"-cron/job-b", func(s *Session) { s.Agent = agent })
	}

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))
	snapshots, err := destination.ListSessionProjectIdentitySnapshotsByID(ctx, []string{"hermes:tip", "augure-desktop:tip", "hermes:unassigned", "augure-desktop:unassigned", "hermes:unchanged", "augure-desktop:unchanged"})
	require.NoError(t, err)
	for _, agent := range []string{"hermes", "augure-desktop"} {
		id := agent + ":tip"
		assertSessionProject(t, destination, id, "manual")
		cleared, err := destination.ClearSessionProjectAssignment(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, agent+"-cron/job-b", cleared.Project)
		assert.Equal(t, agent+"-cron/job-b", snapshots[id].Project)
		assertSessionProject(t, destination, agent+":unassigned", agent+"-cron/job-b")
		assert.Equal(t, agent+"-cron/job-b", snapshots[agent+":unassigned"].Project)
		assertSessionProject(t, destination, agent+":unchanged", agent+"-cron/job-b")
		assert.Equal(t, agent+"-cron/job-b", snapshots[agent+":unchanged"].Project)
	}
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
