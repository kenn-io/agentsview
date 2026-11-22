package sync_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFirstImportNewestWave(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude)
	defer env.engine.Close()
	base := time.Now().Add(-time.Hour)
	for i := range 12 {
		content := testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, fmt.Sprintf("session %d", i)).String()
		path := env.writeClaudeSession(t, "project", fmt.Sprintf("session-%02d.jsonl", i), content)
		mtime := base.Add(time.Duration(i) * time.Second)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
	stats, complete := env.engine.SyncNewest(t.Context(), 3, nil)
	require.False(t, complete)
	require.Equal(t, 3, stats.Synced)
	require.True(t, env.engine.LastSyncFinishedAt(t.Context()).IsZero())
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 3)
	ids := make([]string, 0, len(page.Sessions))
	for _, session := range page.Sessions {
		ids = append(ids, session.ID)
	}
	require.ElementsMatch(t, []string{"session-09", "session-10", "session-11"}, ids)
	env.engine.SyncAll(t.Context(), nil)
	page, err = env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 12)
	require.False(t, env.engine.LastSyncFinishedAt(t.Context()).IsZero())
}

func TestFirstImportMixedSources(t *testing.T) {
	for _, newerWarp := range []bool{false, true} {
		t.Run(fmt.Sprintf("warp-newer-%t", newerWarp), func(t *testing.T) {
			env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentWarp)
			defer env.engine.Close()
			warp := createWarpDB(t, env.warpDir)
			warp.addConversation(t, "warp-session", "2026-04-07 10:00:00", "warp message")
			path := env.writeClaudeSession(t, "project", "claude-session.jsonl", testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, "claude message").String())
			mtime := time.Date(2026, 4, 7, 9, 0, 0, 0, time.UTC)
			want := "warp:warp-session"
			if !newerWarp {
				mtime = mtime.Add(2 * time.Hour)
				want = "claude-session"
			}
			require.NoError(t, os.Chtimes(path, mtime, mtime))
			stats, complete := env.engine.SyncNewest(t.Context(), 1, nil)
			require.False(t, complete)
			require.Equal(t, 1, stats.Synced)
			page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			require.Equal(t, want, page.Sessions[0].ID)
			env.engine.SyncAll(t.Context(), nil)
			page, err = env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 2)
		})
	}
}

func TestFirstImportWithoutSourceTimestamp(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude)
	defer env.engine.Close()
	content := testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, "hello").String()
	older := env.writeClaudeSession(t, "project", "unknown.jsonl", content)
	newer := env.writeClaudeSession(t, "project", "known.jsonl", content)
	require.NoError(t, os.Chtimes(older, time.Unix(0, 0), time.Unix(0, 0)))
	require.NoError(t, os.Chtimes(newer, time.Unix(10, 0), time.Unix(10, 0)))
	stats, complete := env.engine.SyncNewest(t.Context(), 1, nil)
	require.False(t, complete)
	require.Equal(t, 1, stats.Synced)
	unknown, err := env.db.GetSession(t.Context(), "unknown")
	require.NoError(t, err)
	require.Nil(t, unknown)
	stats = env.engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)
	unknown, err = env.db.GetSession(t.Context(), "unknown")
	require.NoError(t, err)
	require.NotNil(t, unknown)
}
