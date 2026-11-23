package sync_test

import (
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

func TestSyncAllSinceOpenClawSQLiteKeepsTimestampPreservingChanges(t *testing.T) {
	for _, members := range []int{2, 50} {
		for _, tc := range []struct {
			name     string
			mutation string
			want     []string
		}{
			{
				name: "edit",
				mutation: `UPDATE transcript_events
					SET event_json = REPLACE(event_json, 'old response', 'new response')
					WHERE session_id = 'changed' AND seq = 2`,
				want: []string{"hello", "new response"},
			},
			{
				name:     "delete",
				mutation: `DELETE FROM transcript_events WHERE session_id = 'changed' AND seq = 1`,
				want:     []string{"old response"},
			},
		} {
			t.Run(strconv.Itoa(members)+"/"+tc.name, func(t *testing.T) {
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

				// Fixture event timestamps are in 2023. Neither mutation changes
				// the latest event timestamp, even though the member has changed.
				cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				require.Zero(t, engine.SyncAllSince(t.Context(), cutoff, nil).Synced)
				sourceDB, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = sourceDB.Close() })
				_, err = sourceDB.ExecContext(t.Context(), tc.mutation)
				require.NoError(t, err)
				require.NoError(t, sourceDB.Close())

				stats = engine.SyncAllSince(t.Context(), cutoff, nil)
				require.False(t, stats.Aborted)
				assert.Equal(t, 1, stats.Synced)
				assert.Equal(t, members-1, stats.Skipped, "unchanged members still use their fingerprints")
				assertMessageContent(t, database, "openclaw:main:changed", tc.want...)
				assertMessageContent(t, database, "openclaw:main:unchanged-1", "hello", "old response")
				assert.Zero(t, engine.SyncAllSince(t.Context(), cutoff, nil).Synced)
			})
		}
	}
}

func TestSyncAllSinceOpenClawLegacyUsesMtimeCutoff(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main", "sessions", "legacy.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session","version":3,"id":"legacy","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","message":{"role":"user","content":"legacy question"}}
`), 0o600))
	oldTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))
	database := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentOpenClaw: {root}},
		Machine:   "local",
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAllSince(t.Context(), oldTime.Add(time.Hour), nil)
	require.False(t, stats.Aborted)
	assert.Zero(t, stats.Synced)
	session, err := database.GetSessionFull(t.Context(), "openclaw:main:legacy")
	require.NoError(t, err)
	assert.Nil(t, session)

	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assertMessageContent(t, database, "openclaw:main:legacy", "legacy question")
}
