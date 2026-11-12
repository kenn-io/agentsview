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
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestMaterializeSnapshotRejectsNewerInventory(t *testing.T) {
	source, inputs := archiveFixture(t)
	broken := invalidIndexSnapshot(t, source, inputs, "inventory version")
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	defer index.Close()
	// The unsupported version is refused before source ownership is read.
	owner := db.ArchiveSessionProvenance{ArchiveSourceProvenance: db.ArchiveSourceProvenance{
		Attachment: db.ArchiveAttachmentKey{RepositoryID: index.Info.RepositoryID, SnapshotID: broken.Snapshot},
	}}
	_, _, _, err = MaterializeSnapshot(t.Context(), []config.ArchiveSource{broken}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
	require.ErrorContains(t, err, "unsupported recovery inventory version")
}

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
			_, err = conn.Exec(`CREATE TABLE raw_archive_files(root_id TEXT,path TEXT,sha256 TEXT,size INTEGER,mod_time_ns INTEGER,covered INTEGER)`)
			require.NoError(t, err)
			insert := func(root, name, hash string, covered bool) {
				_, err := conn.Exec(`INSERT INTO raw_archive_files VALUES(?,?,?,?,?,?)`, root, name, hash, 1, 1, covered)
				require.NoError(t, err)
			}
			primary := "project/id.jsonl"
			insert("root", primary, strings.Repeat("a", 64), true)
			for i := 0; i < tc.covered; i++ {
				insert("root", "project/id/subagents/meta.json", strings.Repeat(string(rune('b'+i)), 64), true)
			}
			for i := 0; i < tc.supplemental; i++ {
				insert("root", "project/id/subagents/meta.json", strings.Repeat(string(rune('d'+i)), 64), false)
			}
			insert("root", "project/sibling/subagents/agent.jsonl", strings.Repeat("e", 64), true)
			insert("other", "project/id/subagents/other.json", strings.Repeat("f", 64), true)
			entries, err := selectedInventory(t.Context(), conn, "root", primary, "project/id/")
			if tc.wantError {
				require.ErrorContains(t, err, "ambiguous")
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
			_, err = conn.Exec(`INSERT INTO raw_archive_files VALUES(?,?,?,?,?,?)`, "root", "project/id/subagents/corrupt.json", strings.Repeat("a", 64), 1, 1, 2)
			require.NoError(t, err)
			_, err = selectedInventory(t.Context(), conn, "root", primary, "project/id/")
			require.Error(t, err)
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
			_, err = tree.EntryPath(strings.TrimSuffix(selected, id+".jsonl") + "sibling/subagents/meta.json")
			require.Error(t, err)
			_, err = tree.EntryPath(strings.TrimSuffix(selected, id+".jsonl") + "00000000-0000-4000-8000-000000000000/subagents/agent-reviewer.jsonl")
			require.Error(t, err)
			require.NoError(t, tree.Cleanup())
			_, _, _, err = MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 1)
			require.Error(t, err)
			files, err := os.ReadDir(scratch)
			require.NoError(t, err)
			assert.Empty(t, files)
		})
	}
}

func TestMaterializeSnapshotEquivalentCandidates(t *testing.T) {
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	index, err := OpenIndex(t.Context(), source, t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	sources, err := index.Sources(t.Context(), "", 10)
	require.NoError(t, err)
	var owner db.ArchiveSessionProvenance
	for _, item := range sources {
		if item.Root.Provider == "claude" {
			owner = db.ArchiveSessionProvenance{ArchiveSourceProvenance: index.provenance(item), NativeSessionID: "019eb791-cf7d-75c1-8439-9ed74c122e02"}
		}
	}
	require.NotEmpty(t, owner.ManifestID)
	ctx := t.Context()
	// Duplicate configuration and an unavailable equivalent membership do not
	// make the configured, verified snapshot ambiguous.
	other := owner
	other.Attachment.SnapshotID = "unconfigured"
	second := source
	second.Name = "second"
	duplicate := []config.ArchiveSource{source, second}
	tree, primary, chosen, err := MaterializeSnapshot(ctx, duplicate, []db.ArchiveSessionProvenance{other, owner}, "claude", 512<<20)
	require.NoError(t, err)
	assert.Equal(t, owner.Attachment, chosen)
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
			_, _, _, err := MaterializeSnapshot(t.Context(), []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{modified}, "claude", 512<<20)
			require.ErrorContains(t, err, tc.want)
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
	assert.Equal(t, owner.Attachment, chosen)
	require.NoError(t, tree.Cleanup())
	broken := source
	broken.Name = "broken"
	broken.Repository = filepath.Join(t.TempDir(), "repository")
	require.NoError(t, os.CopyFS(broken.Repository, os.DirFS(source.Repository)))
	manifestPath := filepath.Join(broken.Repository, "snapshots", source.Snapshot+".mvmanifest")
	require.NoError(t, os.Chmod(manifestPath, 0o600))
	require.NoError(t, os.WriteFile(manifestPath, []byte("corrupt"), 0o600))
	_, _, _, err = MaterializeSnapshot(t.Context(), []config.ArchiveSource{broken, source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
	require.ErrorContains(t, err, "parsing snapshot")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, _, err = MaterializeSnapshot(ctx, []config.ArchiveSource{source}, []db.ArchiveSessionProvenance{owner}, "claude", 512<<20)
	require.ErrorIs(t, err, context.Canceled)
	directory := owner
	directory.Attachment.SnapshotID = "directory:fixture"
	_, _, _, err = MaterializeSnapshot(t.Context(), nil, []db.ArchiveSessionProvenance{directory}, "claude", 512<<20)
	require.ErrorContains(t, err, "directory captures cannot")
}
