package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncDevinEmptySession(t *testing.T) {
	const (
		emptyID   = "empty-session"
		healthyID = "healthy-session"
		createdAt = 1_700_000_000
	)
	mtime := time.Unix(createdAt+10, 0)
	tests := []struct {
		name           string
		includeHealthy bool
		addMessages    func(t *testing.T, sourceDB *sql.DB, dbPath string) string
		wantContent    []string
	}{
		{name: "empty session alone"},
		{name: "mixed with healthy session", includeHealthy: true},
		{
			name: "message nodes appear",
			addMessages: func(t *testing.T, sourceDB *sql.DB, dbPath string) string {
				t.Helper()
				_, err := sourceDB.ExecContext(t.Context(), `
					INSERT INTO message_nodes (session_id, node_id, chat_message, created_at)
					VALUES (?, 1, '{"role":"user","content":"First node"}', ?)`,
					emptyID, createdAt+1,
				)
				require.NoError(t, err)
				return dbPath
			},
			wantContent: []string{"First node"},
		},
		{
			name: "transcript appears",
			addMessages: func(t *testing.T, _ *sql.DB, dbPath string) string {
				t.Helper()
				path := writeDevinSyncTranscript(t, dbPath, emptyID)
				require.NoError(t, os.Chtimes(path, mtime, mtime))
				return path
			},
			wantContent: []string{"First prompt", "First answer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath, sourceDB := openDevinSyncFixture(t, root)
			t.Cleanup(func() { assert.NoError(t, sourceDB.Close()) })
			insertSession := func(id string) {
				_, err := sourceDB.ExecContext(t.Context(), `
					INSERT INTO sessions (id, title, created_at, last_activity_at)
					VALUES (?, 'New session', ?, ?)`, id, createdAt, createdAt)
				require.NoError(t, err)
			}
			insertSession(emptyID)
			var healthyTranscript string
			if tc.includeHealthy {
				insertSession(healthyID)
				healthyTranscript = writeDevinSyncTranscript(t, dbPath, healthyID)
			}
			require.NoError(t, os.Chtimes(dbPath, mtime, mtime))

			archive := dbtest.OpenTestDB(t)
			engine := NewEngine(t.Context(), archive, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentDevin: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)
			assert.Equal(t, 1, stats.Skipped)
			if tc.includeHealthy {
				assert.Equal(t, 1, stats.Synced)
				assert.Equal(t, "devin:"+healthyID, requireOnlyStoredSession(t, archive).ID)
				assertDevinSyncMessages(t, archive, healthyID, []string{"First prompt", "First answer"})

				// A present session row that becomes empty must not retire its archive.
				require.NoError(t, os.Remove(healthyTranscript))
				stats = engine.SyncAll(t.Context(), nil)
				assert.Zero(t, stats.Failed)
				assert.Zero(t, stats.Tombstoned)
				assert.Equal(t, "devin:"+healthyID, requireOnlyStoredSession(t, archive).ID)
				assertDevinSyncMessages(t, archive, healthyID, []string{"First prompt", "First answer"})
			} else {
				assert.Zero(t, stats.Synced)
				page, err := archive.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
				require.NoError(t, err)
				assert.Empty(t, page.Sessions)
			}

			if tc.addMessages != nil {
				require.NotEmpty(t, engine.SnapshotSkipCache())
				before := devinSourceFingerprint(t, root)
				path := tc.addMessages(t, sourceDB, dbPath)
				// Prove that source content alone can invalidate the empty skip.
				require.NoError(t, os.Chtimes(dbPath, mtime, mtime))
				after := devinSourceFingerprint(t, root)
				assert.Equal(t, before.MTimeNS, after.MTimeNS)
				assert.NotEqual(t, before.Hash, after.Hash)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				stored := requireOnlyStoredSession(t, archive)
				assert.Equal(t, "devin:"+emptyID, stored.ID)
				assert.Equal(t, len(tc.wantContent), stored.MessageCount)
				assertDevinSyncMessages(t, archive, emptyID, tc.wantContent)
			}
		})
	}
}

func writeDevinSyncTranscript(t *testing.T, dbPath, id string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(dbPath), "transcripts")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, id+".json")
	require.NoError(t, os.WriteFile(path, []byte(`{"steps":[
		{"step_id":"step-1","source":"user","message":"First prompt"},
		{"step_id":"step-2","source":"agent","message":"First answer"}
	]}`), 0o644))
	return path
}

func assertDevinSyncMessages(t *testing.T, archive *db.DB, id string, want []string) {
	t.Helper()
	messages, err := archive.GetMessages(t.Context(), "devin:"+id, 0, 10, true)
	require.NoError(t, err)
	var content []string
	for _, m := range messages {
		content = append(content, m.Content)
	}
	assert.Equal(t, want, content)
}
