package sync_test

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
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
		for _, stmt := range []string{
			`INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','assistant','new reply',1778767220)`,
			`UPDATE sessions SET title='new title' WHERE id='child'`,
			`UPDATE messages SET content='rewritten' WHERE session_id='child'`,
			`UPDATE sessions SET input_tokens=777 WHERE id='child'`,
			`UPDATE messages SET codex_reasoning_items='[]' WHERE session_id='child'`,
		} {
			t.Run(string(agent)+"/"+stmt, func(t *testing.T) {
				root := t.TempDir()
				conn, path := hermesFreshnessWriter(t, root)
				_, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at,title) VALUES ('sibling','cli',1778767200,'sibling'); INSERT INTO messages(session_id,role,content,timestamp) VALUES ('sibling','user','untouched',1778767210)`)
				require.NoError(t, err)
				database := dbtest.OpenTestDB(t)
				engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
				t.Cleanup(engine.Close)
				require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
				sibling, err := database.GetSessionFull(t.Context(), string(agent)+":sibling")
				require.NoError(t, err)
				require.NotNil(t, sibling)
				_, err = conn.ExecContext(t.Context(), stmt)
				require.NoError(t, err)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
				assert.Equal(t, 1, engine.LastSyncStats().Synced)
				assert.Equal(t, 1, engine.LastSyncStats().TotalSessions)
				after, err := database.GetSessionFull(t.Context(), string(agent)+":sibling")
				require.NoError(t, err)
				require.NotNil(t, after)
				assert.Equal(t, sibling.FileHash, after.FileHash)
				assert.Equal(t, sibling.TranscriptRevision, after.TranscriptRevision)
			})
		}
	}
}

func TestHermesWatcherNativeTimestampRepair(t *testing.T) {
	root := t.TempDir()
	conn, path := hermesFreshnessWriter(t, root)
	_, err := conn.ExecContext(t.Context(), `INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','user','middle',-5),('child','user','last',1778767300)`)
	require.NoError(t, err)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetAllMessages(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.Len(t, before, 3)
	assert.Equal(t, "middle", before[0].Content)
	// Native recovery uses the first valid neighbour for the interior invalid time. An
	// installed recovered output preserves IDs, revision, session row and ledger.
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET timestamp=1778767210 WHERE NOT(timestamp BETWEEN 0.0 AND 4200000000.0)`)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
	assert.Equal(t, 1, engine.LastSyncStats().Synced)
	after, err := database.GetAllMessages(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.Len(t, after, 3)
	assert.Equal(t, "middle", after[1].Content)
	assert.NotEqual(t, before[0].Timestamp, after[1].Timestamp)
}

func TestHermesWatcherCardinality(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, n := range []int{20, 2000} {
			t.Run(string(agent)+"/"+fmt.Sprint(n), func(t *testing.T) {
				root := t.TempDir()
				conn, path := hermesFreshnessWriter(t, root)
				tx, err := conn.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				for i := 1; i < n; i++ {
					_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions(id,source,started_at) VALUES (?,'cli',1778767200)`, fmt.Sprintf("s-%04d", i))
					require.NoError(t, err)
					_, err = tx.ExecContext(t.Context(), `INSERT INTO messages(session_id,role,content,timestamp) VALUES (?,'user','unchanged member',1778767200)`, fmt.Sprintf("s-%04d", i))
					require.NoError(t, err)
				}
				require.NoError(t, tx.Commit())
				database := dbtest.OpenTestDB(t)
				engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
				t.Cleanup(engine.Close)
				require.Equal(t, n, engine.SyncAll(t.Context(), nil).Synced)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO gateway_heartbeats VALUES ('gateway',1778768000)`)
				require.NoError(t, err)
				var scans atomic.Int64
				ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
				// LastSyncStats retains previous stats on a zero-source pass.
				var logs strings.Builder
				output := log.Writer()
				log.SetOutput(&logs)
				err = engine.SyncPathsContext(ctx, []string{path + "-wal"})
				log.SetOutput(output)
				require.NoError(t, err)
				assert.Contains(t, logs.String(), "matched 0 source(s): 0 updated")
				assert.Zero(t, scans.Load())
			})
		}
	}
}

func TestHermesWatcherPreservesSuppressedMember(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			conn, path := hermesFreshnessWriter(t, root)
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			id := string(agent) + ":child"
			require.NoError(t, database.SoftDeleteSession(t.Context(), id))
			before, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, before)
			_, err = conn.ExecContext(t.Context(), `UPDATE messages SET content='must remain suppressed' WHERE session_id='child'`)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
			after, err := database.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.NotNil(t, after.DeletedAt)
			assert.Equal(t, before.FileHash, after.FileHash)
			assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
		})
	}
}
