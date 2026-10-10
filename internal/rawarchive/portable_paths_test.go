package rawarchive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSeedReparsePreservesProducerPaths(t *testing.T) {
	for _, original := range []string{"/example/sessions", `Z:\example\sessions`} {
		t.Run(original, func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
			root := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "portable history", id).String()))
			stored := original + "/project/" + id + ".jsonl"
			if original[0] != '/' {
				stored = original + `\project\` + id + ".jsonl"
			}
			opts := newImportCapture(t, owner, "source", RootSpec{Provider: "claude", Path: root})
			source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
			require.NoError(t, err)
			require.NoError(t, source.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "project-a", Machine: owner, FilePath: &stored}))
			require.NoError(t, source.Close())
			opts.Destination = filepath.Join(t.TempDir(), "capture")
			descriptor, err := Capture(ctx, opts)
			require.NoError(t, err)
			var inventory captureInventory
			data, err := os.ReadFile(filepath.Join(opts.Destination, "inventory.json"))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &inventory))
			descriptor.Source.Roots[0].OriginalPath = original
			descriptor.Source.Roots[0].ConfiguredPath = original
			descriptor.Source.Roots[0].Aliases = nil
			inventory.Source = descriptor.Source
			descriptor.CaptureID = ""
			data, err = json.Marshal(descriptor)
			require.NoError(t, err)
			reportPath := filepath.Join(opts.Destination, "roots", "application", "capture-report.json")
			require.NoError(t, os.WriteFile(reportPath, data, 0o600))
			for i, file := range inventory.Files {
				if file.RootID == "application" && file.Path == "capture-report.json" {
					inventory.Files[i], err = inventoryFile(ctx, reportPath, "application", file.Path, file.Method)
					require.NoError(t, err)
				}
			}
			data, err = json.Marshal(inventory)
			require.NoError(t, err)
			digest := sha256.Sum256(data)
			descriptor.CaptureID = hex.EncodeToString(digest[:])
			require.NoError(t, os.WriteFile(filepath.Join(opts.Destination, "inventory.json"), data, 0o600))
			data, err = json.Marshal(descriptor)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(opts.Destination, "capture.json"), data, 0o600))
			seedPath := filepath.Join(t.TempDir(), "seed")
			_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), seedPath, nil)
			require.NoError(t, err)
			seeded, err := db.OpenIsolatedContext(ctx, filepath.Join(seedPath, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, seeded.Close()) })
			archive, err := Open(ctx, seeded, seedPath, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			session, err := seeded.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, stored, *session.FilePath)
			messages, err := seeded.GetAllMessages(ctx, id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "portable history", messages[0].Content)
		})
	}
}
