package sync

import (
	"context"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyWorktreeMappingToSingleSessionUsesSameFileSiblingForEmptyCwd(
	t *testing.T,
) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	ctx := context.Background()
	filePath := filepath.Join(t.TempDir(), "shared-session.jsonl")
	worktreePrefix := "/srv/worktrees/service"

	require.NoError(t, database.UpsertSession(db.Session{
		ID: "target", Machine: "archive.example", Agent: "claude",
		Project: "branch", Cwd: "", FilePath: &filePath,
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "sibling", Machine: "archive.example", Agent: "claude",
		Project: "branch", Cwd: worktreePrefix + "/feature", FilePath: &filePath,
	}))
	_, err = database.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: "archive.example", PathPrefix: worktreePrefix,
		Project: "service", Enabled: true,
	})
	require.NoError(t, err)

	engine := NewEngine(database, EngineConfig{Machine: "archive.example"})
	require.NoError(t, engine.applyWorktreeMappingToSingleSession("target"))

	target, err := database.GetSession(ctx, "target")
	require.NoError(t, err)
	require.NotNil(t, target)
	assert.Equal(t, "service", target.Project)
	sibling, err := database.GetSession(ctx, "sibling")
	require.NoError(t, err)
	require.NotNil(t, sibling)
	assert.Equal(t, "branch", sibling.Project,
		"same-file sibling supplies evidence but remains outside session scope")
}
