package sync_test

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func hermesFreshnessWriter(t *testing.T, root string) (*sql.DB, string) {
	t.Helper()
	path := writeHermesSyncStateDB(t, root)
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
 ALTER TABLE sessions ADD COLUMN transcript_revision INTEGER DEFAULT 0;
 CREATE INDEX idx_messages_session ON messages(session_id, timestamp);
 CREATE TABLE gateway_heartbeats(id TEXT PRIMARY KEY, last_heartbeat REAL);
 CREATE TRIGGER messages_transcript_revision_update AFTER UPDATE OF role, content, api_content, tool_calls, tool_call_id, tool_name, reasoning, reasoning_content, reasoning_details ON messages BEGIN UPDATE sessions SET transcript_revision = transcript_revision + 1 WHERE id = new.session_id; END;
 CREATE TRIGGER messages_display_revision_update AFTER UPDATE OF codex_reasoning_items, codex_message_items, display_kind, display_metadata ON messages BEGIN UPDATE sessions SET transcript_revision = transcript_revision + 1 WHERE id = new.session_id; END;`)
	require.NoError(t, err)
	return conn, path
}

// This catches whole-store deep fingerprint seeding even when no session is reparsed.
func TestHermesHeartbeatWriteChecksNoMembers(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			conn, path := hermesFreshnessWriter(t, root)
			database := dbtest.OpenTestDB(t)
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			_, err := conn.ExecContext(t.Context(), `INSERT INTO gateway_heartbeats VALUES ('gateway', 1778767300)`)
			require.NoError(t, err)
			var scans atomic.Int64
			ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
			selected, updated := hermesPassStats(t, func() error { return engine.SyncPathsContext(ctx, []string{path + "-wal"}) })
			assert.Zero(t, scans.Load(), "heartbeat must not read transcript payloads")
			assert.Zero(t, updated)
			assert.Zero(t, selected)
			_, ok := parser.ResolveStoredMemberFreshnessContainer(mustHermesProvider(t, agent, root), filepath.Join(root, "state.db"))
			assert.True(t, ok)
		})
	}
}

func mustHermesProvider(t *testing.T, agent parser.AgentType, root string) parser.Provider {
	t.Helper()
	p, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	return p
}

func TestHermesWatcherSelectsOnlyChangedMember(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		for _, stmt := range []string{
			`INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','assistant','new reply',1778767220)`,
			`UPDATE sessions SET title='new title' WHERE id='child'`,
			`UPDATE messages SET content='rewritten' WHERE session_id='child'`,
			`UPDATE sessions SET input_tokens=777 WHERE id='child'`,
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
				var scans atomic.Int64
				ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
				require.NoError(t, engine.SyncPathsContext(ctx, []string{path + "-wal"}))
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
	_, err := conn.ExecContext(t.Context(), `UPDATE messages SET timestamp=-5 WHERE session_id='child'`)
	require.NoError(t, err)
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	before, err := database.GetAllMessages(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.Len(t, before, 1)
	// Native recovery uses the session start when no valid neighbour exists. An
	// installed recovered output preserves IDs, revision, session row and ledger.
	_, err = conn.ExecContext(t.Context(), `UPDATE messages SET timestamp=(SELECT started_at FROM sessions WHERE id=messages.session_id) WHERE NOT(timestamp BETWEEN 0.0 AND 4200000000.0)`)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path + "-wal"}))
	assert.Equal(t, 1, engine.LastSyncStats().Synced)
	after, err := database.GetAllMessages(t.Context(), "hermes:child")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.NotEqual(t, before[0].Timestamp, after[0].Timestamp)
}

func TestHermesWatcherCardinality(t *testing.T) {
	for _, n := range []int{20, 2000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
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
			engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentHermes: {root}}, Machine: "local"})
			t.Cleanup(engine.Close)
			require.Equal(t, n, engine.SyncAll(t.Context(), nil).Synced)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO gateway_heartbeats VALUES ('gateway',1778768000)`)
			require.NoError(t, err)
			var scans atomic.Int64
			ctx := parser.WithSharedContainerScanObserver(t.Context(), func() { scans.Add(1) })
			selected, updated := hermesPassStats(t, func() error { return engine.SyncPathsContext(ctx, []string{path + "-wal"}) })
			assert.Zero(t, scans.Load())
			assert.Zero(t, selected)
			assert.Zero(t, updated)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO messages(session_id,role,content,timestamp) VALUES ('child','assistant','new',1778768000)`)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(ctx, []string{path + "-wal"}))
			assert.Equal(t, 1, engine.LastSyncStats().TotalSessions)
			assert.Equal(t, 1, engine.LastSyncStats().Synced)
			assert.LessOrEqual(t, scans.Load(), int64(3), "deep work is bounded at both cardinalities")
		})
	}
}

// LastSyncStats intentionally retains the previous stats on a zero-source
// pass. Read the per-pass user summary to count selection and updates instead.
type hermesPassLog struct {
	mu                gosync.Mutex
	selected, updated int
	seen              bool
}

func (r *hermesPassLog) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(string(data), "sync: changed paths:") {
		text := string(data)
		text = text[strings.Index(text, "sync: changed paths:"):]
		var paths int
		if n, _ := fmt.Sscanf(text, "sync: changed paths: %d path(s) matched %d source(s): %d updated", &paths, &r.selected, &r.updated); n == 3 {
			r.seen = true
		}
	}
	return len(data), nil
}
func hermesPassStats(t *testing.T, run func() error) (int, int) {
	t.Helper()
	capture := &hermesPassLog{}
	writer := log.Writer()
	log.SetOutput(io.MultiWriter(writer, capture))
	defer log.SetOutput(writer)
	require.NoError(t, run())
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.True(t, capture.seen, "changed-path summary must be recorded")
	return capture.selected, capture.updated
}
