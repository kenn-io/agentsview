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
	"go.kenn.io/agentsview/internal/db"
)

func TestCopyPreservesAssetReferences(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	assetDir := filepath.Join(source, "assets")
	require.NoError(t, os.MkdirAll(assetDir, 0o700))
	ref, _, err := assets.Put(assetDir, "image/png", []byte("canonical image"))
	require.NoError(t, err)
	content := "![image](" + ref + ")"
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "example", Agent: "claude", Project: "example"}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "example", Role: "user", Content: content}}))
	require.NoError(t, database.Close())
	dest := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}}))
	got, err := os.ReadFile(filepath.Join(dest, "assets", strings.TrimPrefix(ref, "asset://")))
	require.NoError(t, err)
	assert.Equal(t, "canonical image", string(got))
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	var copiedContent string
	require.NoError(t, copyDB.Reader().QueryRow(ctx, "SELECT content FROM messages WHERE session_id = ?", "example").Scan(&copiedContent))
	assert.Equal(t, content, copiedContent)
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
	err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	require.ErrorIs(t, copyDB.RequireSourceSync(ctx), db.ErrArchiveOnly)
	retained, err := copyDB.GetSessionFull(ctx, "session")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Nil(t, retained.DeletedAt)
	err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.Error(t, err)
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
}

func TestCopyRefusesIncompleteSources(t *testing.T) {
	source := t.TempDir()
	parent := t.TempDir()
	destination := filepath.Join(parent, "copy")
	err := Copy(t.Context(), CopyOptions{DataDir: source, Destination: destination, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.Error(t, err)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	assert.Empty(t, entries)
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
	usage, err := database.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	require.Equal(t, 330, usage.Totals.InputTokens)
	require.Equal(t, 100, usage.Totals.OutputTokens)
	id, err := database.GetDatabaseID(ctx)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	caches := db.UsageCacheFiles(path, id)
	cache := caches[len(caches)-1]
	original, err := sql.Open("sqlite3", cache)
	require.NoError(t, err)
	var paths int
	require.NoError(t, original.QueryRowContext(ctx, "SELECT count(*) FROM usage_rollup_installs WHERE project = ?", "/work/api").Scan(&paths))
	require.Positive(t, paths)
	require.NoError(t, original.Close())
	dest := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}}))
	copied, err := sql.Open("sqlite3", filepath.Join(dest, filepath.Base(cache)))
	require.NoError(t, err)
	var input, output int
	require.NoError(t, copied.QueryRowContext(ctx, `SELECT sum(input_tokens), sum(output_tokens) FROM usage_facts`).Scan(&input, &output))
	assert.Equal(t, 330, input)
	assert.Equal(t, 100, output)
	require.NoError(t, copied.QueryRowContext(ctx, "SELECT count(*) FROM usage_rollup_installs WHERE project = ?", "/work/api").Scan(&paths))
	assert.Zero(t, paths)
	require.NoError(t, copied.Close())
}

func TestCopyPrefersCurrentUsageCache(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	path := filepath.Join(source, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	started := "2026-08-10T09:00:00Z"
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "session", Agent: "claude", Project: "project", StartedAt: &started}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "session", Role: "assistant", Timestamp: started, Model: "model-a", TokenUsage: []byte(`{"input_tokens":330}`)}}))
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
