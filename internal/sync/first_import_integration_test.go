package sync_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFirstImportPublishesNewestSourceBeforeReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mtime time.Time
		want  string
	}{
		{"file newest", time.Date(2026, 4, 7, 13, 0, 0, 0, time.UTC), "claude-recent"},
		{"database newest", time.Date(2026, 4, 7, 11, 0, 0, 0, time.UTC), "warp:recent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupFocusedTestEnv(t, parser.AgentClaude, parser.AgentWarp)
			t.Cleanup(env.engine.Close)
			warp := createWarpDB(t, env.warpDir)
			warp.addConversation(t, "recent", "2026-04-07 12:00:00", "warp message")
			path := env.writeClaudeSession(t, "project", "claude-recent.jsonl", testjsonl.NewSessionBuilder().AddClaudeUser(tsEarly, "claude message").String())
			require.NoError(t, os.Chtimes(path, tc.mtime, tc.mtime))

			paused, resume := make(chan struct{}), make(chan struct{})
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
			page, err := env.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, page.Sessions, 1)
			require.Equal(t, tc.want, page.Sessions[0].ID)
			messages, err := env.db.GetMessages(t.Context(), tc.want, 0, 10, true)
			require.NoError(t, err)
			require.NotEmpty(t, messages)
		})
	}
}
