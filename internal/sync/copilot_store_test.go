package sync_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestCopilotStoreChangesPassIncrementalCutoff(t *testing.T) {
	for _, tc := range []struct {
		name, sessionPath, journal, change string
		wantOutput                         int
	}{
		{"flat database", "session-state/usage.jsonl", "DELETE", "write", 11},
		{"directory WAL", "session-state/usage/events.jsonl", "WAL", "write", 11},
		{"deleted store", "session-state/usage.jsonl", "DELETE", "delete", 3},
		{"older replacement", "session-state/usage/events.jsonl", "DELETE", "replace", 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			path := filepath.Join(root, tc.sessionPath)
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"usage"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"Hello","model":"gpt-5.4","outputTokens":3}}
`), 0o644))
			old := time.Now().Add(-2 * time.Hour)
			require.NoError(os.Chtimes(path, old, old))
			storePath := filepath.Join(root, "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(store.Close()) })
			_, err = store.ExecContext(t.Context(), `PRAGMA journal_mode=`+tc.journal+`;
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY, session_id TEXT,
model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,
cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
INSERT INTO assistant_usage_events VALUES(1,'usage','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:02Z');`)
			require.NoError(err)
			archive := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
			before, err := archive.GetSession(t.Context(), "copilot:usage")
			require.NoError(err)
			require.NotNil(before)
			require.NoError(os.Chtimes(storePath, old, old))
			if tc.journal == "WAL" {
				require.NoError(os.Chtimes(storePath+"-wal", old, old))
			}

			if tc.change == "delete" {
				require.NoError(store.Close())
				require.NoError(os.Remove(storePath))
			} else {
				_, err = store.ExecContext(t.Context(), `UPDATE assistant_usage_events SET output_tokens=11 WHERE id=1`)
				require.NoError(err)
				if tc.change == "replace" {
					require.NoError(store.Close())
					data, err := os.ReadFile(storePath)
					require.NoError(err)
					replacement := filepath.Join(t.TempDir(), "session-store.db")
					require.NoError(os.WriteFile(replacement, data, 0o644))
					require.NoError(os.Chtimes(replacement, old, old))
					require.NoError(os.Rename(replacement, storePath))
				}
			}
			stats := engine.SyncAllSince(t.Context(), old.Add(time.Hour), nil)
			assert.Equal(1, stats.Synced, "a store-only change must survive the transcript cutoff")
			usage, err := archive.GetSessionUsage(t.Context(), "copilot:usage", true)
			require.NoError(err)
			require.NotNil(usage)
			assert.Equal(tc.wantOutput, usage.TotalOutputTokens)
			after, err := archive.GetSession(t.Context(), "copilot:usage")
			require.NoError(err)
			require.NotNil(after)
			assert.Equal(before.FileMtime, after.FileMtime)
			assert.Equal(before.EndedAt, after.EndedAt)
		})
	}
}

func TestCopilotStoreStatErrorsSurviveIncrementalCutoff(t *testing.T) {
	for _, suffix := range []string{"", "-wal"} {
		t.Run("session-store.db"+suffix, func(t *testing.T) {
			require := require.New(t)

			root := t.TempDir()
			path := filepath.Join(root, "session-state", "stat-error.jsonl")
			require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"stat-error"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"Hello","model":"gpt-5.4","outputTokens":3}}
`), 0o644))
			storePath := filepath.Join(root, "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(store.Close()) })
			_, err = store.ExecContext(t.Context(), `CREATE TABLE sessions(id TEXT PRIMARY KEY)`)
			require.NoError(err)
			require.NoError(store.Close())
			archive := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local",
			})
			t.Cleanup(engine.Close)
			require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
			if suffix == "" {
				require.NoError(os.Remove(storePath))
			}
			badPath := storePath + suffix
			if err := os.Symlink(filepath.Base(badPath), badPath); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			// All timestamps, including parent mtime and ctime, predate this
			// cutoff. Only the stat error may retain the source for verification.
			stats := engine.SyncAllSince(t.Context(), time.Now().Add(time.Hour), nil)
			assert.Equal(t, 1, stats.Failed, "an unreadable store must report an error instead of being filtered out")
		})
	}
}

func TestCopilotStoreReadFailurePreservesUsageAndRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "session-state", "locked.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"locked"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"Hello","model":"gpt-5.4","outputTokens":3}}
`), 0o644))
	storePath := filepath.Join(root, "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(err)
	store.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(store.Close()) })
	_, err = store.ExecContext(t.Context(), `PRAGMA journal_mode=DELETE;
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY, session_id TEXT,
model TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,
cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
INSERT INTO assistant_usage_events VALUES(1,'locked','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:02Z');`)
	require.NoError(err)
	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	_, err = store.ExecContext(t.Context(), `UPDATE assistant_usage_events SET output_tokens=11 WHERE id=1;
BEGIN EXCLUSIVE`)
	require.NoError(err)
	t.Cleanup(func() { _, _ = store.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK") })

	assert.Error(engine.SyncPathsContext(t.Context(), []string{storePath}))
	usage, err := archive.GetSessionUsage(t.Context(), "copilot:locked", true)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(7, usage.TotalOutputTokens, "a failed read must preserve the stored usage")
	_, err = store.ExecContext(t.Context(), "ROLLBACK")
	require.NoError(err)

	// Releasing the lock changes no store bytes or fingerprint. A retry must
	// still import the row instead of treating the failed read as current.
	assert.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	usage, err = archive.GetSessionUsage(t.Context(), "copilot:locked", true)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(11, usage.TotalOutputTokens)
}

func TestCopilotStoreGapAndRecoveryDoNotDoubleCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	path := filepath.Join(root, "session-state", "gap", "events.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(os.WriteFile(path, []byte(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"gap"}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"First","model":"gpt-5.4","outputTokens":3}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:02Z","data":{"content":"Later","model":"gpt-5.4","outputTokens":7}}
`), 0o644))
	storePath := filepath.Join(root, "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	_, err = store.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
CREATE TABLE sessions(id TEXT PRIMARY KEY);
INSERT INTO sessions VALUES('gap');
CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY AUTOINCREMENT,
session_id TEXT,model TEXT,input_tokens INTEGER,output_tokens INTEGER,
cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);
INSERT INTO assistant_usage_events VALUES(1,'gap','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:03Z');`)
	require.NoError(err)
	archive := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(archive, agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local"})
	t.Cleanup(engine.Close)
	require.Equal(1, engine.SyncAll(t.Context(), nil).Synced)
	usage, err := archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(10, usage.TotalOutputTokens)
	require.Len(usage.Breakdown, 2)
	total := 0
	for _, entry := range usage.Breakdown {
		total += entry.OutputTokens
	}
	assert.Equal(10, total, "usage reports include the remainder exactly once")
	_, err = store.ExecContext(t.Context(), `INSERT INTO assistant_usage_events VALUES(2,'gap','gpt-5.4',100,3,0,0,0,'2026-09-08T12:00:01Z')`)
	require.NoError(err)
	require.NoError(engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
	usage, err = archive.GetSessionUsage(t.Context(), "copilot:gap", true)
	require.NoError(err)
	require.NotNil(usage)
	assert.Equal(10, usage.TotalOutputTokens)
	require.Len(usage.Breakdown, 2)
	for _, entry := range usage.Breakdown {
		assert.Equal("session-store", entry.Source)
	}
}

func TestCopilotStoreUpdateOnlySyncsChangedSession(t *testing.T) {
	for _, count := range []int{8, 800} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := t.TempDir()
			storePath := filepath.Join(root, "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(store.Close()) })
			_, err = store.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
CREATE TABLE sessions (id TEXT PRIMARY KEY);
CREATE TABLE assistant_usage_events (
id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, model TEXT,
input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
cache_write_tokens INTEGER, reasoning_tokens INTEGER, created_at TEXT);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);`)
			require.NoError(err)
			tx, err := store.BeginTx(t.Context(), nil)
			require.NoError(err)
			for i := range count {
				id := fmt.Sprintf("session-%04d", i)
				_, err = tx.ExecContext(t.Context(), `INSERT INTO sessions VALUES (?);
INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
VALUES (?,'gpt-5.4',100,3,'2026-09-04T17:00:02Z')`, id, id)
				require.NoError(err)
				path := filepath.Join(root, "session-state", id, "events.jsonl")
				require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
				transcript := fmt.Sprintf(`{"type":"session.start","timestamp":"2026-09-04T17:00:00Z","data":{"sessionId":%q}}
{"type":"user.message","timestamp":"2026-09-04T17:00:01Z","data":{"content":"Question"}}
{"type":"assistant.message","timestamp":"2026-09-04T17:00:02Z","data":{"content":"Answer","outputTokens":3}}
`, id)
				require.NoError(os.WriteFile(path, []byte(transcript), 0o644))
			}
			require.NoError(tx.Commit())
			archive := dbtest.OpenTestDB(t)
			cfg := agentsync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local"}
			engine := agentsync.NewEngine(archive, cfg)
			t.Cleanup(engine.Close)
			require.Equal(count, engine.SyncAll(t.Context(), nil).Synced)
			_, err = store.ExecContext(t.Context(), `INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
VALUES ('session-0000','gpt-5.4',100,7,'2026-09-04T17:00:03Z')`)
			require.NoError(err)
			require.NoError(engine.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
			stats := engine.LastSyncStats()
			assert.Equal(1, stats.Synced)
			assert.Equal(count-1, stats.Skipped)
			usage, err := archive.GetUsageEvents(t.Context(), "copilot:session-0000")
			require.NoError(err)
			require.Len(usage, 2)
			assert.Equal(3, usage[0].OutputTokens)
			assert.Equal(7, usage[1].OutputTokens)
			other, err := archive.GetUsageEvents(t.Context(), "copilot:session-0001")
			require.NoError(err)
			require.Len(other, 1)
			assert.Equal(3, other[0].OutputTokens)

			// Rebuilding the transient producer cache must still honor archive fingerprints.
			engine.Close()
			restarted := agentsync.NewEngine(archive, cfg)
			t.Cleanup(restarted.Close)
			bytesBefore := parser.CopilotTranscriptBytesRead()
			stats = restarted.SyncAll(t.Context(), nil)
			assert.Zero(parser.CopilotTranscriptBytesRead()-bytesBefore, "cold engines reuse verified transcript fingerprints")
			assert.Zero(stats.Synced)
			assert.Equal(count, stats.Skipped)

			_, err = store.ExecContext(t.Context(), `DELETE FROM assistant_usage_events WHERE id=(SELECT MAX(id) FROM assistant_usage_events)`)
			require.NoError(err)
			require.NoError(restarted.SyncPathsContext(t.Context(), []string{storePath + "-wal"}))
			assert.Equal(1, restarted.LastSyncStats().Synced)
			usage, err = archive.GetUsageEvents(t.Context(), "copilot:session-0000")
			require.NoError(err)
			require.Len(usage, 1)
			assert.Equal(3, usage[0].OutputTokens)

			missing := filepath.Join(root, "session-state", "session-0001", "events.jsonl")
			require.NoError(os.Remove(missing))
			require.NoError(restarted.ReconcileWatchRoots(t.Context(), []string{root}, false))
			saved, err := archive.GetSessionFull(t.Context(), "copilot:session-0001")
			require.NoError(err)
			require.NotNil(saved, "missing transcripts remain archived")
			assert.NotNil(saved.SourceMissingAt)
			assert.Nil(saved.DeletedAt)
			other, err = archive.GetUsageEvents(t.Context(), "copilot:session-0001")
			require.NoError(err)
			require.Len(other, 1, "source removal preserves archived usage")
		})
	}
}

func TestCopilotStoreWithoutUsageSchemaSkipsUnchangedSessions(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		for _, count := range []int{8, 800} {
			t.Run(fmt.Sprintf("incomplete=%t/sessions=%d", incomplete, count), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				root := t.TempDir()
				storePath := filepath.Join(root, "session-store.db")
				store, err := sql.Open("sqlite3", storePath)
				require.NoError(err)
				store.SetMaxOpenConns(1)
				t.Cleanup(func() { require.NoError(store.Close()) })
				_, err = store.ExecContext(t.Context(), `CREATE TABLE sessions(id TEXT PRIMARY KEY, summary TEXT)`)
				require.NoError(err)
				if incomplete {
					_, err = store.ExecContext(t.Context(), `CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY, session_id TEXT, model TEXT)`)
					require.NoError(err)
				}
				for i := range count {
					id := fmt.Sprintf("session-%04d", i)
					_, err = store.ExecContext(t.Context(), `INSERT INTO sessions VALUES (?, 'before')`, id)
					require.NoError(err)
					path := filepath.Join(root, "session-state", id, "events.jsonl")
					require.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
					transcript := fmt.Sprintf(`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":%q}}
{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"Hello","model":"gpt-5.4","outputTokens":3}}
`, id)
					require.NoError(os.WriteFile(path, []byte(transcript), 0o644))
				}
				archive := dbtest.OpenTestDB(t)
				engine := agentsync.NewEngine(archive, agentsync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{parser.AgentCopilot: {root}}, Machine: "local",
				})
				t.Cleanup(engine.Close)
				require.Equal(count, engine.SyncAll(t.Context(), nil).Synced)

				_, err = store.ExecContext(t.Context(), `UPDATE sessions SET summary='after' WHERE id='session-0000'`)
				require.NoError(err)
				require.NoError(engine.SyncPathsContext(t.Context(), []string{storePath}))
				stats := engine.LastSyncStats()
				require.Zero(stats.Synced, "metadata-only writes must not reparse transcripts")
				assert.Equal(count, stats.Skipped)

				// Taking a lock without writing leaves the SQLite state unchanged.
				// Reusing the cached no-usage result must not query the locked store.
				_, err = store.ExecContext(t.Context(), `BEGIN EXCLUSIVE`)
				require.NoError(err)
				t.Cleanup(func() { _, _ = store.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK") })
				require.NoError(engine.SyncPathsContext(t.Context(), []string{storePath}))
				assert.Equal(count, engine.LastSyncStats().Skipped)
				_, err = store.ExecContext(t.Context(), `ROLLBACK`)
				require.NoError(err)

				if !incomplete {
					_, err = store.ExecContext(t.Context(), `CREATE TABLE assistant_usage_events(id INTEGER PRIMARY KEY, session_id TEXT, model TEXT)`)
					require.NoError(err)
				}
				_, err = store.ExecContext(t.Context(), `ALTER TABLE assistant_usage_events ADD COLUMN input_tokens INTEGER;
ALTER TABLE assistant_usage_events ADD COLUMN output_tokens INTEGER;
ALTER TABLE assistant_usage_events ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE assistant_usage_events ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE assistant_usage_events ADD COLUMN reasoning_tokens INTEGER;
ALTER TABLE assistant_usage_events ADD COLUMN created_at TEXT;
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id,id);
INSERT INTO assistant_usage_events VALUES(1,'session-0000','gpt-5.4',100,7,0,0,0,'2026-09-08T12:00:01Z')`)
				require.NoError(err)
				require.NoError(engine.SyncPathsContext(t.Context(), []string{storePath}))
				stats = engine.LastSyncStats()
				assert.Equal(1, stats.Synced)
				assert.Equal(count-1, stats.Skipped)
				usage, err := archive.GetSessionUsage(t.Context(), "copilot:session-0000", true)
				require.NoError(err)
				require.NotNil(usage)
				assert.Equal(7, usage.TotalOutputTokens)
			})
		}
	}
}
