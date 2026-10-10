package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestReparseHonorsReceivingArchiveDeletions(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected", true: "all"}[all], func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const deleted = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			const active = "019eb791-cf7d-75c1-8439-9ed74c122e03"
			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			for _, id := range []string{deleted, active} {
				path := filepath.Join(root, "project", id+".jsonl")
				dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
					AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "archived history", id).String()))
			}
			require.NoError(t, database.EnableArchiveOnly(ctx))
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			defer archive.Close()
			capture := newImportCapture(t, foreign, "foreign", RootSpec{Provider: "claude", Path: root})
			report, err := archive.Import(ctx, loadTestCapture(t, &capture))
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			sources, err := database.ListRawArchiveSources(ctx, "", 10)
			require.NoError(t, err)
			require.Len(t, sources, 2)
			var deletedManifest string
			var manifests []string
			for _, source := range sources {
				manifests = append(manifests, source.ManifestID)
				if source.OriginalPath == filepath.Join(root, "project", deleted+".jsonl") {
					deletedManifest = source.ManifestID
				}
			}
			require.NotEmpty(t, deletedManifest)
			_, err = archive.Reparse(ctx, ReparseOptions{ManifestIDs: []string{deletedManifest}, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			require.NoError(t, database.DeleteSession(ctx, foreign+"~"+deleted))
			require.True(t, database.IsSessionExcluded(ctx, foreign+"~"+deleted))
			require.NoError(t, os.RemoveAll(root))

			opts := ReparseOptions{All: all, ScratchBytes: 1 << 20}
			if !all {
				opts.ManifestIDs = manifests
			}
			report, err = archive.Reparse(ctx, opts)
			require.NoError(t, err)
			assert.Equal(t, 2, report.Parsed)
			assert.Equal(t, 1, report.Suppressed)
			session, err := database.GetSessionFull(ctx, foreign+"~"+deleted)
			require.NoError(t, err)
			assert.Nil(t, session)
			assert.True(t, database.IsSessionExcluded(ctx, foreign+"~"+deleted))
			messages, err := database.GetAllMessages(ctx, foreign+"~"+active)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "archived history", messages[0].Content,
				"a deleted session does not prevent the rest of the batch from publishing")
		})
	}
}
