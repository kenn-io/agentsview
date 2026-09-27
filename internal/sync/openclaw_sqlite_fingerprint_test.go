package sync_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestOpenClawSQLiteSyncOnlyChangedMember(t *testing.T) {
	for _, members := range []int{2, 50} {
		t.Run(strconv.Itoa(members), func(t *testing.T) {
			root := t.TempDir()
			dbPath := createOpenClawSyncSQLiteFixture(t, root, "changed")
			for i := 1; i < members; i++ {
				addOpenClawSyncSQLiteSession(t, dbPath, fmt.Sprintf("unchanged-%d", i))
			}
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentOpenClaw: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			require.Equal(t, members, stats.Synced)
			require.Zero(t, engine.SyncAll(t.Context(), nil).Synced)

			sourceDB, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			_, err = sourceDB.ExecContext(t.Context(), `
				INSERT INTO transcript_events(session_id, seq, event_json, created_at)
				VALUES ('changed', 3, ?, 1700000000003)
			`, `{"type":"message","id":"m3","timestamp":"2026-09-22T10:00:03Z","message":{"role":"user","content":"follow up"}}`)
			require.NoError(t, err)
			require.NoError(t, sourceDB.Close())

			stats = engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			assert.Equal(t, 1, stats.Synced, "a member append must not reparse its siblings")
			assertMessageContent(t, database, "openclaw:main:changed", "hello", "old response", "follow up")
			assertMessageContent(t, database, "openclaw:main:unchanged-1", "hello", "old response")
		})
	}
}
