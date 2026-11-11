package sync_test

import (
	"database/sql"
	"log"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest/hermesfixture"
	"go.kenn.io/agentsview/internal/sync"
)

func hermesFreshnessWriter(t *testing.T, root string) (*sql.DB, string) {
	t.Helper()
	path := writeHermesSyncStateDB(t, root)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	hermesfixture.CreateWatcherSchema(t, conn)
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
 CREATE TABLE gateway_heartbeats(id TEXT PRIMARY KEY, last_heartbeat REAL);`)
	require.NoError(t, err)
	return conn, path
}

func TestHermesWatcherSelectsOnlyChangedMember(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			conn, path := hermesFreshnessWriter(t, root)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at,title) VALUES ('sibling','cli',1778767200,'sibling'); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('sibling','user','untouched',1778767210)`)
			require.NoError(t, err)
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
			_, err = conn.ExecContext(t.Context(), `UPDATE messages SET content='rewritten' WHERE session_id='child'`)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
			assert.Equal(t, 1, engine.LastSyncStats().Synced)
			assert.Equal(t, 1, engine.LastSyncStats().TotalSessions)
			messages, err := database.GetAllMessages(t.Context(), string(agent)+":child")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "rewritten", messages[0].Content)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO gateway_heartbeats VALUES ('gateway',1778768000)`)
			require.NoError(t, err)
			var logs strings.Builder
			output := log.Writer()
			log.SetOutput(&logs)
			err = engine.SyncPathsContext(t.Context(), []string{path + "-wal"})
			log.SetOutput(output)
			require.NoError(t, err)
			assert.Contains(t, logs.String(), "matched 0 source(s): 0 updated")
		})
	}
}
