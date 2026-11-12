package archivesource

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			require.ErrorContains(t, err, "saved transcript does not match snapshot")
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
		{"archive", func(p *db.ArchiveSessionProvenance) { p.Source.ArchiveID = "wrong" }, "snapshot archive identity differs"},
		{"root", func(p *db.ArchiveSessionProvenance) { p.Source.RootID = "wrong" }, "snapshot source ownership"},
		{"manifest", func(p *db.ArchiveSessionProvenance) { p.ManifestID = "wrong" }, "snapshot source ownership"},
		{"session", func(p *db.ArchiveSessionProvenance) { p.NativeSessionID = "wrong" }, "snapshot session ownership differs"},
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
		require.ErrorContains(t, err, "directory captures cannot")
		assert.Nil(t, refused)
		assert.Empty(t, path)
		assert.Empty(t, attachment)
		assertScratchEmpty(t)
	})
}

func TestSnapshotPrimaryWindowsCoordinates(t *testing.T) {
	entry := rawsync.Entry{Path: "project/id.jsonl", Type: "file"}
	bound, primary, err := snapshotPrimaryPath(`C:\agent\projects\project\id.jsonl`, `C:\agent`, "id", []rawsync.Entry{entry})
	require.NoError(t, err)
	assert.Equal(t, entry, bound)
	assert.Equal(t, "projects/project/id.jsonl", primary)
	_, _, err = snapshotPrimaryPath(`D:\elsewhere\project\id.jsonl`, `C:\agent`, "id", []rawsync.Entry{entry})
	require.ErrorContains(t, err, "outside capture root")
}

func TestMaterializeSnapshotUsesAcceptedPrimaryAfterSiblingCapture(t *testing.T) {
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const siblingID = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	inputs := t.TempDir()
	root := filepath.Join(inputs, "claude", "projects")
	primary := filepath.Join(root, "project", id+".jsonl")
	original := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "original", id).String()
	dbtest.WriteTestFile(t, primary, []byte(original))
	dbtest.WriteTestFile(t, filepath.Join(root, "project", id, "tool-results", "tool.txt"), []byte("original tool"))
	source := backupSourceRoots(t, inputs, []rawarchive.RootSpec{{Provider: "claude", Path: root}})
	changed := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "changed", id).String()
	require.NoError(t, os.WriteFile(primary, []byte(changed), 0o600))
	dbtest.WriteTestFile(t, filepath.Join(root, "project", siblingID+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:01:00Z", "sibling", siblingID).String()))
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	capture := filepath.Join(inputs, "capture-next")
	_, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: filepath.Join(inputs, "application"), Destination: capture, IdentityFrom: filepath.Join(inputs, "capture", "capture.json"), Roots: []rawarchive.RootSpec{{Provider: "claude", Path: root}}, Settings: settings})
	require.NoError(t, err)
	spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(capture, "capture.json"))
	require.NoError(t, err)
	seed := filepath.Join(inputs, "seed")
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	files, err := database.ListRawArchiveFiles(ctx, 0, 100)
	require.NoError(t, err)
	covered := 0
	for _, file := range files {
		if file.Path == "project/"+id+".jsonl" && file.Covered {
			covered++
		}
	}
	require.Equal(t, 2, covered, "the later accepted sibling must cover the changed primary")
	report, err := archive.Backup(ctx, source.Repository, settings, "test")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
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
