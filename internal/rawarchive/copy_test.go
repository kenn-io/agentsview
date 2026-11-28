package rawarchive

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestCopyPreservesAssetReferences(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	assetDir := filepath.Join(source, "assets")
	require.NoError(t, os.MkdirAll(assetDir, 0o700))
	ref, _, err := assets.Put(assetDir, "image/png", []byte("canonical image"))
	require.NoError(t, err)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dest := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}}))
	got, err := os.ReadFile(filepath.Join(dest, "assets", strings.TrimPrefix(ref, "asset://")))
	require.NoError(t, err)
	assert.Equal(t, "canonical image", string(got))
}

func TestCopyDetachedArchive(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	path := filepath.Join(source, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,project,cwd,file_path) VALUES('session',?,?,?)`, source, source, path)
		return err
	}))
	require.NoError(t, database.Close())
	dest := filepath.Join(t.TempDir(), "copy")
	prices := map[string]config.CustomModelRate{
		"model": {InputMicrodollarsPerMTok: 1, OutputMicrodollarsPerMTok: 2, CacheCreationMicrodollarsPerMTok: 3, CacheCreation1hMicrodollarsPerMTok: 4, CacheReadMicrodollarsPerMTok: 5},
		"alias": {PriceAs: "gpt-4o"},
	}
	err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}, CustomModelPricing: prices})
	require.NoError(t, err)
	t.Setenv("AGENTSVIEW_DATA_DIR", dest)
	cfg, err := config.LoadCapture()
	require.NoError(t, err)
	assert.Equal(t, prices, cfg.CustomModelPricing)
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	require.ErrorIs(t, copyDB.RequireSourceSync(ctx), db.ErrArchiveOnly)
	retained, err := copyDB.GetSessionFull(ctx, "session")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Nil(t, retained.DeletedAt)
	detached, err := copyDB.HostPathsDetached(ctx)
	require.NoError(t, err)
	assert.True(t, detached)
	_, err = os.Stat(filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	original, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, original.Close()) })
	var project, cwd string
	var filePath sql.NullString
	require.NoError(t, original.Reader().QueryRow(ctx, `SELECT project,cwd,file_path FROM sessions WHERE id='session'`).Scan(&project, &cwd, &filePath))
	assert.Equal(t, source, project)
	assert.Equal(t, source, cwd)
	assert.Equal(t, path, filePath.String)

	for _, kind := range []string{"missing", "empty", "unrelated", "existing destination"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, parent := t.TempDir(), t.TempDir()
			path := filepath.Join(source, "sessions.db")
			dest := filepath.Join(parent, "copy")
			switch kind {
			case "empty":
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			case "unrelated":
				foreign, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = foreign.ExecContext(ctx, "CREATE TABLE unrelated(id INTEGER)")
				require.NoError(t, err)
				require.NoError(t, foreign.Close())
			case "existing destination":
				require.NoError(t, os.Mkdir(dest, 0o700))
			}
			err := Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
			require.Error(t, err)
			if kind == "empty" || kind == "unrelated" {
				require.ErrorContains(t, err, "source is not an AgentsView archive")
			}
			entries, err := os.ReadDir(parent)
			require.NoError(t, err)
			if kind == "existing destination" {
				require.Len(t, entries, 1)
				assert.Equal(t, "copy", entries[0].Name())
				entries, err = os.ReadDir(dest)
				require.NoError(t, err)
			}
			assert.Empty(t, entries)
		})
	}
}

func TestCopyUsageCache(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	path := filepath.Join(source, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	started := "2026-08-10T09:00:00Z"
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "session", Agent: "claude", Project: "/work/api", StartedAt: &started}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "session", Role: "assistant", Timestamp: started, Model: "model-a", TokenUsage: []byte(`{"input_tokens":330,"output_tokens":100}`)}}))
	_, err = database.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	id, err := database.GetDatabaseID(ctx)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	caches := db.UsageCacheFiles(path, id)
	require.Greater(t, len(caches), 1)
	require.NoError(t, os.WriteFile(caches[0], []byte("corrupt obsolete cache"), 0o600))
	dest := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}}))
	assert.FileExists(t, filepath.Join(dest, filepath.Base(caches[len(caches)-1])))
	assert.NoFileExists(t, filepath.Join(dest, filepath.Base(caches[0])))
}

func TestCopyExplainsUnreadableUsageCache(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	path := filepath.Join(source, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	id, err := database.GetDatabaseID(ctx)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	caches := db.UsageCacheFiles(path, id)
	cache := caches[len(caches)-1]
	require.NoError(t, os.WriteFile(cache, []byte("corrupt cache"), 0o600))
	destination := filepath.Join(t.TempDir(), "copy")
	err = Copy(ctx, CopyOptions{DataDir: source, Destination: destination, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.ErrorContains(t, err, cache)
	require.ErrorContains(t, err, "usage cache")
	require.ErrorContains(t, err, "move")
	assert.NoDirExists(t, destination)
	require.NoError(t, os.Rename(cache, cache+".bad"))
	require.NoError(t, Copy(ctx, CopyOptions{DataDir: source, Destination: destination, Settings: RecoverySettings{LocalMachineName: "source"}}))
}
