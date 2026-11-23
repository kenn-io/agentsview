package sync_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFirstImportWaveSelectsRecentFileAndDatabaseSources(t *testing.T) {
	env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentWarp)
	t.Cleanup(env.engine.Close)
	warp := createWarpDB(t, env.warpDir)
	warp.addConversation(t, "recent", "2026-04-07 12:00:00", "warp message")
	for i := range 102 {
		path := env.writeClaudeSession(t, "project", fmt.Sprintf("session-%03d.jsonl", i), testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, "claude message").String())
		mtime := time.Date(2026, 4, 7, 11, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		if i == 101 {
			mtime = mtime.Add(2 * time.Hour)
		}
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}

	paused := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		discoveries := 0
		_, _, err := env.engine.RunFirstImport(t.Context(), func(progress syncpkg.Progress) {
			if progress.Phase == syncpkg.PhaseDiscovering {
				discoveries++
				if discoveries == 2 {
					close(paused)
					<-resume
				}
			}
		})
		done <- err
	}()
	defer func() {
		close(resume)
		require.NoError(t, <-done)
	}()
	select {
	case <-paused:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "reconciliation did not start")
	}
	page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 200})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 100)
	var ids []string
	for _, session := range page.Sessions {
		ids = append(ids, session.ID)
		messages, err := env.db.GetMessages(t.Context(), session.ID, 0, 10, true)
		require.NoError(t, err)
		require.NotEmpty(t, messages)
	}
	require.Contains(t, ids, "session-101")
	require.Contains(t, ids, "warp:recent")
	require.NotContains(t, ids, "session-000")
}
