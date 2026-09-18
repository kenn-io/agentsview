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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	assignment, err := database.AssignSessionProject(ctx, "session-a", "target-project")
	require.NoError(err)
	assert.Equal("target_project", assignment.Project)

	result, err := database.ApplyWorktreeProjectMappings(ctx, "host-a.example")
	require.NoError(err)
	assert.Zero(result.MatchedSessions,
		"folder rules must not claim sessions with explicit assignments")

	insertSession(t, database, "session-a", "temp_project", func(session *Session) {
		session.Machine = "host-a.example"
		session.Cwd = "/tmp/agent-run"
	})
	stored, err := database.GetSession(ctx, "session-a")
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("target_project", stored.Project,
		"a parser upsert must preserve the explicit assignment")
	assert.True(stored.ProjectAssigned)
}

func TestClearSessionProjectAssignmentRestoresAutomaticFolderMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	first, err := database.AssignSessionProject(ctx, "session-a", "first-project")
	require.NoError(err)
	assert.Equal("temporary", first.OriginalProject)
	second, err := database.AssignSessionProject(ctx, "session-a", "second-project")
	require.NoError(err)
	assert.Equal("temporary", second.OriginalProject,
		"reassigning must preserve the initial automatic project")

	cleared, err := database.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(err)
	assert.Equal("folder_project", cleared.Project)
	stored, err := database.GetSession(ctx, "session-a")
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal("folder_project", stored.Project)
	assert.False(stored.ProjectAssigned)
}

func TestSessionProjectAssignmentMigrationBackfillsAutomaticProject(t *testing.T) {
	require := require.New(t)

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := Open(path)
	require.NoError(err)
	insertSession(t, database, "session-a", "automatic_project", func(session *Session) {
		session.Machine = "host-a.example"
	})
	_, err = database.AssignSessionProject(ctx, "session-a", "manual-project")
	require.NoError(err)
	require.NoError(database.Close())

	execRawSQLite(t, path,
		`ALTER TABLE session_project_assignments DROP COLUMN original_project`)
	database, err = Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = database.Close() })

	cleared, err := database.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(err)
	assert.Equal(t, "automatic_project", cleared.Project)
}

func TestAssignedSessionProvidesSiblingFolderEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	_, err = database.AssignSessionProject(
		ctx, "assigned-reference", "assigned-project",
	)
	require.NoError(err)

	result, err := database.ApplyWorktreeProjectMappings(ctx, "host-a.example")
	require.NoError(err)
	assert.Equal(1, result.MatchedSessions)
	assert.Equal(1, result.UpdatedSessions)
	assertSessionProject(t, database, "assigned-reference", "assigned_project")
	assertSessionProject(t, database, "empty-cwd-sibling", "folder_project")
}

func TestCopySessionMetadataFromPreservesSessionProjectAssignment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := t.Context()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(err)
	t.Cleanup(func() { _ = source.Close() })
	insertSession(t, source, "session-a", "temporary", func(session *Session) {
		session.Machine = "host-a.example"
	})
	require.NoError(source.UpsertProjectIdentityObservation(
		ctx, export.ProjectIdentityObservation{
			SessionID: "session-a", Project: "temporary", Machine: "host-a.example",
			RootPath: "/work/project", GitRemote: "https://example.com/repository.git",
			ObservedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		},
	))
	_, err = source.AssignSessionProject(ctx, "session-a", "target-project")
	require.NoError(err)

	destinationPath := filepath.Join(dir, "destination.db")
	destination, err := Open(destinationPath)
	require.NoError(err)
	t.Cleanup(func() { _ = destination.Close() })
	insertSession(t, destination, "session-a", "reparsed", func(session *Session) {
		session.Machine = "host-a.example"
	})
	require.NoError(destination.UpsertProjectIdentityObservation(
		ctx, export.ProjectIdentityObservation{
			SessionID: "session-a", Project: "reparsed", Machine: "host-a.example",
			RootPath: "/work/project", GitRemote: "https://example.com/repository.git",
			ObservedAt: time.Date(2026, 8, 25, 12, 5, 0, 0, time.UTC),
		},
	))

	require.NoError(destination.CopySessionMetadataFrom(sourcePath))
	assertSessionProject(t, destination, "session-a", "target_project")
	observations, err := destination.ListProjectIdentityObservations(
		ctx, []string{"reparsed", "target_project"},
	)
	require.NoError(err)
	require.Len(observations, 1)
	assert.Equal("target_project", observations[0].Project)

	insertSession(t, destination, "session-a", "reparsed")
	assertSessionProject(t, destination, "session-a", "target_project")
	cleared, err := destination.ClearSessionProjectAssignment(ctx, "session-a")
	require.NoError(err)
	assert.Equal("temporary", cleared.Project)
}
