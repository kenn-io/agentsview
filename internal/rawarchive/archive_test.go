package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestRejectedIdentityKeepsHistoryAndChangedImportKeepsHead(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	path := filepath.Join(root, "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	header := testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").String()
	valid := header + testjsonl.NewSessionBuilder().AddCodexMessage("2026-01-01T00:00:01Z", "user", "retained question").String()
	dbtest.WriteTestFile(t, path, []byte(valid))
	database := dbtest.OpenTestDB(t)
	engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Ephemeral: true, Machine: "origin-device", DisableFilesystemProjectDiscovery: true})
	require.NoError(t, engine.SyncPathsContext(ctx, []string{path}))
	engine.Close()
	before, err := database.GetAllMessages(ctx, "codex:"+id)
	require.NoError(t, err)
	require.Len(t, before, 1)
	// The same native session ID belongs to a different original source path.
	// Reject that collision after parsing without publishing scratch writes.
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	spec := ImportSpec{DeviceID: "origin", Machine: "origin-device", Roots: []RootSpec{{ID: "codex", Provider: "codex", Path: root, OriginalPath: filepath.Join(root, "different-origin")}}}
	report, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	require.Equal(t, 1, report.Sources)
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.Error(t, err)
	assert.Zero(t, report.Parsed)
	after, err := database.GetAllMessages(ctx, "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	source, err := database.GetRawArchiveSource(ctx, sources[0].ManifestID)
	require.NoError(t, err)
	assert.NotEmpty(t, source.ParseError)
	report, err = archive.Verify(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Parsed)
	// A different capture of the same identity is retained but never silently
	// reparented or allowed to replace the accepted source.
	require.NoError(t, os.WriteFile(path, []byte(header), 0o600))
	report, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Len(t, report.Gaps, 1)
	head, err := database.RawArchiveHead(ctx, source.RootID, source.SourceKey)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.Equal(t, source.ManifestID, head.ManifestID)
	files, err := database.ListRawArchiveFiles(ctx, 0, 10)
	require.NoError(t, err)
	assert.Len(t, files, 2)
	target := filepath.Join(t.TempDir(), "native")
	_, err = archive.Extract(ctx, target)
	require.Error(t, err)
	assert.NoDirExists(t, target)
}

// Sources absent from the seed must still claim distinct identities within one
// batch. Otherwise the second parse silently overwrites the first scratch row.
func TestReparseRejectsBatchIdentityCollision(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	spec := ImportSpec{DeviceID: "device", Machine: "origin-device"}
	for _, name := range []string{"one", "two"} {
		root := t.TempDir()
		dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", name, id).String()))
		spec.Roots = append(spec.Roots, RootSpec{ID: name, Provider: "claude", Path: root, OriginalPath: root})
	}
	report, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Equal(t, 2, report.Sources)
	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.ErrorContains(t, err, "another selected source")
	assert.Zero(t, report.Parsed)
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	assert.Nil(t, session)
	report, err = archive.Verify(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, report.Sources)
	assert.Zero(t, report.Parsed)
}
