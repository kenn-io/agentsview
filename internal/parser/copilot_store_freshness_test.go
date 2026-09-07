package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createCopilotUsageStore(t *testing.T, root string) *sql.DB {
	t.Helper()
	store, err := sql.Open("sqlite3", filepath.Join(root, "session-store.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`PRAGMA journal_mode=WAL;
 CREATE TABLE sessions (id TEXT PRIMARY KEY);
 CREATE TABLE assistant_usage_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, model TEXT,
 input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
 cache_write_tokens INTEGER, reasoning_tokens INTEGER, created_at TEXT);
 CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id, id);`)
	require.NoError(t, err)
	return store
}

func TestCopilotStoreFingerprintWorkScalesWithChangedUsage(t *testing.T) {
	for _, count := range []int{8, 800} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			store := createCopilotUsageStore(t, root)
			tx, err := store.Begin()
			require.NoError(t, err)
			for i := range count {
				id := fmt.Sprintf("session-%04d", i)
				_, err := tx.Exec(`INSERT INTO sessions VALUES (?);
     INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
     VALUES (?, 'gpt-5.4', 100, 3, '2026-09-04T17:00:02Z')`, id, id)
				require.NoError(t, err)
				path := filepath.Join(root, copilotStateDir, id, "events.jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				text := fmt.Sprintf("{\"type\":\"session.start\",\"timestamp\":\"2026-09-04T17:00:00Z\",\"data\":{\"sessionId\":%q}}\n", id) +
					fmt.Sprintf("{\"type\":\"assistant.message\",\"timestamp\":\"2026-09-04T17:00:01Z\",\"data\":{\"content\":%q,\"outputTokens\":3}}\n", strings.Repeat("x", 8192))
				require.NoError(t, os.WriteFile(path, []byte(text), 0o644))
			}
			require.NoError(t, tx.Commit())
			provider := newCopilotTestProvider(t, root)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			before := make(map[string]SourceFingerprint)
			for _, source := range sources {
				fp, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				before[source.Key] = fp
			}
			cache := provider.sources.cache
			bytesBefore, rowsBefore := cache.transcriptBytes, cache.usageRows
			_, err = store.Exec(`INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
    VALUES ('session-0000','gpt-5.4',100,7,'2026-09-04T17:00:03Z')`)
			require.NoError(t, err)
			changed, err := provider.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: filepath.Join(root, "session-store.db-wal"), EventKind: "write"})
			require.NoError(t, err)
			var changedKeys []string
			for _, source := range changed {
				fp, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				if fp.Hash != before[source.Key].Hash {
					changedKeys = append(changedKeys, source.Key)
				}
				assert.Equal(t, before[source.Key].MTimeNS, fp.MTimeNS)
			}
			assert.Equal(t, []string{filepath.Join(root, copilotStateDir, "session-0000", "events.jsonl")}, changedKeys)
			assert.Zero(t, cache.transcriptBytes-bytesBefore, "a store-only event must not reread transcript payloads")
			assert.Equal(t, int64(2), cache.usageRows-rowsBefore, "only the changed session's two usage rows are read")
		})
	}
}

func TestCopilotStoreFingerprintReconcilesEditsAndDeletions(t *testing.T) {
	root := t.TempDir()
	store := createCopilotUsageStore(t, root)
	_, err := store.Exec(`INSERT INTO sessions VALUES ('session-a');
 INSERT INTO assistant_usage_events VALUES (1,'session-a','gpt-5.4',100,3,0,0,0,'2026-09-04T17:00:01Z');
 INSERT INTO assistant_usage_events VALUES (2,'session-a','gpt-5.4',100,7,0,0,0,'2026-09-04T17:00:02Z');`)
	require.NoError(t, err)
	cache := newCopilotSourceCache()
	path := filepath.Join(root, "session-store.db")
	before, err := cache.usageHash(t.Context(), path, "session-a")
	require.NoError(t, err)
	_, err = store.Exec(`UPDATE assistant_usage_events SET output_tokens=9 WHERE id=1`)
	require.NoError(t, err)
	same, err := cache.usageHash(t.Context(), path, "session-a")
	require.NoError(t, err)
	assert.Equal(t, before, same, "edits below the latest ID wait for periodic verification")
	snapshot := cache.stores[path]
	snapshot.verifiedAt = time.Now().Add(-copilotStoreVerifyInterval)
	cache.stores[path] = snapshot
	edited, err := cache.usageHash(t.Context(), path, "session-a")
	require.NoError(t, err)
	assert.NotEqual(t, before, edited)
	_, err = store.Exec(`DELETE FROM assistant_usage_events WHERE id=1`)
	require.NoError(t, err)
	snapshot = cache.stores[path]
	snapshot.verifiedAt = time.Now().Add(-copilotStoreVerifyInterval)
	cache.stores[path] = snapshot
	deleted, err := cache.usageHash(t.Context(), path, "session-a")
	require.NoError(t, err)
	assert.NotEqual(t, edited, deleted)
	_, err = store.Exec(`DELETE FROM assistant_usage_events; DELETE FROM sessions`)
	require.NoError(t, err)
	empty, err := cache.usageHash(t.Context(), path, "session-a")
	require.NoError(t, err)
	assert.Empty(t, empty, "removing the last row is visible on the fast path")
}
