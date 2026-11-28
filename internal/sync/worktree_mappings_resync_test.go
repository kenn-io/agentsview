package sync_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/agentsview/internal/worktree"
)

func TestResyncOrphansAfterRemovingConfiguredWorktreeMappings(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentClaude)
	t.Cleanup(env.engine.Close)
	ctx := t.Context()
	prefix := filepath.ToSlash(filepath.Join(t.TempDir(), "checkouts"))
	// Leave one readable source so the rebuild runs with both unavailable
	// transcripts present only in the old archive.
	env.writeClaudeSession(t, "live", "live.jsonl", testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "live transcript").String())

	orphans := []struct {
		id, cwd, project, snapshotProject string
	}{
		{"with-snapshot", prefix + "/app.dev", "recorded", "recorded"},
		{"without-snapshot", prefix + "/app.test", "branch", ""},
	}
	for _, tc := range orphans {
		source := filepath.Join(env.claudeDir, "missing", tc.id+".jsonl")
		session := db.Session{ID: tc.id, Agent: "claude", Machine: "local", Cwd: tc.cwd, Project: tc.project, FilePath: new(source)}
		obs := export.ProjectIdentityObservation{
			SessionID: session.ID, Machine: session.Machine, RootPath: session.Cwd, Project: session.Project,
			ObservedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		if tc.snapshotProject != "" {
			obs = export.WithRecordedGitRemote(obs, "https://example.com/team/recorded.git")
		}
		require.NoError(t, env.db.UpsertSessionWithProjectIdentity(ctx, session, obs, tc.snapshotProject))
	}
	snapshots, err := env.db.ListSessionProjectIdentitySnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "with-snapshot", snapshots[0].SessionID)
	assert.Equal(t, export.ProjectResolutionResolved, snapshots[0].RemoteResolution)

	env.db.SetConfiguredWorktreeMappings("local", []worktree.Rule{
		{PathPrefix: prefix, Layout: worktree.LayoutRepoDotSuffix},
	})
	applied, err := env.db.ApplyWorktreeProjectMappings(ctx, "local")
	require.NoError(t, err)
	require.Equal(t, 2, applied.UpdatedSessions)
	for _, tc := range orphans {
		assertSessionProject(t, env.db, tc.id, "app")
	}

	env.db.SetConfiguredWorktreeMappings("local", nil)
	stats := env.engine.ResyncAll(ctx, nil)
	require.False(t, stats.Aborted, "ResyncAll aborted: %v", stats.Warnings)
	require.Equal(t, 1, stats.Synced)
	require.Equal(t, 2, stats.OrphanedCopied)
	assertSessionProject(t, env.db, "with-snapshot", "recorded")
	assertSessionProject(t, env.db, "without-snapshot", "app")
}
