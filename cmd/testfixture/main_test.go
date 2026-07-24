package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestCreateProjectReclassificationFixture(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
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
		session, getErr := database.GetSession(context.Background(), sessionID)
		require.NoError(t, getErr)
		require.NotNil(t, session)
		assert.Equal(t, machine, session.Machine)
		assert.Equal(t, wrongProject, session.Project)
		assert.Equal(t, wantCwd, session.Cwd)
	}

	snapshots, err := database.ListSessionProjectIdentitySnapshots(
		context.Background(),
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
