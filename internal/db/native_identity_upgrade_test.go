package db_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestNativeIdentityUpgradePreservesArchiveSessions(t *testing.T) {
	const native = "11111111-1111-4111-8111-111111111111"
	providers := []parser.AgentType{parser.AgentCodex, parser.AgentTraeX, parser.AgentAugureCode, parser.AgentEvener}
	require.ElementsMatch(t, providers, db.ScopedDataVersionProviders(158, 159))
	for _, agent := range providers {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "rollout-2026-01-01T00-00-00-"+native+".jsonl")
			body := `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"` + native + `"}}` + "\n" +
				`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
			if agent == parser.AgentEvener {
				path = filepath.Join(root, "sessions", native+".transcript.jsonl")
				body = `{"kind":"header","format_version":2,"session_id":"` + native + `","created_at":"2026-01-01T00:00:00Z"}` + "\n" +
					`{"kind":"entry","seq":0,"turn":{"kind":"USER_INPUT","timestamp":"2026-01-01T00:00:01Z","message":{"content":[{"kind":"text","text":"hello"}]}}}` + "\n"
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			database := dbtest.OpenTestDB(t)
			cfg := agentsync.EngineConfig{Machine: "fixture", AgentDirs: map[parser.AgentType][]string{agent: {root}}, Ephemeral: true, DisableFilesystemProjectDiscovery: true}
			engine := agentsync.NewEngine(t.Context(), database, cfg)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()
			require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET source_session_id='',data_version=158; PRAGMA user_version=158`)
				return err
			}))
			archivePath := database.Path()
			require.NoError(t, database.Close())
			reopened, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			needsSync, fullRebuild, err := db.ArchiveResyncState(t.Context(), archivePath)
			require.NoError(t, err)
			assert.True(t, needsSync)
			assert.False(t, fullRebuild)
			engine = agentsync.NewEngine(t.Context(), reopened, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err := reopened.GetSessionFull(t.Context(), string(agent)+":"+native)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, native, session.SourceSessionID)
			assert.Equal(t, &path, session.FilePath)
			messages, err := reopened.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "hello", messages[0].Content)
			var sessions int
			require.NoError(t, reopened.Reader().QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions))
			assert.Equal(t, 1, sessions)
		})
	}
}
