package remotesync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildManifestListsRegularFilesWithSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	a := filepath.Join(sub, "a.jsonl")
	require.NoError(t, os.WriteFile(a, []byte("aaaa"), 0o644))
	mtime := time.Date(2026, 7, 8, 9, 0, 0, 987654321, time.UTC)
	require.NoError(t, os.Chtimes(a, mtime, mtime))
	require.NoError(t, os.Symlink(a, filepath.Join(sub, "link.jsonl")))
	extra := filepath.Join(dir, "index.jsonl")
	require.NoError(t, os.WriteFile(extra, []byte("x"), 0o644))

	m, err := BuildManifest(TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {sub}},
		ExtraFiles: []string{extra},
	})
	require.NoError(t, err)

	// Sorted by path: <tmp>/index.jsonl precedes <tmp>/proj/a.jsonl.
	require.Len(t, m.Files, 2)
	assert.Equal(t, extra, m.Files[0].Path)
	assert.Equal(t, a, m.Files[1].Path)
	assert.Equal(t, int64(4), m.Files[1].Size)
	info, err := os.Stat(a)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), m.Files[1].MtimeNS)
}

func TestBuildManifestToleratesMissingRootsAndExtraFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := BuildManifest(TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {filepath.Join(dir, "gone")},
		},
		ExtraFiles: []string{filepath.Join(dir, "gone.jsonl")},
	})
	require.NoError(t, err)
	assert.Empty(t, m.Files)
}

func TestBuildManifestRejectsFileScopedAgents(t *testing.T) {
	_, err := BuildManifest(TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentWindsurf: {"/srv/Windsurf/User"}},
		Files: map[parser.AgentType][]string{
			parser.AgentWindsurf: {"/srv/Windsurf/User/workspaceStorage/a/state.vscdb"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file-scoped")
}
