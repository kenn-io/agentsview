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
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/db"
)

func TestCopyPreservesAssetReferences(t *testing.T) {
	for _, state := range []string{"valid", "missing", "corrupt"} {
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
			path := filepath.Join(assetDir, strings.TrimPrefix(ref, "asset://"))
			switch state {
			case "missing":
				require.NoError(t, os.Remove(path))
			case "corrupt":
				require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
			}
			parent := t.TempDir()
			dest := filepath.Join(parent, "copy")
			_, err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
			if state != "valid" {
				require.Error(t, err)
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
	_, err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	var project, cwd string
	var filePath sql.NullString
	require.NoError(t, copyDB.Reader().QueryRow(ctx, `SELECT project,cwd,file_path FROM sessions WHERE id='session'`).Scan(&project, &cwd, &filePath))
	assert.Equal(t, db.CopiedProjectLabel(source), project)
	assert.Empty(t, cwd)
	assert.False(t, filePath.Valid)
	detached, err := copyDB.HostPathsDetached(ctx)
	require.NoError(t, err)
	assert.True(t, detached)
	require.ErrorIs(t, copyDB.RequireSourceSync(ctx), db.ErrArchiveOnly)
	retained, err := copyDB.GetSessionFull(ctx, "session")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Nil(t, retained.DeletedAt)
	_, err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	original, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, original.Close()) })
	require.NoError(t, original.Reader().QueryRow(ctx, `SELECT project,cwd,file_path FROM sessions WHERE id='session'`).Scan(&project, &cwd, &filePath))
	assert.Equal(t, source, project)
	assert.Equal(t, source, cwd)
	assert.Equal(t, path, filePath.String)
}

func TestCopyOmitsRecoveryVaults(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	for _, dir := range []string{source, filepath.Join(source, Directory)} {
		repository, err := artifact.OpenRepository(ctx, dir)
		require.NoError(t, err)
		require.NoError(t, repository.Close())
	}
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "example", Agent: "claude", Project: "example"}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "example", Role: "user", Content: "retained history"}}))
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		for _, query := range []string{
			`INSERT INTO raw_archive_roots VALUES('root','device','machine','claude','/original/root','configured')`,
			`INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns) VALUES('root','file','hash',1,1)`,
			`INSERT INTO raw_archive_sources(manifest_id,root_id,source_key,original_path,canonical_json) VALUES('manifest','root','source','/original/file',x'7b7d')`,
			`INSERT INTO raw_archive_devices VALUES('device',x'7b7d')`,
			`INSERT INTO raw_archive_root_aliases VALUES('root','/original/alias')`,
			`INSERT INTO raw_archive_sessions VALUES('device','claude','native','example','root','source')`,
			`INSERT INTO archive_attachments(repository_id,snapshot_id,archive_id) VALUES('repo','snapshot','archive')`,
			`INSERT INTO archive_source_bindings VALUES('archive','device','root','source','manifest')`,
			`INSERT INTO archive_attachment_sources(repository_id,snapshot_id,archive_id,device_id,root_id,source_key,target) VALUES('repo','snapshot','archive','device','root','source','target')`,
			`INSERT INTO archive_session_provenance VALUES('example','archive','device','root','source','manifest','native')`,
		} {
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, database.EnableArchiveOnly(ctx))
	require.NoError(t, database.Close())
	vaultPaths := []string{filepath.Join(source, "artifacts", "docbank.db"), filepath.Join(source, Directory, "artifacts", "docbank.db")}
	vaultBytes := make([][]byte, len(vaultPaths))
	for i, path := range vaultPaths {
		vaultBytes[i], err = os.ReadFile(path)
		require.NoError(t, err)
	}
	dest := filepath.Join(t.TempDir(), "copy")
	report, err := Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	assert.Contains(t, report.Excluded, "raw-archive: recovery vault")
	assert.Contains(t, report.Excluded, "artifacts: recovery vault")
	for _, dir := range []string{"artifacts", Directory} {
		assert.NoDirExists(t, filepath.Join(dest, dir))
	}
	for i, path := range vaultPaths {
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, vaultBytes[i], got)
	}
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	require.ErrorIs(t, copyDB.RequireSourceSync(ctx), db.ErrArchiveOnly)
	session, err := copyDB.GetSessionFull(ctx, "example")
	require.NoError(t, err)
	require.NotNil(t, session)
	var content string
	require.NoError(t, copyDB.Reader().QueryRow(ctx, `SELECT content FROM messages WHERE session_id='example'`).Scan(&content))
	assert.Equal(t, "retained history", content)
	for _, table := range []string{"raw_archive_roots", "raw_archive_files", "raw_archive_sources", "raw_archive_devices", "raw_archive_root_aliases", "raw_archive_sessions", "archive_attachments", "archive_attachment_sources", "archive_source_bindings", "archive_session_provenance"} {
		var count int
		require.NoError(t, copyDB.Reader().QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		assert.Zero(t, count, table)
	}
	original, err := db.OpenIsolatedContext(ctx, filepath.Join(source, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, original.Close()) })
	require.ErrorIs(t, original.RequireSourceSync(ctx), db.ErrArchiveOnly)
	var originalPath string
	require.NoError(t, original.Reader().QueryRow(ctx, `SELECT original_path FROM raw_archive_sources`).Scan(&originalPath))
	assert.Equal(t, "/original/file", originalPath)
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
			_, err := Copy(ctx, CopyOptions{DataDir: source, Destination: destination, Settings: RecoverySettings{LocalMachineName: "source"}})
			require.Error(t, err)
			entries, err := os.ReadDir(parent)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestCopyExcludesUnreadableUsageCache(t *testing.T) {
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
	require.NoError(t, os.WriteFile(cache, []byte("not a database"), 0o600))
	dest := filepath.Join(t.TempDir(), "copy")
	report, err := Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	assert.Contains(t, report.Excluded, filepath.Base(cache)+": unreadable cache")
	assert.NoFileExists(t, filepath.Join(dest, filepath.Base(cache)))
}

func TestCopySelectsUsageCaches(t *testing.T) {
	ctx := t.Context()
	source := t.TempDir()
	path := filepath.Join(source, "sessions.db")
	database, err := db.OpenIsolatedContext(ctx, path)
	require.NoError(t, err)
	started := "2026-08-10T09:00:00Z"
	require.NoError(t, database.UpsertSession(ctx, db.Session{ID: "session", Agent: "claude", Project: "/work/api", StartedAt: &started}))
	require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: "session", Role: "assistant", Timestamp: started, Model: "model-a", TokenUsage: []byte(`{"input_tokens":330,"output_tokens":100}`)}}))
	filter := db.UsageFilter{Timezone: "UTC"}
	want, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, 330, want.Totals.InputTokens)
	require.Equal(t, 100, want.Totals.OutputTokens)
	id, err := database.GetDatabaseID(ctx)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	caches := db.UsageCacheFiles(path, id)
	cache := caches[len(caches)-1]
	unrelated := filepath.Join(source, "usage-cache-v999-unrelated.db")
	require.NoError(t, os.WriteFile(unrelated, []byte("unrelated"), 0o600))
	dest := filepath.Join(t.TempDir(), "copy")
	_, err = Copy(ctx, CopyOptions{DataDir: source, Destination: dest, Settings: RecoverySettings{LocalMachineName: "source"}})
	require.NoError(t, err)
	copied, err := sql.Open("sqlite3", filepath.Join(dest, filepath.Base(cache)))
	require.NoError(t, err)
	var input, output int
	require.NoError(t, copied.QueryRowContext(ctx, `SELECT sum(input_tokens), sum(output_tokens) FROM usage_facts`).Scan(&input, &output))
	assert.Equal(t, 330, input)
	assert.Equal(t, 100, output)
	require.NoError(t, copied.Close())
	assert.NoFileExists(t, filepath.Join(dest, filepath.Base(unrelated)))
	copyDB, err := db.OpenIsolatedContext(ctx, filepath.Join(dest, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copyDB.Close()) })
	got, err := copyDB.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want.Totals, got.Totals)
	got, err = copyDB.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC", Project: db.CopiedProjectLabel("/work/api")})
	require.NoError(t, err)
	assert.Equal(t, want.Totals.InputTokens, got.Totals.InputTokens)
	assert.Equal(t, want.Totals.OutputTokens, got.Totals.OutputTokens)
}
