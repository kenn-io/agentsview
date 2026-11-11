package sync_test

import (
	"database/sql"
	"log"
	"os"
	"path/filepath"
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
			require.NoError(t, engine.ReparsePathsContext(t.Context(), []string{path + "-wal"}))
			assert.Equal(t, 2, engine.LastSyncStats().Synced)
		})
	}
}

func TestHermesWatcherDiscoversStaleAndNewOrphanTranscripts(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			conn, path := hermesFreshnessWriter(t, root)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES ('sibling','cli',1778767200),('trashed','cli',1778767200); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('sibling','user','untouched',1778767210),('trashed','user','original',1778767210)`)
			require.NoError(t, err)
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, 3, engine.SyncAll(t.Context(), nil).Synced)
			require.NoError(t, database.SetSessionDataVersion(t.Context(), string(agent)+":child", 0))
			require.NoError(t, database.SoftDeleteSession(t.Context(), string(agent)+":trashed"))
			dir := filepath.Join(root, "sessions")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			for _, id := range []string{"child", "new", "trashed"} {
				content := `{"role":"session_meta","platform":"cli","timestamp":"2026-05-14T10:00:00Z"}` + "\n" + `{"role":"user","content":"orphan ` + id + `","timestamp":"2026-05-14T10:01:00Z"}` + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(dir, "session_"+id+".jsonl"), []byte(content), 0o600))
			}
			_, err = conn.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id IN ('child','trashed'); DELETE FROM sessions WHERE id IN ('child','trashed')`)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
			for _, id := range []string{"child", "new"} {
				messages, err := database.GetAllMessages(t.Context(), string(agent)+":"+id)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "orphan "+id, messages[0].Content)
			}
			trashed, err := database.GetSessionFull(t.Context(), string(agent)+":trashed")
			require.NoError(t, err)
			require.NotNil(t, trashed)
			assert.NotNil(t, trashed.DeletedAt)
		})
	}
}
