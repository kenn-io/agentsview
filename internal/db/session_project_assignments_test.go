package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestAssignSessionProjectOverridesSyncAndFolderRules(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

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

func TestAssignedSessionProvidesSiblingFolderEvidence(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
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
	ctx := context.Background()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
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
	destination, err := Open(destinationPath)
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
}
