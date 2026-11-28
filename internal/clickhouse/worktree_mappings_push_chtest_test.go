//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/worktree"
)

func TestPushPublishesConfiguredWorktreeProjectNotRule(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	url, database := chtest.FreshDatabase(t)
	target := Target{URL: url, Database: database}
	local.SetConfiguredWorktreeMappings(fixtureMachine, []worktree.Rule{
		{PathPrefix: "/code", Layout: worktree.LayoutRepoDotSuffix},
	})
	_, err := local.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
		Machine: fixtureMachine, PathPrefix: "/other",
		Layout: db.WorktreeMappingLayoutRepoDotSuffix, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, local.UpsertSession(ctx, db.Session{
		ID: "configured", Project: "branch", Machine: fixtureMachine,
		Cwd: "/code/sample-app.dev", Agent: "claude",
	}))
	result, err := local.ApplyWorktreeProjectMappings(ctx, fixtureMachine)
	require.NoError(t, err)
	require.Equal(t, 1, result.UpdatedSessions)
	pusher := newTestSync(t, local, target, storage.PusherOptions{})

	_, err = pusher.Push(ctx, false, nil)
	require.NoError(t, err)
	conn := chtest.Open(t, target.URL, target.Database)
	var project string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT project FROM sessions WHERE id = 'configured'`).Scan(&project))
	assert.Equal(t, "sample_app", project)

	var count uint64
	var layout string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT count(), min(layout) FROM source_worktree_project_mappings`,
	).Scan(&count, &layout))
	assert.EqualValues(t, 1, count, "only the archive rule reaches the replica")
	assert.Equal(t, db.WorktreeMappingLayoutRepoDotSuffix, layout)
}
