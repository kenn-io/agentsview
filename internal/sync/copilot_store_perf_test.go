package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestCopilotStoreUpdateOnlySyncsChangedSession(t *testing.T) {
	for _, count := range []int{8, 800} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			storePath := filepath.Join(root, "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			_, err = store.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE sessions (id TEXT PRIMARY KEY);
CREATE TABLE assistant_usage_events (
id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, model TEXT,
input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
cache_write_tokens INTEGER, reasoning_tokens INTEGER, created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);`)
			require.NoError(t, err)
			tx, err := store.Begin()
			require.NoError(t, err)
			for i := range count {
				id := fmt.Sprintf("session-%04d", i)
				_, err = tx.Exec(`INSERT INTO sessions VALUES (?);
INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
VALUES (?,'gpt-5.4',100,3,'2026-09-04T17:00:02Z')`, id, id)
				require.NoError(t, err)
				path := filepath.Join(root, "session-state", id, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				transcript := fmt.Sprintf(`{"type":"session.start","timestamp":"2026-09-04T17:00:00Z","data":{"sessionId":%q}}
{"type":"user.message","timestamp":"2026-09-04T17:00:01Z","data":{"content":"Question"}}
{"type":"assistant.message","timestamp":"2026-09-04T17:00:02Z","data":{"content":"Answer","outputTokens":3}}
`, id)
				require.NoError(t, os.WriteFile(path, []byte(transcript), 0o644))
			}
			require.NoError(t, tx.Commit())
			archive := dbtest.OpenTestDB(t)
			cfg := agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local"}
			engine := agentsync.NewEngine(archive, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, count, engine.SyncAll(t.Context(), nil).Synced)
			_, err = store.Exec(`INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
VALUES ('session-0000','gpt-5.4',100,7,'2026-09-04T17:00:03Z')`)
			require.NoError(t, err)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
			stats := engine.LastSyncStats()
			assert.Equal(t, 1, stats.Synced)
			assert.Equal(t, count-1, stats.Skipped)
			usage, err := archive.GetUsageEvents(t.Context(), "copilot:session-0000")
			require.NoError(t, err)
			require.Len(t, usage, 2)
			assert.Equal(t, 3, usage[0].OutputTokens)
			assert.Equal(t, 7, usage[1].OutputTokens)
			other, err := archive.GetUsageEvents(t.Context(), "copilot:session-0001")
			require.NoError(t, err)
			require.Len(t, other, 1)
			assert.Equal(t, 3, other[0].OutputTokens)

			// Rebuilding the transient producer cache must still honor archive fingerprints.
			engine.Close()
			restarted := agentsync.NewEngine(archive, cfg)
			t.Cleanup(restarted.Close)
			bytesBefore := parser.CopilotTranscriptBytesRead()
			stats = restarted.SyncAll(t.Context(), nil)
			assert.Zero(t, parser.CopilotTranscriptBytesRead()-bytesBefore, "cold engines reuse verified transcript fingerprints")
			assert.Zero(t, stats.Synced)
			assert.Equal(t, count, stats.Skipped)

			_, err = store.Exec(`DELETE FROM assistant_usage_events WHERE id=(SELECT MAX(id) FROM assistant_usage_events)`)
			require.NoError(t, err)
			require.NoError(t, restarted.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
			assert.Equal(t, 1, restarted.LastSyncStats().Synced)
			usage, err = archive.GetUsageEvents(t.Context(), "copilot:session-0000")
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, 3, usage[0].OutputTokens)

			missing := filepath.Join(root, "session-state", "session-0001", "events.jsonl")
			require.NoError(t, os.Remove(missing))
			require.NoError(t, restarted.ReconcileWatchRoots(t.Context(), []string{root}, false))
			saved, err := archive.GetSessionFull(t.Context(), "copilot:session-0001")
			require.NoError(t, err)
			require.NotNil(t, saved, "missing transcripts remain archived")
			assert.NotNil(t, saved.SourceMissingAt)
			assert.Nil(t, saved.DeletedAt)
			other, err = archive.GetUsageEvents(t.Context(), "copilot:session-0001")
			require.NoError(t, err)
			require.Len(t, other, 1, "source removal preserves archived usage")
		})
	}
}

func TestCopilotStoreGapAndRecoveryDoNotDoubleCount(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session-state", "gap", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"gap"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"First","model":"gpt-5.4","outputTokens":3}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:02Z","data":{"content":"Later","model":"gpt-5.4","outputTokens":7}}
`), 0o644))
	storePath := filepath.Join(root, "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE sessions(id TEXT PRIMARY KEY);
INSERT INTO sessions VALUES('gap');
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY AUTOINCREMENT,
session_id TEXT,model TEXT,input_tokens INTEGER,output_tokens INTEGER,
cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);
INSERT INTO assistant_usage_events VALUES(1,'gap','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:03Z');`)
	require.NoError(t, err)
	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	usage, err := archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.TotalOutputTokens)
	require.Len(t, usage.Breakdown, 2)
	total := 0
	for _, entry := range usage.Breakdown {
		total += entry.OutputTokens
	}
	assert.Equal(t, 10, total, "usage reports include the remainder exactly once")
	_, err = store.Exec(`INSERT INTO assistant_usage_events VALUES(2,'gap','gpt-5.4',100,3,0,0,0,'2026-09-08T12:00:01Z')`)
	require.NoError(t, err)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
	usage, err = archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.TotalOutputTokens)
	require.Len(t, usage.Breakdown, 2)
	for _, entry := range usage.Breakdown {
		assert.Equal(t, "session-store", entry.Source)
	}
}
