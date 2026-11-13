package archivesource

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSelectedInventoryAuthorityAndScope(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		covered, supplemental int
		wantError             bool
	}{
		{"covered wins over supplemental", 1, 1, false},
		{"single supplemental", 0, 1, false},
		{"covered conflict", 2, 0, true},
		{"supplemental conflict", 0, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := sql.Open("sqlite3", ":memory:")
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.ExecContext(t.Context(), `CREATE TABLE raw_archive_files(root_id TEXT,path TEXT,sha256 TEXT,size INTEGER,mod_time_ns INTEGER,covered INTEGER)`)
			require.NoError(t, err)
			insert := func(root, name, hash string, covered bool) {
				_, err := conn.ExecContext(t.Context(), `INSERT INTO raw_archive_files VALUES(?,?,?,?,?,?)`, root, name, hash, 1, 1, covered)
				require.NoError(t, err)
			}
			primary := "project/id.jsonl"
			insert("root", primary, strings.Repeat("a", 64), true)
			insert("root", primary, strings.Repeat("9", 64), true)
			bound := rawsync.Entry{Path: primary, Type: "file", Length: 1, Objects: []rawsync.ObjectRef{{SHA256: strings.Repeat("a", 64), Length: 1}}}
			for i := range tc.covered {
				insert("root", "project/id/subagents/meta.json", strings.Repeat(string(rune('b'+i)), 64), true)
			}
			for i := range tc.supplemental {
				insert("root", "project/id/subagents/meta.json", strings.Repeat(string(rune('d'+i)), 64), false)
			}
			insert("root", "project/sibling/subagents/agent.jsonl", strings.Repeat("e", 64), true)
			insert("other", "project/id/subagents/other.json", strings.Repeat("f", 64), true)
			entries, err := selectedInventory(t.Context(), conn, "root", bound, "project/id/")
			if tc.wantError {
				require.ErrorContains(t, err, "conflicting saved file versions")
				return
			}
			require.NoError(t, err)
			require.Len(t, entries, 2)
			assert.Equal(t, primary, entries[0].Path)
			want := "d"
			if tc.covered != 0 {
				want = "b"
			}
			assert.Equal(t, strings.Repeat(want, 64), entries[1].Objects[0].SHA256)
			assert.Equal(t, bound.Objects, entries[0].Objects)
			if tc.name != "covered wins over supplemental" {
				return
			}
			_, err = conn.ExecContext(t.Context(), `DELETE FROM raw_archive_files WHERE path=? AND sha256=?`, primary, strings.Repeat("a", 64))
			require.NoError(t, err)
			_, err = selectedInventory(t.Context(), conn, "root", bound, "project/id/")
			require.ErrorContains(t, err, "saved transcript does not match selected archive")
			insert("root", primary, strings.Repeat("a", 64), true)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO raw_archive_files VALUES(?,?,?,?,?,?)`, "root", "project/id/subagents/corrupt.json", strings.Repeat("a", 64), 1, 1, 2)
			require.NoError(t, err)
			_, err = selectedInventory(t.Context(), conn, "root", bound, "project/id/")
			require.ErrorContains(t, err, "couldn't convert 2 into type bool")
		})
	}
}

func TestMaterializeSnapshotCaptureLayouts(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	for _, layout := range []string{"home", "projects", "flat"} {
		t.Run(layout, func(t *testing.T) {
			inputs := t.TempDir()
			root := filepath.Join(inputs, "home")
			projects := filepath.Join(root, "projects")
			if layout == "flat" {
				projects = root
			}
			primary := filepath.Join(projects, "project", id+".jsonl")
			content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "selected", id).String()
			dbtest.WriteTestFile(t, primary, []byte(content))
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "tool-results", "tool.txt"), []byte("tool"))
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "subagents", "meta.json"), []byte("metadata"))
			child := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:01:00Z", "selected child", "agent-reviewer").String()
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "subagents", "agent-reviewer.jsonl"), []byte(child))
			otherChild := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "sibling child", "agent-reviewer").String()
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", "00000000-0000-4000-8000-000000000000", "subagents", "agent-reviewer.jsonl"), []byte(otherChild))
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", "sibling", "subagents", "meta.json"), []byte("sibling"))
			selectedRoot := root
			if layout == "projects" {
				selectedRoot = projects
			}
			source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: selectedRoot}})
			index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
			require.NoError(t, err)
			defer index.Close()
			sources, err := index.Sources(t.Context(), "", 100)
			require.NoError(t, err)
			var owner db.ArchiveSessionProvenance
			grouped := false
			for _, item := range sources {
				// Capture groups same-agent continuations into one child manifest.
				if strings.Contains(string(item.CanonicalJSON), id+"/subagents/agent-reviewer.jsonl") && strings.Contains(string(item.CanonicalJSON), "00000000-0000-4000-8000-000000000000/subagents/agent-reviewer.jsonl") {
					grouped = true
					assert.NotContains(t, item.SourceKey, id+"/subagents/")
				}
				if item.Root.Provider == "claude" && strings.HasSuffix(item.SourceKey, id+".jsonl") {
					owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: id}
				}
			}
			require.NotEmpty(t, owner.ManifestID)
			require.True(t, grouped, "capture must group both parent directories")
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			t.Setenv("TMP", scratch)
			t.Setenv("TEMP", scratch)
			tree, selected, _, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
			require.NoError(t, err)
			for rel, want := range map[string]string{selected: content, strings.TrimSuffix(selected, ".jsonl") + "/tool-results/tool.txt": "tool", strings.TrimSuffix(selected, ".jsonl") + "/subagents/meta.json": "metadata", strings.TrimSuffix(selected, ".jsonl") + "/subagents/agent-reviewer.jsonl": child} {
				name, err := tree.EntryPath(rel)
				require.NoError(t, err)
				data, err := os.ReadFile(name)
				require.NoError(t, err)
				assert.Equal(t, want, string(data))
			}
			_, err = tree.EntryPath(strings.TrimSuffix(selected, id+".jsonl") + "00000000-0000-4000-8000-000000000000/subagents/agent-reviewer.jsonl")
			require.Error(t, err)
			require.NoError(t, tree.Cleanup())
			if layout == "home" {
				refused, path, attachment, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 1)
				require.ErrorIs(t, err, rawderive.ErrSizeLimit)
				assert.Nil(t, refused)
				assert.Equal(t, selected, path)
				assert.Equal(t, owner.Attachment.SnapshotID, attachment)
			}
			files, err := os.ReadDir(scratch)
			require.NoError(t, err)
			assert.Empty(t, files)
		})
	}
}

func TestReadCodexSnapshotUsesAcceptedOriginal(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	inputs := t.TempDir()
	root := filepath.Join(inputs, "codex", "sessions")
	name := "rollout-2026-01-01T00-00-00-" + id + ".jsonl"
	parent := "rollout-20240115-11111111-1111-4111-8111-111111111111.jsonl"
	unrelated := "rollout-20240115-22222222-2222-4222-8222-222222222222.jsonl"
	conflicted := "rollout-20240115-33333333-3333-4333-8333-333333333333.jsonl"
	unsafeName := "../rollout-20240115-44444444-4444-4444-8444-444444444444.jsonl"
	original := testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").AddCodexMessage("2026-01-01T00:00:01Z", "user", "saved Codex question").String()
	dbtest.WriteTestFile(t, filepath.Join(root, name), []byte(original))
	dbtest.WriteTestFile(t, filepath.Join(filepath.Dir(root), "archived_sessions", parent), []byte("parent bytes"))
	dbtest.WriteTestFile(t, filepath.Join(filepath.Dir(root), "archived_sessions", conflicted), []byte("conflict bytes"))
	other := filepath.Join(inputs, "other", "sessions")
	dbtest.WriteTestFile(t, filepath.Join(other, unrelated), []byte("unrelated bytes"))
	archiveRoot := filepath.Join(filepath.Dir(root), "archived_sessions")
	roots := []rawarchive.RootSpec{{Provider: "codex", Path: root}, {Provider: "codex", Path: archiveRoot}, {Provider: "codex", Path: other}}
	source := backupSourceRoots(t, inputs, roots)
	changed := testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").AddCodexMessage("2026-01-01T00:00:01Z", "user", "later question").String()
	dbtest.WriteTestFile(t, filepath.Join(root, name), []byte(changed))
	dbtest.WriteTestFile(t, filepath.Join(filepath.Dir(root), "archived_sessions", conflicted), []byte("later conflict bytes"))
	ctx := t.Context()
	capture := filepath.Join(inputs, "capture-next")
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	_, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: filepath.Join(inputs, "application"), Destination: capture, IdentityFrom: filepath.Join(inputs, "capture", "capture.json"), Roots: roots, Settings: settings})
	require.NoError(t, err)
	spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(capture, "capture.json"))
	require.NoError(t, err)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(inputs, "seed", "sessions.db"))
	require.NoError(t, err)
	databaseOpen := true
	t.Cleanup(func() {
		if databaseOpen {
			assert.NoError(t, database.Close())
		}
	})
	archive, err := rawarchive.Open(ctx, database, filepath.Join(inputs, "seed"), nil)
	require.NoError(t, err)
	archiveOpen := true
	t.Cleanup(func() {
		if archiveOpen {
			assert.NoError(t, archive.Close())
		}
	})
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	selectedRoot := ""
	// Model two accepted manifests retaining different versions of this page.
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		var originalHome string
		var countSources int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.source_key=? AND s.original_path=? AND r.provider=?`, "codex:"+id, filepath.Join(root, name), "codex").Scan(&countSources)
		if err != nil {
			return err
		}
		require.Equal(t, 1, countSources)
		err = tx.QueryRowContext(ctx, `SELECT s.root_id,r.original_path FROM raw_archive_sources s JOIN raw_archive_roots r ON r.id=s.root_id WHERE s.source_key=? AND s.original_path=? AND r.provider=?`, "codex:"+id, filepath.Join(root, name), "codex").Scan(&selectedRoot, &originalHome)
		if err != nil {
			return err
		}
		var sameHome int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM raw_archive_roots WHERE provider='codex' AND original_path=?`, originalHome).Scan(&sameHome)
		if err != nil {
			return err
		}
		require.Equal(t, 2, sameHome)
		// A sibling root's inventory must never be substituted into this source.
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns,covered)
			SELECT r.id,?,f.sha256,f.size,f.mod_time_ns,0 FROM raw_archive_roots r JOIN raw_archive_files f ON f.root_id=? AND f.path LIKE ?
			WHERE r.original_path=? AND r.id<>? LIMIT 1`, "sibling-only.jsonl", selectedRoot, "%"+parent, originalHome, selectedRoot)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE raw_archive_files SET covered=1 WHERE path LIKE ?`, "%"+conflicted)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 4, count)
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_archive_files(root_id,path,sha256,size,mod_time_ns,covered)
			SELECT root_id,?,sha256,size,mod_time_ns,0 FROM raw_archive_files WHERE root_id=? AND path LIKE ? LIMIT 1`, unsafeName, selectedRoot, "%"+parent)
		return err
	}))
	report, err := archive.Backup(ctx, source.Repository, settings, "test")
	require.NoError(t, err)
	source.Snapshot = report.SnapshotID
	require.NoError(t, archive.Close())
	archiveOpen = false
	require.NoError(t, database.Close())
	databaseOpen = false
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	matches := 0
	for _, item := range sources {
		if item.Root.Provider == "codex" && item.RootID == selectedRoot && item.SourceKey == "codex:"+id && item.OriginalPath == filepath.Join(root, name) {
			matches++
			owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: "codex:" + id}
		}
	}
	require.Equal(t, 1, matches)
	require.NotEmpty(t, owner.ManifestID)
	selected, err := ReadCodexSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, 512<<20, func(_ string, names, _ []string, read func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		require.Len(t, names, 4)
		for _, candidate := range names {
			if path.Base(candidate) == conflicted || candidate == unsafeName {
				continue
			}
			data, err := read(t.Context(), candidate, 512<<20)
			require.NoError(t, err)
			switch path.Base(candidate) {
			case name:
				assert.Contains(t, string(data), "saved Codex question")
				assert.NotContains(t, string(data), "later question")
			case parent:
				assert.Equal(t, "parent bytes", string(data))
			default:
				require.FailNowf(t, "unexpected candidate", "%q", candidate)
			}
		}
		for _, candidate := range names {
			if path.Base(candidate) == conflicted {
				_, err := read(t.Context(), candidate, 512<<20)
				require.ErrorContains(t, err, "conflicting saved file versions")
			}
		}
		_, err := read(t.Context(), unsafeName, 512<<20)
		require.ErrorContains(t, err, "entry path: invalid raw logical path")
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, source.Snapshot, selected)
}

func TestReadCodexSnapshotPreservesCompressedLegacyBytes(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	inputs := t.TempDir()
	root := filepath.Join(inputs, "custom")
	name := "rollout-20240115-" + id + ".jsonl.zst"
	data := []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2024-01-15T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").AddCodexMessage("2024-01-15T00:00:01Z", "user", "saved question").String())
	encoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := encoder.EncodeAll(data, nil)
	encoder.Close()
	dbtest.WriteTestFile(t, filepath.Join(root, name), compressed)
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "codex", Path: root}})
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	owner := db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(sources[0]), NativeSessionID: "codex:" + id}
	_, err = ReadCodexSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, 512<<20, func(_ string, names, _ []string, read func(context.Context, string, int64) ([]byte, error), _ CodexAncestry) error {
		require.Len(t, names, 1)
		assert.Equal(t, name, filepath.Base(names[0]))
		got, err := read(t.Context(), names[0], 512<<20)
		require.NoError(t, err)
		assert.Equal(t, compressed, got)
		return nil
	})
	require.NoError(t, err)
}

func TestMaterializeSnapshotEquivalentCandidates(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	inputs := t.TempDir()
	projects := filepath.Join(inputs, "claude", "projects")
	dbtest.WriteTestFile(t, filepath.Join(projects, "project-a", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "saved Claude question", id).String()))
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	for _, item := range sources {
		if item.Root.Provider == "claude" {
			owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: id}
		}
	}
	require.NotEmpty(t, owner.ManifestID)
	newer := invalidIndexSnapshot(t, source, inputs, "inventory version")
	require.NoError(t, os.RemoveAll(inputs))
	broken := source
	broken.Name = "broken"
	broken.Repository = filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.CopyFS(broken.Repository, os.DirFS(source.Repository)))
	manifestPath := filepath.Join(broken.Repository, "snapshots", source.Snapshot+".mvmanifest")
	require.NoError(t, os.Chmod(manifestPath, 0o600))
	require.NoError(t, os.WriteFile(manifestPath, []byte("corrupt"), 0o600))
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	assertScratchEmpty := func(t *testing.T) {
		t.Helper()
		files, err := os.ReadDir(scratch)
		require.NoError(t, err)
		assert.Empty(t, files)
	}
	ctx := t.Context()
	// An unavailable equivalent membership does not hide the verified snapshot.
	other := owner
	other.Attachment.SnapshotID = "unconfigured"
	tree, primary, chosen, err := MaterializeSnapshot(ctx, []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{other, owner}, "claude", 512<<20)
	require.NoError(t, err)
	assert.Equal(t, owner.Attachment.SnapshotID, chosen)
	name, err := tree.EntryPath(primary)
	require.NoError(t, err)
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Contains(t, string(data), "saved Claude question")
	require.NoError(t, tree.Cleanup())
	for _, tc := range []struct {
		name   string
		change func(*db.ArchiveSessionProvenance)
		want   string
	}{
		{"archive", func(p *db.ArchiveSessionProvenance) { p.Source.ArchiveID = "wrong" }, "saved archive identity differs"},
		{"root", func(p *db.ArchiveSessionProvenance) { p.Source.RootID = "wrong" }, "saved source ownership"},
		{"manifest", func(p *db.ArchiveSessionProvenance) { p.ManifestID = "wrong" }, "saved source ownership"},
		{"unbound unknown native id", func(p *db.ArchiveSessionProvenance) { p.NativeSessionID = "wrong" }, "saved transcript source differs"},
		{"repository", func(p *db.ArchiveSessionProvenance) { p.Attachment.RepositoryID = "wrong" }, "owning snapshot is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := owner
			tc.change(&modified)
			refused, path, attachment, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{modified}, "claude", 512<<20)
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, refused)
			assert.Empty(t, path)
			assert.Empty(t, attachment)
			assertScratchEmpty(t)
		})
	}
	wrongRepo := owner
	wrongRepo.Attachment.RepositoryID = "different-repository"
	missing := owner
	missing.Attachment.SnapshotID = "20000101T000000Z-00000000000000000000000000000000"
	missingSource := source
	missingSource.Name, missingSource.Snapshot = "missing", missing.Attachment.SnapshotID
	tree, _, chosen, err = MaterializeSnapshot(t.Context(), []config.ArchiveSource{missingSource, source}, []db.ArchiveSessionProvenance{wrongRepo, missing, owner}, "claude", 512<<20)
	require.NoError(t, err)
	assert.Equal(t, owner.Attachment.SnapshotID, chosen)
	require.NoError(t, tree.Cleanup())
	t.Run("newer inventory", func(t *testing.T) {
		modified := owner
		modified.Attachment.SnapshotID = newer.Snapshot
		refused, path, attachment, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{newer}, []db.ArchiveSessionProvenance{modified}, "claude", 512<<20)
		require.ErrorContains(t, err, "unsupported recovery inventory version")
		assert.Nil(t, refused)
		assert.Empty(t, path)
		assert.Empty(t, attachment)
		assertScratchEmpty(t)
	})
	t.Run("corrupt snapshot", func(t *testing.T) {
		refused, path, attachment, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{broken, source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
		require.ErrorContains(t, err, "parsing snapshot")
		assert.Nil(t, refused)
		assert.Empty(t, path)
		assert.Empty(t, attachment)
		assertScratchEmpty(t)
	})
	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		refused, path, attachment, err := MaterializeSnapshot(ctx, []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, refused)
		assert.Empty(t, path)
		assert.Empty(t, attachment)
		assertScratchEmpty(t)
	})
	t.Run("directory capture", func(t *testing.T) {
		directory := owner
		directory.Attachment.SnapshotID = "directory:fixture"
		refused, path, attachment, err := MaterializeSnapshot(t.Context(), nil, []db.ArchiveSessionProvenance{directory}, "claude", 512<<20)
		require.ErrorContains(t, err, "owning snapshot is not configured")
		assert.Nil(t, refused)
		assert.Empty(t, path)
		assert.Empty(t, attachment)
		assertScratchEmpty(t)
	})
}

func TestSnapshotPrimaryWindowsCoordinates(t *testing.T) {
	entry := rawsync.Entry{Path: "project/id.jsonl", Type: "file"}
	bound, primary, err := snapshotPrimaryPath(`C:\agent\projects\project\id.jsonl`, `C:\agent\projects\project\id.jsonl`, `C:\agent`, "id", []rawsync.Entry{entry})
	require.NoError(t, err)
	assert.Equal(t, entry, bound)
	assert.Equal(t, "projects/project/id.jsonl", primary)
	_, _, err = snapshotPrimaryPath(`D:\elsewhere\project\id.jsonl`, `D:\elsewhere\project\id.jsonl`, `C:\agent`, "id", []rawsync.Entry{entry})
	require.ErrorContains(t, err, "outside capture root")

	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	codexEntry := rawsync.Entry{Path: "sessions/rollout-20260101-" + id + ".jsonl", Type: "file"}
	original := `C:\agent\sessions\rollout-20260101-` + id + `.jsonl`
	_, primary, err = snapshotPrimaryPath("codex:"+id, original, `C:\agent`, "codex:"+id, []rawsync.Entry{codexEntry})
	require.NoError(t, err)
	assert.Equal(t, codexEntry.Path, primary)
	_, _, err = snapshotPrimaryPath("codex:11111111-1111-4111-8111-111111111111", original, `C:\agent`, "codex:"+id, []rawsync.Entry{codexEntry})
	require.ErrorContains(t, err, "source differs")
	_, _, err = snapshotPrimaryPath("codex:"+id, `D:\outside\`+codexEntry.Path, `C:\agent`, "codex:"+id, []rawsync.Entry{codexEntry})
	require.ErrorContains(t, err, "outside capture root")
}

func TestMaterializeSnapshotUsesAcceptedPrimaryAfterSiblingCapture(t *testing.T) {
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const siblingID = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	inputs := t.TempDir()
	root := filepath.Join(inputs, "claude", "projects")
	primary := filepath.Join(root, "project", id+".jsonl")
	original := fmt.Sprintf(`{"type":"user","uuid":"root-message","parentUuid":null,"timestamp":"2026-01-01T00:00:00Z","sessionId":%q,"message":{"content":"original"}}`+"\n", id)
	dbtest.WriteTestFile(t, primary, []byte(original))
	dbtest.WriteTestFile(t, filepath.Join(root, "project", id, "tool-results", "tool.txt"), []byte("original tool"))
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: root}})
	changed := original + fmt.Sprintf(`{"type":"user","uuid":"next-message","parentUuid":"root-message","timestamp":"2026-01-01T00:00:01Z","sessionId":%q,"message":{"content":"changed"}}`+"\n", id)
	require.NoError(t, os.WriteFile(primary, []byte(changed), 0o600))
	sibling := fmt.Sprintf(`{"type":"user","uuid":"root-message","parentUuid":null,"timestamp":"2026-01-01T00:00:00Z","sessionId":%q,"sessionKind":"bg","message":{"content":"original"}}`+"\n", siblingID)
	dbtest.WriteTestFile(t, filepath.Join(root, "project", siblingID+".jsonl"), []byte(sibling))
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	capture := filepath.Join(inputs, "capture-next")
	_, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: filepath.Join(inputs, "application"), Destination: capture, IdentityFrom: filepath.Join(inputs, "capture", "capture.json"), Roots: []rawarchive.RootSpec{{Provider: "claude", Path: root}}, Settings: settings})
	require.NoError(t, err)
	spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(capture, "capture.json"))
	require.NoError(t, err)
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	accepted, err := database.ListRawArchiveSources(ctx, "", 100)
	require.NoError(t, err)
	var siblingManifest []byte
	for _, item := range accepted {
		if strings.HasSuffix(item.SourceKey, siblingID+".jsonl") {
			siblingManifest = item.CanonicalJSON
		}
	}
	require.Contains(t, string(siblingManifest), id+".jsonl", "the accepted background sibling must retain the primary")
	files, err := database.ListRawArchiveFiles(ctx, 0, 100)
	require.NoError(t, err)
	rel, err := filepath.Rel(filepath.Dir(root), primary)
	require.NoError(t, err)
	covered := 0
	for _, file := range files {
		if file.Path == filepath.ToSlash(rel) && file.Covered {
			covered++
		}
	}
	require.Equal(t, 2, covered, "the later accepted sibling must cover the changed primary")
	report, err := archive.Backup(ctx, source.Repository, settings, "test")
	require.NoError(t, err)
	source.Snapshot = report.SnapshotID
	index, err := OpenIndex(ctx, source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	sources, err := index.Sources(ctx, "", 100)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	for _, item := range sources {
		if strings.HasSuffix(item.SourceKey, id+".jsonl") {
			owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: id}
		}
	}
	require.NotEmpty(t, owner.ManifestID)
	tree, selected, _, err := MaterializeSnapshot(ctx, []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
	require.NoError(t, err)
	defer tree.Cleanup()
	name, err := tree.EntryPath(selected)
	require.NoError(t, err)
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))
	tool, err := tree.EntryPath(strings.TrimSuffix(selected, ".jsonl") + "/tool-results/tool.txt")
	require.NoError(t, err)
	data, err = os.ReadFile(tool)
	require.NoError(t, err)
	assert.Equal(t, "original tool", string(data))
}

func TestMaterializeSnapshotSavedSessionBinding(t *testing.T) {
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	for _, conflict := range []bool{false, true} {
		name := "matching"
		if conflict {
			name = "conflicting"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			inputs := t.TempDir()
			projects := filepath.Join(inputs, "claude", "projects")
			primary := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "selected", id).String()
			dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(primary))
			dbtest.WriteTestFile(t, filepath.Join(projects, "other-project", id+".jsonl"), []byte(primary))
			source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
			seed := filepath.Join(inputs, "seed")
			database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			roots, err := database.ListRawArchiveRoots(ctx)
			require.NoError(t, err)
			sources, err := database.ListRawArchiveSources(ctx, "", 100)
			require.NoError(t, err)
			var root db.RawArchiveRoot
			var selected, alternate db.RawArchiveSource
			for _, item := range sources {
				if strings.HasSuffix(item.SourceKey, string(filepath.Separator)+filepath.Join("project", id+".jsonl")) {
					selected = item
				}
				if strings.HasSuffix(item.SourceKey, string(filepath.Separator)+filepath.Join("other-project", id+".jsonl")) {
					alternate = item
				}
			}
			require.NotEmpty(t, selected.ManifestID)
			require.NotEmpty(t, alternate.ManifestID)
			for _, item := range roots {
				if item.ID == selected.RootID {
					root = item
				}
			}
			require.NotEmpty(t, root.ID)
			key := selected.SourceKey
			if conflict {
				key = alternate.SourceKey
			}
			_, err = database.BindRawArchiveSession(ctx, root, key, id, root.DeviceID+"~"+id)
			require.NoError(t, err)
			archive, err := rawarchive.Open(ctx, database, seed, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			report, err := archive.Backup(ctx, source.Repository, rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}, "binding")
			require.NoError(t, err)
			source.Snapshot = report.SnapshotID
			index, err := OpenIndex(ctx, source, t.TempDir(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, index.Close()) })
			items, err := index.Sources(ctx, "", 100)
			require.NoError(t, err)
			var owner db.ArchiveSessionProvenance
			for _, item := range items {
				if item.ManifestID == selected.ManifestID {
					owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: id}
				}
			}
			require.NotEmpty(t, owner.ManifestID)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			t.Setenv("TMP", scratch)
			t.Setenv("TEMP", scratch)
			tree, path, _, err := MaterializeSnapshot(ctx, []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
			if conflict {
				require.ErrorContains(t, err, "saved session ownership differs")
				assert.Nil(t, tree)
				assert.Empty(t, path)
			} else {
				require.NoError(t, err)
				file, err := tree.EntryPath(path)
				require.NoError(t, err)
				data, err := os.ReadFile(file)
				require.NoError(t, err)
				assert.Equal(t, primary, string(data))
				require.NoError(t, tree.Cleanup())
			}
			remaining, err := os.ReadDir(scratch)
			require.NoError(t, err)
			assert.Empty(t, remaining)
		})
	}
}

func TestCodexCompanionsRespectRootAndExactAliases(t *testing.T) {
	const rootID = "11111111-1111-4111-8111-111111111111"
	const parentID = "22222222-2222-4222-8222-222222222222"
	rootName := "rollout-2026-01-01T00-00-00-" + rootID + ".jsonl"
	parentName := "rollout-2024-01-01T00-00-00-" + parentID + ".jsonl"
	ref := rawsync.ObjectRef{SHA256: strings.Repeat("a", 64), Length: 4}
	other := rawsync.ObjectRef{SHA256: strings.Repeat("b", 64), Length: 4}
	entries := []rawsync.Entry{
		{Path: "root/" + rootName, Length: 4, Objects: []rawsync.ObjectRef{ref}},
		{Path: "first/" + parentName, Length: 4, Objects: []rawsync.ObjectRef{ref}},
		{Path: "alias/" + parentName, Length: 4, Objects: []rawsync.ObjectRef{ref}},
		{Path: "different/" + parentName, Length: 4, Objects: []rawsync.ObjectRef{other}},
	}
	byPath := map[string]rawsync.Entry{rootName: entries[0]}
	ancestors := codexCompanions(entries, []string{rootName}, byPath)
	require.Equal(t, []string{"first/" + parentName, "different/" + parentName}, ancestors)
	assert.Equal(t, entries[1], byPath[ancestors[0]])
	assert.Equal(t, entries[3], byPath[ancestors[1]])
}

type codexCatalogStore struct {
	tenant string
	ref    rawsync.ObjectRef
	data   []byte
}

func (s codexCatalogStore) CopyObject(_ context.Context, tenant string, ref rawsync.ObjectRef, dst io.Writer) (rawsync.ObjectInfo, error) {
	if tenant != s.tenant || ref != s.ref {
		return rawsync.ObjectInfo{}, rawsync.ErrNotFound
	}
	_, err := dst.Write(s.data)
	return rawsync.ObjectInfo{Ref: ref}, err
}

func TestReadCodexCatalogExposesManifestCompanion(t *testing.T) {
	const (
		gID = "33333333-3333-4333-8333-333333333333"
		pID = "11111111-1111-4111-8111-111111111111"
		cID = "22222222-2222-4222-8222-222222222222"
		rID = "44444444-4444-4444-8444-444444444444"
	)
	for _, scenario := range []string{"newer inventory", "revert skips parent", "ordinary"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			inputs := t.TempDir()
			child, parent, grandparent := filepath.Join(inputs, "child"), filepath.Join(inputs, "parent"), filepath.Join(inputs, "grandparent")
			gName := "rollout-2023-01-15T00-00-00-" + gID + ".jsonl"
			pName := "rollout-2024-01-15T00-00-00-" + pID + ".jsonl"
			oldName := "rollout-2025-01-01T00-00-00-" + cID + ".jsonl"
			key := cID
			if scenario != "ordinary" {
				key += "_" + rID
			}
			cName := "rollout-2026-01-01T00-00-00-" + key + ".jsonl"
			gPath := filepath.Join(grandparent, "sessions", "2023", "01", "15", gName)
			pPath := filepath.Join(parent, "sessions", "2024", "01", "15", pName)
			oldPath := filepath.Join(child, "sessions", "2025", "01", "01", oldName)
			cPath := filepath.Join(child, "sessions", "2026", "01", "01", cName)
			page := func(id, stamp, message string, ordinal int, fields map[string]any) string {
				fields["history_mode"] = "paginated"
				withOrdinal := func(line string, n int) string {
					return strings.TrimSuffix(line, "}") + fmt.Sprintf(`,"ordinal":%d}`, n)
				}
				return testjsonl.JoinJSONL(withOrdinal(testjsonl.CodexSessionMetaWithFieldsJSON(id, "/work", "codex_cli_rs", stamp, fields), ordinal), withOrdinal(testjsonl.CodexMsgJSON("user", message, stamp), ordinal+1))
			}
			base := func(id string, ordinal, size int) map[string]any {
				return map[string]any{"thread_id": id, "end_ordinal_exclusive": ordinal, "end_byte_offset": size}
			}
			g := page(gID, "2023-01-15T00:00:00Z", "grandparent", 0, map[string]any{})
			p := page(pID, "2024-01-15T00:00:00Z", "parent", 2, map[string]any{"forked_from_id": gID, "forked_from_ordinal_exclusive": 2, "history_base": base(gID, 2, len(g))})
			if scenario == "newer inventory" {
				p = page(pID, "2024-01-15T00:00:00Z", "parent", 0, map[string]any{})
			}
			cutoff, history := 4, base(pID, 4, len(p))
			if scenario != "ordinary" {
				cutoff, history = 2, base(gID, 2, len(g))
			}
			if scenario == "newer inventory" {
				history = base(pID, 2, len(p))
			}
			message := "child"
			if scenario == "newer inventory" {
				message = "new child"
			}
			c := page(cID, "2026-01-01T00:00:00Z", message, cutoff, map[string]any{"forked_from_id": pID, "forked_from_ordinal_exclusive": cutoff, "history_base": history})
			old := page(cID, "2025-01-01T00:00:00Z", "old child", 2, map[string]any{"forked_from_id": pID, "forked_from_ordinal_exclusive": 2, "history_base": base(pID, 2, len(p))})
			roots := []rawarchive.RootSpec{{Provider: "codex", Path: child}, {Provider: "codex", Path: parent}, {Provider: "codex", Path: grandparent}}
			if scenario == "newer inventory" {
				dbtest.WriteTestFile(t, oldPath, []byte(old))
				backupSourceRoots(t, inputs, roots[:1])
			} else {
				dbtest.WriteTestFile(t, gPath, []byte(g))
				dbtest.WriteTestFile(t, pPath, []byte(p))
				dbtest.WriteTestFile(t, cPath, []byte(c))
				backupSourceRoots(t, inputs, roots)
			}
			seed := filepath.Join(inputs, "seed")
			database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			var firstRootID, firstManifestID string
			if scenario == "newer inventory" {
				firstRoots, err := database.ListRawArchiveRoots(ctx)
				require.NoError(t, err)
				var matching []db.RawArchiveRoot
				for _, root := range firstRoots {
					if root.Provider == "codex" && root.OriginalPath == child {
						matching = append(matching, root)
					}
				}
				require.Len(t, matching, 1)
				firstRootID = matching[0].ID
				firstSource, err := database.RawArchiveSourceByKey(ctx, firstRootID, "codex:"+cID)
				require.NoError(t, err)
				require.NotNil(t, firstSource)
				firstManifestID = firstSource.ManifestID
				dbtest.WriteTestFile(t, pPath, []byte(p))
				dbtest.WriteTestFile(t, cPath, []byte(c))
				capture := filepath.Join(inputs, "second")
				settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
				_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: filepath.Join(inputs, "application"), Destination: capture, IdentityFrom: filepath.Join(inputs, "capture", "capture.json"), Roots: roots[:2], Settings: settings})
				require.NoError(t, err)
				spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(capture, "capture.json"))
				require.NoError(t, err)
				archive, err := rawarchive.Open(ctx, database, seed, nil)
				require.NoError(t, err)
				_, err = archive.Import(ctx, spec)
				require.NoError(t, err)
				require.NoError(t, archive.Close())
			}
			archiveID, err := database.GetArchiveID(ctx)
			require.NoError(t, err)
			identity, err := rawsync.NewAuthIdentity(archiveID, sourceDevice)
			require.NoError(t, err)
			savedRoots, err := database.ListRawArchiveRoots(ctx)
			require.NoError(t, err)
			rootsByPath := make(map[string]db.RawArchiveRoot)
			for _, root := range savedRoots {
				rootsByPath[root.OriginalPath] = root
			}
			childRoot, parentRoot := rootsByPath[child], rootsByPath[parent]
			require.NotEmpty(t, childRoot.ID)
			require.NotEmpty(t, parentRoot.ID)
			require.Equal(t, sourceDevice, childRoot.DeviceID)
			require.Equal(t, sourceDevice, parentRoot.DeviceID)
			require.NotEqual(t, childRoot.ID, parentRoot.ID)
			if scenario != "newer inventory" {
				grandparentRoot := rootsByPath[grandparent]
				require.NotEmpty(t, grandparentRoot.ID)
				require.Equal(t, sourceDevice, grandparentRoot.DeviceID)
				require.NotEqual(t, grandparentRoot.ID, childRoot.ID)
				require.NotEqual(t, grandparentRoot.ID, parentRoot.ID)
			}
			selectedKey := key
			if scenario == "newer inventory" {
				selectedKey = cID
			}
			source, err := database.RawArchiveSourceByKey(ctx, childRoot.ID, "codex:"+selectedKey)
			require.NoError(t, err)
			require.NotNil(t, source)
			manifest, err := rawsync.ParseCanonicalManifest(identity, source.ManifestID, source.CanonicalJSON, rawsync.DefaultManifestLimits())
			require.NoError(t, err)
			if scenario == "newer inventory" {
				require.Equal(t, firstRootID, childRoot.ID)
				require.Equal(t, firstManifestID, source.ManifestID)
				require.Equal(t, oldPath, source.OriginalPath)
				require.Len(t, manifest.Manifest.Entries, 1)
				require.Equal(t, oldName, filepath.Base(manifest.Manifest.Entries[0].Path))
			} else {
				require.Equal(t, cPath, source.OriginalPath)
				require.Len(t, manifest.Manifest.Entries, 2)
			}
			checkEntry := func(m rawsync.CanonicalManifest, name, content string) {
				t.Helper()
				var matches []rawsync.Entry
				for _, entry := range m.Manifest.Entries {
					if filepath.Base(entry.Path) == name {
						matches = append(matches, entry)
					}
				}
				require.Len(t, matches, 1)
				require.Equal(t, int64(len(content)), matches[0].Length)
				require.Equal(t, []rawsync.ObjectRef{{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content))), Length: int64(len(content))}}, matches[0].Objects)
			}
			companionManifest := manifest
			if scenario == "newer inventory" {
				checkEntry(manifest, oldName, old)
				newSource, err := database.RawArchiveSourceByKey(ctx, childRoot.ID, "codex:"+key)
				require.NoError(t, err)
				require.NotNil(t, newSource)
				require.Equal(t, cPath, newSource.OriginalPath)
				companionManifest, err = rawsync.ParseCanonicalManifest(identity, newSource.ManifestID, newSource.CanonicalJSON, rawsync.DefaultManifestLimits())
				require.NoError(t, err)
			}
			checkEntry(companionManifest, cName, c)
			checkEntry(companionManifest, pName, p)
			parentSource, err := database.RawArchiveSourceByKey(ctx, parentRoot.ID, "codex:"+pID)
			require.NoError(t, err)
			require.NotNil(t, parentSource)
			require.Equal(t, pPath, parentSource.OriginalPath)
			if scenario != "newer inventory" {
				parentManifest, err := rawsync.ParseCanonicalManifest(identity, parentSource.ManifestID, parentSource.CanonicalJSON, rawsync.DefaultManifestLimits())
				require.NoError(t, err)
				checkEntry(parentManifest, pName, p)
				checkEntry(parentManifest, gName, g)
				grandparentSource, err := database.RawArchiveSourceByKey(ctx, rootsByPath[grandparent].ID, "codex:"+gID)
				require.NoError(t, err)
				require.NotNil(t, grandparentSource)
				require.Equal(t, gPath, grandparentSource.OriginalPath)
			}
			files, err := database.ListRawArchiveFiles(ctx, 0, 1000)
			require.NoError(t, err)
			inventory := []struct {
				root          db.RawArchiveRoot
				name, content string
			}{{childRoot, cName, c}, {parentRoot, pName, p}}
			if scenario != "newer inventory" {
				inventory = append(inventory, struct {
					root          db.RawArchiveRoot
					name, content string
				}{rootsByPath[grandparent], gName, g})
			}
			for _, item := range inventory {
				hash := fmt.Sprintf("%x", sha256.Sum256([]byte(item.content)))
				count := 0
				for _, file := range files {
					if file.SHA256 == hash {
						count++
						require.Equal(t, item.root.ID, file.RootID)
						require.Equal(t, item.name, filepath.Base(file.Path))
						require.Equal(t, int64(len(item.content)), file.Size)
					}
				}
				require.Equal(t, 1, count)
			}
			bound, primary, err := snapshotPrimaryPath(source.SourceKey, source.OriginalPath, childRoot.OriginalPath, "codex:"+selectedKey, manifest.Manifest.Entries)
			require.NoError(t, err)
			bound.Path = primary
			conn, err := sql.Open("sqlite3", filepath.Join(seed, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, conn.Close()) })
			wantID, want := gID, g
			if scenario == "newer inventory" {
				wantID, want = pID, p
			}
			ref := rawsync.ObjectRef{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(want))), Length: int64(len(want))}
			called := false
			err = readCodexCatalog(ctx, conn, childRoot.ID, child, bound, identity, manifest, codexCatalogStore{archiveID, ref, []byte(want)}, 1<<20, func(root string, names, ancestors []string, read func(context.Context, string, int64) ([]byte, error), ancestry CodexAncestry) error {
				called = true
				assert.Equal(t, child, root)
				require.NotNil(t, ancestry)
				from := ""
				for _, name := range append(names, ancestors...) {
					if filepath.Base(name) == cName {
						from = name
					}
				}
				require.NotEmpty(t, from)
				if scenario == "newer inventory" {
					require.Contains(t, names, from)
				}
				if scenario == "ordinary" {
					for _, name := range ancestors {
						if filepath.Base(name) == pName {
							from = name
						}
					}
					require.Equal(t, pName, filepath.Base(from))
				}
				tokens, err := ancestry(ctx, from, wantID)
				require.NoError(t, err)
				require.Len(t, tokens, 1)
				got, err := read(ctx, tokens[0], 1<<20)
				require.NoError(t, err)
				assert.Equal(t, want, string(got))
				return nil
			})
			require.NoError(t, err)
			require.True(t, called)
		})
	}
}
