package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// codexTrashCapture captures a Codex home with one active session and one
// session the source installation may have trashed.
func codexTrashCapture(t *testing.T, device, trashed, active string, trashAtSource bool) CaptureOptions {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	paths := map[string]string{}
	for i, id := range []string{trashed, active} {
		path := filepath.Join(home, "sessions", "2026", "01", "01",
			"rollout-2026-01-01T0"+string(rune('0'+i))+"-00-00-"+id+".jsonl")
		dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
			AddCodexMeta("2026-01-01T00:00:00Z", id, "/workspace/project-a", "codex_cli_rs").
			AddCodexMessage("2026-01-01T00:00:01Z", "user", "history "+id).
			AddCodexMessage("2026-01-01T00:00:02Z", "assistant", "reply "+id).String()))
		paths[id] = path
	}
	opts := newImportCapture(t, device, "source", RootSpec{Provider: "codex", Path: home})
	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	for _, id := range []string{trashed, active} {
		require.NoError(t, source.UpsertSession(ctx, db.Session{
			ID: "codex:" + id, Agent: "codex", Project: "project-a", Machine: device, FilePath: new(paths[id]),
		}))
	}
	if trashAtSource {
		require.NoError(t, source.SoftDeleteSession(ctx, "codex:"+trashed))
	}
	require.NoError(t, source.Close())
	return opts
}

func TestReparseSuppressesTrashedCodexSessions(t *testing.T) {
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const trashed = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const active = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	for _, tt := range []struct {
		name          string
		seed          bool
		trashAtSource bool
	}{
		{name: "source trash in a receiving archive", trashAtSource: true},
		{name: "source trash in a seeded archive", seed: true, trashAtSource: true},
		{name: "trash applied in the receiving archive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			device := foreign
			if tt.seed {
				device = owner
			}
			opts := codexTrashCapture(t, device, trashed, active, tt.trashAtSource)
			spec := loadTestCapture(t, &opts)
			for _, root := range spec.Roots {
				if root.Provider == "codex" {
					require.NoError(t, os.RemoveAll(filepath.Join(root.OriginalPath, "sessions")))
				}
			}
			var database *db.DB
			var archive *Archive
			var err error
			sessionID := func(id string) string { return foreign + "~codex:" + id }
			if tt.seed {
				seedPath := filepath.Join(t.TempDir(), "seed")
				_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
				require.NoError(t, err)
				database, err = db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
				require.NoError(t, err)
				archive, err = Open(ctx, database, seedPath, nil)
				sessionID = func(id string) string { return "codex:" + id }
			} else {
				data := t.TempDir()
				dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
				database = dbtest.OpenTestDB(t)
				require.NoError(t, database.EnableArchiveOnly(ctx))
				archive, err = Open(ctx, database, data, nil)
				require.NoError(t, err)
				report, importErr := archive.Import(ctx, spec)
				require.NoError(t, importErr)
				require.Empty(t, report.Gaps)
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			if !tt.trashAtSource {
				_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
				require.NoError(t, err)
				require.NoError(t, database.SoftDeleteSession(ctx, sessionID(trashed)))
			}

			report, err := archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err, "a trashed Codex session must not abort the batch")
			assert.Equal(t, 2, report.Parsed)
			assert.Equal(t, 1, report.Suppressed)
			messages, err := database.GetAllMessages(ctx, sessionID(active))
			require.NoError(t, err)
			assert.Len(t, messages, 2, "the unrelated active session publishes")
			if tt.seed || !tt.trashAtSource {
				assert.True(t, database.IsSessionTrashed(ctx, sessionID(trashed)), "the trashed row is preserved")
			} else {
				session, err := database.GetSessionFull(ctx, sessionID(trashed))
				require.NoError(t, err)
				assert.Nil(t, session, "source trash is not published to a receiving archive")
			}
		})
	}
}
