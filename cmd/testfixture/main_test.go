package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestCreateProjectReclassificationFixture(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, createProjectReclassificationFixture(database, base))

	const (
		machine      = "remote-example-host"
		wrongProject = "wrong_branch_label"
		worktreeRoot = "/srv/worktrees/github.com/example-org/sample-service/example-worktree"
	)
	wantCwds := map[string]string{
		"test-session-project-reclassification-root":   worktreeRoot,
		"test-session-project-reclassification-nested": worktreeRoot + "/cmd/server",
	}
	for sessionID, wantCwd := range wantCwds {
		session, getErr := database.GetSession(t.Context(), sessionID)
		require.NoError(t, getErr)
		require.NotNil(t, session)
		assert.Equal(t, machine, session.Machine)
		assert.Equal(t, wrongProject, session.Project)
		assert.Equal(t, wantCwd, session.Cwd)
	}

	snapshots, err := database.ListSessionProjectIdentitySnapshots(
		t.Context(),
	)
	require.NoError(t, err)
	require.Len(t, snapshots, 2)
	for _, snapshot := range snapshots {
		assert.Equal(t, machine, snapshot.Machine)
		assert.Equal(t, wrongProject, snapshot.Project)
		assert.Equal(t, worktreeRoot, snapshot.RootPath)
		assert.Equal(t, worktreeRoot, snapshot.WorktreeRootPath)
		assert.NotEmpty(t, snapshot.Key)
	}
}

func TestCreateToolSequencesFixture(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, createToolSequencesFixture(t.Context(), database, start))
	session, err := database.GetSession(t.Context(), "test-session-tool-sequences")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 54, session.MessageCount)
	assert.Equal(t, 26, session.UserMessageCount)
	assert.Equal(t, "clean", *session.TerminationStatus)

	messages, err := database.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	require.Len(t, messages, 54)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "grep-1", messages[1].ToolCalls[0].ToolUseID)
	require.Len(t, messages[3].ToolCalls, 1)
	assert.Equal(t, "read-1", messages[3].ToolCalls[0].ToolUseID)
	assert.Equal(t, 53, messages[53].Ordinal)
}
