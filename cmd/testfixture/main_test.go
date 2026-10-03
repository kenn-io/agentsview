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

func TestMixedContentFixtureStoresNativeOwners(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, createSessionFixture(t.Context(), database, specs[2], 2, base))
	messages, err := database.GetMessages(t.Context(), "test-session-mixed-content-7", 0, 0, true)
	require.NoError(t, err)
	require.Len(t, messages, 7)
	assert.Equal(t, "Here is my analysis.", messages[1].Content)
	assert.Equal(t, "Let me analyze...", messages[1].ThinkingText)
	require.NotNil(t, messages[1].ContentLayout)
	require.Len(t, messages[1].ContentLayout.Blocks, 2)
	assert.Equal(t, "thinking", messages[1].ContentLayout.Blocks[0].Kind)
	assert.Equal(t, "text", messages[1].ContentLayout.Blocks[1].Kind)
	for _, ordinal := range []int{3, 4} {
		assert.Empty(t, messages[ordinal].Content)
		require.Len(t, messages[ordinal].ToolCalls, 1)
		assert.NotEmpty(t, messages[ordinal].ToolCalls[0].Rendering)
	}
	assert.Equal(t, "Gemini-style reasoning", messages[5].ThinkingText)
	assert.Equal(t, "This is the visible response after thinking.", messages[5].Content)
}
