package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
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

func TestCopilotStoreGapRetainsOnlyAggregateOutputRemainder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		firstModel    string
		storeOutput   int
		wantTotal     int
		wantRemainder int
	}{
		{"missing first call", "gpt-5.4", 7, 10, 3},
		{"all calls stored", "gpt-5.4", 10, 10, 0},
		{"store has extra calls", "gpt-5.4", 20, 20, 0},
		{"missing other model", "gpt-5.5", 20, 23, 3},
		{"unknown model overlaps store", "", 10, 10, 0},
		{"unknown model excess", "", 7, 10, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := createCopilotUsageStore(t, root)
			_, err := store.Exec(`INSERT INTO sessions VALUES ('gap');
INSERT INTO assistant_usage_events(session_id,model,input_tokens,output_tokens,created_at)
VALUES ('gap','gpt-5.4',100,?,'2026-09-08T12:00:03Z')`, tc.storeOutput)
			require.NoError(t, err)
			path := writeCopilotJSONL(t,
				`{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"gap"}}`,
				fmt.Sprintf(`{"type":"assistant.message","timestamp":"2026-09-08T12:00:01Z","data":{"content":"First","model":%q,"outputTokens":3}}`, tc.firstModel),
				`{"type":"assistant.message","timestamp":"2026-09-08T12:00:02Z","data":{"content":"Later","model":"gpt-5.4","outputTokens":7}}`,
			)
			sess, msgs, events, err := newCopilotTestProvider(t).parseSessionWithStore(path, "local", filepath.Join(root, "session-store.db"))
			require.NoError(t, err)
			require.NotNil(t, sess)
			assert.Equal(t, tc.wantTotal, sess.TotalOutputTokens)
			for _, msg := range msgs {
				assert.Empty(t, msg.TokenUsage, "no invented request-to-response correlation")
			}
			require.NotEmpty(t, events)
			assert.Equal(t, tc.storeOutput, events[0].OutputTokens)
			if tc.wantRemainder == 0 {
				require.Len(t, events, 1)
			} else {
				require.Len(t, events, 2)
				assert.Equal(t, "transcript-output-remainder", events[1].Source)
				assert.Equal(t, tc.firstModel, events[1].Model)
				assert.Equal(t, tc.wantRemainder, events[1].OutputTokens)
				assert.Nil(t, events[1].MessageOrdinal)
			}
		})
	}
}

func TestParseCopilotSession_StoreUsageSupersedesShutdown(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`
		CREATE TABLE assistant_usage_events (
			id INTEGER PRIMARY KEY, session_id TEXT, model TEXT,
			input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
			cache_write_tokens INTEGER, reasoning_tokens INTEGER,
			total_nano_aiu INTEGER, created_at TEXT
		);
		INSERT INTO assistant_usage_events VALUES
			(42, 'store-wins', 'gpt-5.6-sol', 200, 50, 100, 20, 10,
			 250000000, '2026-09-04T17:40:54.970Z');
	`)
	require.NoError(t, err)
	path := writeCopilotJSONL(t,
		`{"type":"session.start","data":{"sessionId":"store-wins"},"timestamp":"2026-09-04T17:00:00Z"}`,
		`{"type":"user.message","data":{"content":"Hello"},"timestamp":"2026-09-04T17:00:01Z"}`,
		`{"type":"assistant.message","data":{"content":"Hi.","model":"gpt-5.6-sol","outputTokens":3},"timestamp":"2026-09-04T17:00:02Z"}`,
		`{"type":"session.shutdown","data":{"totalNanoAiu":100000000,"modelMetrics":{"gpt-5.6-sol":{"usage":{"inputTokens":10,"outputTokens":3}}}},"timestamp":"2026-09-04T17:00:03Z"}`,
	)

	sess, _, usage, err := newCopilotTestProvider(t).parseSessionWithStore(
		path, "local", storePath,
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, usage, 2)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 50, sess.TotalOutputTokens)
	assert.Equal(t, "session-store", usage[0].Source)
	assert.Equal(t, 80, usage[0].InputTokens)
	assert.Equal(t, 50, usage[0].OutputTokens)
	assert.Nil(t, usage[0].Cost)
	assert.Empty(t, usage[0].CostSource)
	assert.Equal(t, "2026-09-04T17:40:54.970Z", usage[0].OccurredAt)
	assert.Equal(t, "session-store:42", usage[0].DedupKey)
	assert.Equal(t, "shutdown", usage[1].Source)
	assert.Zero(t, usage[1].InputTokens)
	assert.Zero(t, usage[1].OutputTokens)
	assert.Zero(t, usage[1].CacheCreationInputTokens)
	assert.Zero(t, usage[1].CacheReadInputTokens)
	assert.Zero(t, usage[1].ReasoningTokens)
	require.NotNil(t, usage[1].Cost)
	assert.Equal(t, money.MustParseDollars("0.001"), *usage[1].Cost)
	assert.Equal(t, copilotReportedCostSource, usage[1].CostSource)
	assert.Equal(t, "2026-09-04T17:00:03Z", usage[1].OccurredAt)
}

func TestLoadCopilotStoreUsage(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session-store.db")
	store, err := sql.Open("sqlite3", storePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.Exec(`
		CREATE TABLE assistant_usage_events (
			id INTEGER PRIMARY KEY, session_id TEXT, model TEXT,
			input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
			cache_write_tokens INTEGER, reasoning_tokens INTEGER,
			total_nano_aiu INTEGER, created_at TEXT
		);
		INSERT INTO assistant_usage_events VALUES
			(7, 'session-1', 'claude-sonnet-4.6', 100, 30, 60, 10, 4,
			 125050000, '2026-09-04T17:40:54.970Z'),
			(8, 'other-session', 'gpt-5.6-sol', 9, 8, 0, 0, 0,
			 5000000, '2026-09-04T17:41:00Z');
	`)
	require.NoError(t, err)

	events, err := loadCopilotStoreUsage(storePath, "session-1")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "session-store", events[0].Source)
	assert.Equal(t, "claude-sonnet-4-6", events[0].Model)
	assert.Equal(t, 30, events[0].InputTokens)
	assert.Equal(t, 30, events[0].OutputTokens)
	assert.Equal(t, 60, events[0].CacheReadInputTokens)
	assert.Equal(t, 10, events[0].CacheCreationInputTokens)
	assert.Equal(t, 4, events[0].ReasoningTokens)
	assert.Nil(t, events[0].Cost)
	assert.Empty(t, events[0].CostSource)
	assert.Equal(t, "2026-09-04T17:40:54.970Z", events[0].OccurredAt)
	assert.Equal(t, "session-store:7", events[0].DedupKey)
}

func TestParseCopilotSession_StoreWithoutUsageSchema(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			storePath := filepath.Join(t.TempDir(), "session-store.db")
			store, err := sql.Open("sqlite3", storePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			// Copilot 1.0.60 stores sessions but has no assistant_usage_events.
			_, err = store.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
			require.NoError(t, err)
			lines := []string{
				`{"type":"session.start","timestamp":"2026-06-05T12:00:00Z","data":{"sessionId":"no-store-usage"}}`,
				`{"type":"assistant.message","timestamp":"2026-06-05T12:00:01Z","data":{"content":"Hello","model":"gpt-5.4","outputTokens":3}}`,
			}
			if shutdown {
				lines = append(lines, `{"type":"session.shutdown","timestamp":"2026-06-05T12:00:02Z","data":{"modelMetrics":{"gpt-5.4":{"usage":{"inputTokens":10,"outputTokens":3}}}}}`)
			}
			path := writeCopilotJSONL(t, lines...)
			sess, messages, usage, err := newCopilotTestProvider(t).parseSessionWithStore(path, "local", storePath)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, messages, 1)
			assert.Equal(t, "Hello", messages[0].Content)
			assert.Equal(t, 3, sess.TotalOutputTokens)
			if shutdown {
				require.Len(t, usage, 1)
				assert.Equal(t, "shutdown", usage[0].Source)
				assert.Equal(t, 10, usage[0].InputTokens)
				assert.Equal(t, 3, usage[0].OutputTokens)
				assert.Empty(t, messages[0].TokenUsage)
			} else {
				assert.Empty(t, usage)
				assert.JSONEq(t, `{"output_tokens":3}`, string(messages[0].TokenUsage))
			}
		})
	}
}

func TestCopilotFingerprintRejectsUnreadableStoreState(t *testing.T) {
	for _, suffix := range []string{"", "-wal"} {
		t.Run("session-store.db"+suffix, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "session-state", "fingerprint.jsonl")
			writeSourceFile(t, path, `{"type":"session.start","timestamp":"2026-09-08T12:00:00Z","data":{"sessionId":"fingerprint"}}`)
			provider := newCopilotTestProvider(t, root)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			absent, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err, "a missing optional store is valid")

			store := createCopilotUsageStore(t, root)
			_, err = store.Exec(`INSERT INTO assistant_usage_events(session_id,model,output_tokens,created_at)
				VALUES ('fingerprint','gpt-5.4',3,'2026-09-08T12:00:01Z')`)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			storePath := filepath.Join(root, "session-store.db")
			original, err := os.ReadFile(storePath)
			require.NoError(t, err)
			// A deterministic failed capture exercises the same retry contract
			// as a header read racing a real WAL checkpoint.
			require.NoError(t, os.WriteFile(storePath+suffix, make([]byte, 100), 0o644))
			_, err = provider.Fingerprint(t.Context(), sources[0])
			assert.Error(t, err, "an unreadable marker must not become a valid fingerprint")

			if suffix == "" {
				require.NoError(t, os.WriteFile(storePath, original, 0o644))
			} else {
				require.NoError(t, os.Remove(storePath+suffix))
			}
			recovered, err := provider.Fingerprint(t.Context(), sources[0])
			require.NoError(t, err)
			assert.NotEqual(t, absent.Hash, recovered.Hash, "recovery must expose the available store")
		})
	}
}
