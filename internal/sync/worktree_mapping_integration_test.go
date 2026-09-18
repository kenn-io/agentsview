package sync

import (
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyWorktreeMappingToSingleSessionUsesSameFileSiblingForEmptyCwd(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(database.Close()) })
	ctx := t.Context()
	filePath := filepath.Join(t.TempDir(), "shared-session.jsonl")
	worktreePrefix := "/srv/worktrees/service"

	require.NoError(database.UpsertSession(db.Session{
		ID: "target", Machine: "archive.example", Agent: "claude",
		Project: "branch", Cwd: "", FilePath: &filePath,
	}))
	require.NoError(database.UpsertSession(db.Session{
		ID: "sibling", Machine: "archive.example", Agent: "claude",
		Project: "branch", Cwd: worktreePrefix + "/feature", FilePath: &filePath,
	}))
	_, err = database.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: worktreePrefix,
		Project: "service", Enabled: true,
	})
	require.NoError(err)

	engine := NewEngine(database, EngineConfig{Machine: "archive.example"})
	finalProject, err := engine.applyWorktreeMappingToSingleSession("target")
	require.NoError(err)
	assert.Equal("service", finalProject)

	target, err := database.GetSession(ctx, "target")
	require.NoError(err)
	require.NotNil(target)
	assert.Equal("service", target.Project)
	sibling, err := database.GetSession(ctx, "sibling")
	require.NoError(err)
	require.NotNil(sibling)
	assert.Equal("branch", sibling.Project,
		"same-file sibling supplies evidence but remains outside session scope")
}
