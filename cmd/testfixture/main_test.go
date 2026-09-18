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
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })

	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(createProjectReclassificationFixture(database, base))

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
		require.NoError(getErr)
		require.NotNil(session)
		assert.Equal(machine, session.Machine)
		assert.Equal(wrongProject, session.Project)
		assert.Equal(wantCwd, session.Cwd)
	}

	snapshots, err := database.ListSessionProjectIdentitySnapshots(
		t.Context(),
	)
	require.NoError(err)
	require.Len(snapshots, 2)
	for _, snapshot := range snapshots {
		assert.Equal(machine, snapshot.Machine)
		assert.Equal(wrongProject, snapshot.Project)
		assert.Equal(worktreeRoot, snapshot.RootPath)
		assert.Equal(worktreeRoot, snapshot.WorktreeRootPath)
		assert.NotEmpty(snapshot.Key)
	}
}
