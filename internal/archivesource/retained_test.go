package archivesource

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank/docbank"
)

func TestRetainedClaudeOriginalAndDirectory(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	ctx := t.Context()
	inputs := t.TempDir()
	projects := filepath.Join(inputs, "claude", "projects")
	original := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "retained question", id).String()
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(original))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "tool-results", "tool.txt"), []byte("tool result"))
	backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	cfg := config.Config{DataDir: seed}
	_, _, _, err = Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1<<20, nil)
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	require.NoError(t, archive.Close())
	found, err := HasRetainedSession(ctx, database, id)
	require.NoError(t, err)
	require.True(t, found)
	tree, primary, source, err := Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1<<20, nil)
	require.NoError(t, err)
	assert.Contains(t, source, "retained:")
	name, err := tree.EntryPath(primary)
	require.NoError(t, err)
	got, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
	companion, err := tree.EntryPath(filepath.ToSlash(filepath.Join(filepath.Dir(primary), id, "tool-results", "tool.txt")))
	require.NoError(t, err)
	got, err = os.ReadFile(companion)
	require.NoError(t, err)
	assert.Equal(t, "tool result", string(got))
	require.NoError(t, tree.Cleanup())
	_, _, _, err = Retained(ctx, cfg, database, id, "wrong-device", id, "claude", "", nil, 1<<20, nil)
	require.Error(t, err)
	_, _, _, err = Retained(ctx, cfg, database, id, sourceDevice, id, "claude", "", nil, 1, nil)
	require.Error(t, err)
	archiveID, err := database.GetArchiveID(ctx)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	owner.NativeSessionID = id
	owner.Source.ArchiveID = archiveID
	err = database.Reader().QueryRowContext(ctx, `SELECT b.device_id,b.root_id,b.source_key,s.manifest_id FROM raw_archive_sessions b JOIN raw_archive_sources s ON s.root_id=b.root_id AND s.source_key=b.source_key WHERE b.session_id=?`, id).Scan(&owner.Source.DeviceID, &owner.Source.RootID, &owner.Source.SourceKey, &owner.ManifestID)
	require.NoError(t, err)
	owner.Attachment = db.ArchiveAttachmentKey{RepositoryID: "archive:" + archiveID, SnapshotID: "directory:" + archiveID}
	require.NoError(t, database.Close())
	directoryCfg := config.Config{ArchiveSources: []config.ArchiveSource{{Directory: seed}}}
	tree, primary, source, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	assert.Equal(t, "directory:"+archiveID, source)
	name, err = tree.EntryPath(primary)
	require.NoError(t, err)
	got, err = os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
	require.NoError(t, tree.Cleanup())
	conflicting := owner
	conflicting.ManifestID = "different-manifest"
	_, _, _, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner, conflicting}, 1<<20, nil)
	require.ErrorContains(t, err, "ambiguous directory source ownership")
	alternate := t.TempDir()
	require.NoError(t, os.CopyFS(alternate, os.DirFS(filepath.Join(seed, rawarchive.Directory, "artifacts"))))
	require.NoError(t, os.Mkdir(filepath.Join(alternate, "application"), 0o700))
	saved, err := os.ReadFile(filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	alternateDB := filepath.Join(alternate, "application", "sessions.db")
	require.NoError(t, os.WriteFile(alternateDB, saved, 0o600))
	// A directory attachment can be accepted before the saved archive is reparsed.
	fixtureDB, err := sql.Open("sqlite3", alternateDB)
	require.NoError(t, err)
	_, err = fixtureDB.ExecContext(ctx, `DELETE FROM raw_archive_sessions`)
	require.NoError(t, err)
	require.NoError(t, fixtureDB.Close())
	before, err := os.ReadFile(alternateDB)
	require.NoError(t, err)
	alternateCfg := config.Config{ArchiveSources: []config.ArchiveSource{{Directory: alternate}}}
	tree, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.NoError(t, err)
	require.NoError(t, tree.Cleanup())
	after, err := os.ReadFile(alternateDB)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = os.Stat(filepath.Join(alternate, "application", "db.write.lock"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	wal := filepath.Join(alternate, "application", "sessions.db-wal")
	require.NoError(t, os.WriteFile(wal, []byte("pending"), 0o600))
	_, _, _, err = Retained(ctx, alternateCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "owner checkpoint")
	require.NoError(t, os.Remove(wal))
	_, _, _, err = Retained(ctx, directoryCfg, nil, id, sourceDevice, id, "claude", "archive://wrong/source", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	require.ErrorContains(t, err, "binding")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, _, err = Retained(cancelled, directoryCfg, nil, id, sourceDevice, id, "claude", "", []db.ArchiveSessionProvenance{owner}, 1<<20, nil)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestRetainedCodexCompressedAndAncestor(t *testing.T) {
	const thread = "22222222-2222-4222-8222-222222222222"
	const parent = "11111111-1111-4111-8111-111111111111"
	ctx := t.Context()
	inputs := t.TempDir()
	home := filepath.Join(inputs, "codex")
	live := filepath.Join(home, "sessions")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl.zst"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := encoder.EncodeAll([]byte(page), nil)
	encoder.Close()
	dbtest.WriteTestFile(t, filepath.Join(live, "2026", "01", "01", pageName), compressed)
	dbtest.WriteTestFile(t, filepath.Join(home, "archived_sessions", parentName), []byte(ancestor))
	backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "codex", Path: home}})
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	var seen []string
	_, _, source, err := Retained(ctx, config.Config{DataDir: seed}, database, "codex:"+thread, sourceDevice, "codex:"+thread, "codex", "", nil, 1<<20, func(_ string, names []string, read func(context.Context, string, int64) ([]byte, error)) error {
		for _, name := range names {
			got, err := read(ctx, name, 1<<20)
			require.NoError(t, err)
			switch filepath.Base(name) {
			case parentName:
				assert.Equal(t, []byte(ancestor), got)
				seen = append(seen, parentName)
			case pageName:
				assert.Equal(t, compressed, got)
				seen = append(seen, pageName)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Contains(t, source, "retained:")
	assert.ElementsMatch(t, []string{parentName, pageName}, seen)
}

func TestRetainedMissingVaultNeedsOwner(t *testing.T) {
	data := t.TempDir()
	_, _, _, err := Retained(t.Context(), config.Config{DataDir: data}, nil, "session", "device", "native", "claude", "", nil, 1024, nil)
	require.ErrorIs(t, err, docbank.ErrVaultNeedsOwner)
	_, err = os.Stat(filepath.Join(data, rawarchive.Directory, "artifacts"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
