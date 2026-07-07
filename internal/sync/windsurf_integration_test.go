package sync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSourceMtimeWindsurfUsesProviderFingerprint(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Windsurf", "User")
	workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
	manifestPath := filepath.Join(workspaceDir, "workspace.json")
	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
	require.NoError(t, os.WriteFile(manifestPath, []byte(`{"folder":"file:///work/demo"}`), 0o644))
	writeSyncWindsurfStateDB(t, dbPath, `{
		"version": 1,
		"sessionId": "mtime-session",
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Question"},
			"response": [{"value": "Answer"}],
			"timestamp": 1710000000000
		}]
	}`)
	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentWindsurf: {root},
		},
		Machine: "devbox",
	})
	defer engine.Close()

	stats := engine.SyncAll(context.Background(), nil)
	require.Equal(t, 1, stats.Synced)
	virtualPath := dbPath + "#mtime-session"
	assert.Equal(t, virtualPath, engine.FindSourceFile("windsurf:mtime-session"))
	before := engine.SourceMtime("windsurf:mtime-session")
	require.NotZero(t, before)

	future := time.Unix(0, before).Add(2 * time.Second)
	require.NoError(t, os.Chtimes(manifestPath, future, future))

	after := engine.SourceMtime("windsurf:mtime-session")
	assert.Greater(t, after, before)
}

func TestProcessFileWindsurfSameMtimeHashChangeReparses(t *testing.T) {
	for _, tt := range []struct {
		name      string
		seedCache bool
		freshSync bool
	}{
		{name: "skip cache", seedCache: true},
		{name: "db freshness", freshSync: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Windsurf", "User")
			workspaceDir := filepath.Join(root, "workspaceStorage", "workspace-hash")
			manifestPath := filepath.Join(workspaceDir, "workspace.json")
			dbPath := filepath.Join(workspaceDir, "state.vscdb")
			require.NoError(t, os.MkdirAll(workspaceDir, 0o755))
			require.NoError(t, os.WriteFile(manifestPath, []byte(`{"folder":"file:///work/demo"}`), 0o644))
			writeSyncWindsurfStateDB(t, dbPath, windsurfSyncPayload("hash-session", "Alpha reply"))
			virtualPath := dbPath + "#hash-session"
			database := dbtest.OpenTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentWindsurf: {root},
				},
				Machine: "devbox",
			})
			defer engine.Close()

			first := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  virtualPath,
				Agent: parser.AgentWindsurf,
			})
			require.NoError(t, first.err)
			require.Len(t, first.results, 1)
			require.Len(t, first.results[0].Messages, 2)
			assert.Equal(t, "Alpha reply", first.results[0].Messages[1].Content)
			initialMtime := first.results[0].Session.File.Mtime
			initialHash := first.results[0].Session.File.Hash
			require.NotZero(t, initialMtime)
			require.NotEmpty(t, initialHash)
			writeSyncWindsurfResult(t, engine, first)

			infoBefore, err := os.Stat(dbPath)
			require.NoError(t, err)
			updateSyncWindsurfStateDB(t, dbPath, windsurfSyncPayload("hash-session", "Bravo reply"))
			initialTime := time.Unix(0, initialMtime)
			require.NoError(t, os.Chtimes(dbPath, initialTime, initialTime))
			infoAfter, err := os.Stat(dbPath)
			require.NoError(t, err)
			require.Equal(t, infoBefore.Size(), infoAfter.Size(),
				"test must keep size stable so hash is the only freshness signal")

			if tt.seedCache {
				engine.cacheSkip(first.cacheKey, initialMtime)
			}
			if tt.freshSync {
				engine.Close()
				engine = NewEngine(database, EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentWindsurf: {root},
					},
					Machine: "devbox",
				})
				defer engine.Close()
			}

			second := engine.processFile(context.Background(), parser.DiscoveredFile{
				Path:  virtualPath,
				Agent: parser.AgentWindsurf,
			})
			require.NoError(t, second.err)
			assert.False(t, second.skip)
			require.Len(t, second.results, 1)
			require.Len(t, second.results[0].Messages, 2)
			assert.Equal(t, "Bravo reply", second.results[0].Messages[1].Content)
			assert.NotEqual(t, initialHash, second.results[0].Session.File.Hash)
		})
	}
}

func writeSyncWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`)
	require.NoError(t, err)
	_, err = conn.Exec(
		`INSERT INTO ItemTable (key, value) VALUES (?, ?)`,
		"workbench.panel.aichat.view.aichat.chatdata",
		payload,
	)
	require.NoError(t, err)
}

func updateSyncWindsurfStateDB(t *testing.T, dbPath, payload string) {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(
		`UPDATE ItemTable SET value = ? WHERE key = ?`,
		payload,
		"workbench.panel.aichat.view.aichat.chatdata",
	)
	require.NoError(t, err)
}

func writeSyncWindsurfResult(t *testing.T, engine *Engine, result processResult) {
	t.Helper()
	require.Len(t, result.results, 1)
	written, _, failed := engine.writeBatch(
		[]pendingWrite{{
			sess:         result.results[0].Session,
			msgs:         result.results[0].Messages,
			usageEvents:  result.results[0].UsageEvents,
			forceReplace: result.forceReplace,
		}},
		syncWriteDefault,
		false,
	)
	require.Equal(t, 0, failed)
	require.Equal(t, 1, written)
}

func windsurfSyncPayload(sessionID, assistant string) string {
	return `{
		"version": 1,
		"sessionId": "` + sessionID + `",
		"requests": [{
			"requestId": "request-1",
			"message": {"text": "Question"},
			"response": [{"value": "` + assistant + `"}],
			"timestamp": 1710000000000
		}]
	}`
}
