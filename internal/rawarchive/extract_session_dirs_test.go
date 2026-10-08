package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExtractedCaptureKeepsEmptySessionDirectories(t *testing.T) {
	ctx := t.Context()
	claudeHome := t.TempDir()
	codexHome := t.TempDir()
	for _, dir := range []string{
		filepath.Join(claudeHome, "projects"),
		filepath.Join(codexHome, "sessions"),
		filepath.Join(codexHome, "archived_sessions"),
	} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	dbtest.WriteTestFile(t, filepath.Join(claudeHome, "history.jsonl"), []byte("{}\n"))
	opts := newImportCapture(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "source",
		RootSpec{Provider: "claude", Path: claudeHome}, RootSpec{Provider: "codex", Path: codexHome})
	spec := loadTestCapture(t, &opts)

	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	report, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Empty(t, report.Gaps)

	target := filepath.Join(t.TempDir(), "recovered")
	_, err = archive.Extract(ctx, target, report.CaptureID)
	require.NoError(t, err)
	for _, root := range spec.Roots {
		for _, dir := range root.SessionDirs {
			assert.DirExists(t, filepath.Join(target, "roots", root.ID, dir))
		}
	}

	recovered, err := LoadImportSpec(ctx, filepath.Join(target, "capture.json"))
	require.NoError(t, err)
	receiving := dbtest.OpenTestDB(t)
	require.NoError(t, receiving.EnableArchiveOnly(ctx))
	second, err := Open(ctx, receiving, t.TempDir(), nil)
	require.NoError(t, err)
	defer second.Close()
	report, err = second.Import(ctx, recovered)
	require.NoError(t, err, "an extracted capture imports like the original")
	assert.Empty(t, report.Gaps)
}
