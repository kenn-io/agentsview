package rawarchive

import (
	"context"
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
	for _, state := range []string{"valid", "missing directory"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			source := t.TempDir()
			assetDir := filepath.Join(source, "assets")
			require.NoError(t, os.MkdirAll(assetDir, 0o700))
			ref, _, err := assets.Put(assetDir, "image/png", []byte("canonical image"))
			require.NoError(t, err)
			jpeg, _, err := assets.Put(assetDir, "image/jpeg", []byte("jpeg image"))
			require.NoError(t, err)
			jpegName := strings.TrimPrefix(strings.TrimSuffix(jpeg, ".jpg")+".jpeg", "asset://")
			require.NoError(t, os.Rename(filepath.Join(assetDir, strings.TrimPrefix(jpeg, "asset://")), filepath.Join(assetDir, jpegName)))
			files := map[string][]byte{strings.TrimPrefix(ref, "asset://"): []byte("canonical image"), jpegName: []byte("jpeg image"), "legacy.png": []byte("legacy image"), "partial.png.tmp-interrupted": []byte("partial")}
			for _, name := range []string{"legacy.png", "partial.png.tmp-interrupted"} {
				require.NoError(t, os.WriteFile(filepath.Join(assetDir, name), files[name], 0o600))
			}
			content := "![one](" + ref + ") ![two](asset://" + jpegName + ") ![three](asset://legacy.png)"
			database, err := db.OpenIsolatedContext(ctx, filepath.Join(source, "sessions.db"))
			require.NoError(t, err)
			require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "example", Agent: "claude", Project: "example"}))
			require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "example", Role: "user", Content: content}}))
			require.NoError(t, database.Close())
			if state == "missing directory" {
				for name := range files {
					require.NoError(t, os.Remove(filepath.Join(assetDir, name)))
				}
				require.NoError(t, os.Remove(assetDir))
			}
			parent := t.TempDir()
			dest := filepath.Join(parent, "copy")
			err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
			if state != "valid" {
				require.ErrorContains(t, err, "asset")
				entries, err := os.ReadDir(parent)
				require.NoError(t, err)
				assert.Empty(t, entries)
				return
			}
			require.NoError(t, err)
			for name, want := range files {
				for _, dir := range []string{assetDir, filepath.Join(dest, "assets")} {
					got, err := os.ReadFile(filepath.Join(dir, name))
					require.NoError(t, err)
					assert.Equal(t, want, got)
				}
			}
			copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
			var got string
			require.NoError(t, copyDB.Reader().QueryRow(ctx, "SELECT content FROM messages WHERE session_id = ?", "example").Scan(&got))
			assert.Equal(t, content, got)
		})
	}
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
	for _, kind := range []string{"missing database", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			if kind != "missing database" {
				database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(source, "sessions.db"))
				require.NoError(t, err)
				require.NoError(t, database.Close())
			}
			ctx := t.Context()
			if kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			parent := t.TempDir()
			destination := filepath.Join(parent, "copy")
			err := Copy(ctx, CopyOptions{DataDir: source, Destination: destination, Settings: RecoverySettings{LocalMachineName: "source"}})
			require.Error(t, err)
			entries, err := os.ReadDir(parent)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestCopyUsageCache(t *testing.T) {
	for _, state := range []string{"unreadable", "valid"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			source := t.TempDir()
			path := filepath.Join(source, "sessions.db")
			database, err := db.OpenIsolatedContext(ctx, path)
			require.NoError(t, err)
			if state == "valid" {
				started := "2026-08-10T09:00:00Z"
				require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "session", Agent: "claude", Project: "/work/api", StartedAt: &started}))
				require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "session", Role: "assistant", Timestamp: started, Model: "model-a", TokenUsage: []byte(`{"input_tokens":330,"output_tokens":100}`)}}))
				usage, err := database.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
				require.NoError(t, err)
				require.Equal(t, 330, usage.Totals.InputTokens)
				require.Equal(t, 100, usage.Totals.OutputTokens)
			}
			id, err := database.GetDatabaseID(ctx)
			require.NoError(t, err)
			require.NoError(t, database.Close())
			caches := db.UsageCacheFiles(path, id)
			cache := caches[len(caches)-1]
			if state == "unreadable" {
				require.NoError(t, os.WriteFile(cache, []byte("not a database"), 0o600))
			} else {
				original, err := sql.Open("sqlite3", cache)
				require.NoError(t, err)
				var paths int
				require.NoError(t, original.QueryRowContext(ctx, "SELECT count(*) FROM usage_rollup_installs WHERE project = ?", "/work/api").Scan(&paths))
				require.Positive(t, paths)
				require.NoError(t, original.Close())
				require.NoError(t, os.WriteFile(filepath.Join(source, "usage-cache-v999-unrelated.db"), []byte("unrelated"), 0o600))
			}
			dest := filepath.Join(t.TempDir(), "copy")
			err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
			if state == "unreadable" {
				require.Error(t, err)
				assert.NoDirExists(t, dest)
				return
			}
			require.NoError(t, err)
			copied, err := sql.Open("sqlite3", filepath.Join(dest, filepath.Base(cache)))
			require.NoError(t, err)
			var input, output int
			require.NoError(t, copied.QueryRowContext(ctx, `SELECT sum(input_tokens), sum(output_tokens) FROM usage_facts`).Scan(&input, &output))
			assert.Equal(t, 330, input)
			assert.Equal(t, 100, output)
			var paths int
			require.NoError(t, copied.QueryRowContext(ctx, "SELECT count(*) FROM usage_rollup_installs WHERE project = ?", "/work/api").Scan(&paths))
			assert.Zero(t, paths)
			require.NoError(t, copied.Close())
			assert.NoFileExists(t, filepath.Join(dest, "usage-cache-v999-unrelated.db"))
		})
	}
}
