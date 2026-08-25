package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

func TestCopySessionMetadataFromPreservesSessionProjectAssignment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	source, err := Open(sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	insertSession(t, source, "session-a", "temporary")
	_, err = source.AssignSessionProject(ctx, "session-a", "target-project")
	require.NoError(t, err)

	destinationPath := filepath.Join(dir, "destination.db")
	destination, err := Open(destinationPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = destination.Close() })
	insertSession(t, destination, "session-a", "reparsed")

	require.NoError(t, destination.CopySessionMetadataFrom(sourcePath))
	assertSessionProject(t, destination, "session-a", "target_project")

	insertSession(t, destination, "session-a", "reparsed")
	assertSessionProject(t, destination, "session-a", "target_project")
}
