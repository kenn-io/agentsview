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

func TestOpenRequiresDedicatedArchive(t *testing.T) {
	ctx := t.Context()
	database := dbtest.OpenTestDB(t)
	data := t.TempDir()
	archive, err := Open(ctx, database, data, nil)
	if archive != nil {
		require.NoError(t, archive.Close())
	}
	require.ErrorContains(t, err, "archive-only target")
	assert.NoDirExists(t, filepath.Join(data, Directory))
	require.NoError(t, database.RequireSourceSync(ctx))
	// Marking the intended target explicitly allows the same library call.
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err = Open(ctx, database, data, nil)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
}

func TestReparseHistoricalLocalMachineKeys(t *testing.T) {
	for _, machine := range []string{"", "local"} {
		t.Run(machine, func(t *testing.T) {
			ctx := t.Context()
			const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const receiver = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			const id = "saved"
			opts := captureFixture(t)
			dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), []byte(owner))
			path := filepath.Join(opts.Roots[0].Path, "project-a", id+".jsonl")
			dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "retained original", id).String()))
			source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
			require.NoError(t, err)
			require.NoError(t, source.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "project-a", Machine: machine, FilePath: new(path), MessageCount: 1}))
			require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: id, Role: "user", Content: "saved projection"}}))
			require.NoError(t, source.UpsertSession(ctx, db.Session{ID: "other", Agent: "claude", Project: "project-a", Machine: "unowned-host"}))
			// The recorded installation bypasses the general first-open adoption.
			require.NoError(t, source.ConfigureArtifactLocalMachine(ctx, owner))
			require.NoError(t, source.Close())
			_, err = Capture(ctx, opts)
			require.NoError(t, err)
			descriptor := filepath.Join(opts.Destination, "capture.json")
			target := filepath.Join(t.TempDir(), "seed")
			report, err := Seed(ctx, descriptor, target, nil)
			require.NoError(t, err)
			assert.Equal(t, []db.MachineIdentityCandidate{{Machine: "unowned-host", Sessions: 1}}, report.UnownedMachines)
			database, err := db.OpenIsolatedContext(ctx, filepath.Join(target, "sessions.db"))
			require.NoError(t, err)
			defer database.Close()
			before, err := database.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, before)
			assert.Equal(t, machine, before.Machine, "seed preserves rows without bulk ownership adoption")
			archive, err := Open(ctx, database, target, nil)
			require.NoError(t, err)
			defer archive.Close()
			require.NoError(t, os.RemoveAll(opts.Roots[0].Path))
			_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			after, err := database.GetSessionFull(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, after)
			assert.Equal(t, owner, after.Machine)
			messages, err := database.GetAllMessages(ctx, id)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "retained original", messages[0].Content)
			other, err := database.GetSessionFull(ctx, "other")
			require.NoError(t, err)
			require.NotNil(t, other)
			assert.Equal(t, "unowned-host", other.Machine)

			// Those sentinel keys must not claim sessions from a foreign device.
			foreignData := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(foreignData, "telemetry-install-id"), []byte(receiver))
			foreignDB := dbtest.OpenTestDB(t)
			require.NoError(t, foreignDB.EnableArchiveOnly(ctx))
			foreignArchive, err := Open(ctx, foreignDB, foreignData, nil)
			require.NoError(t, err)
			defer foreignArchive.Close()
			spec, err := LoadImportSpec(ctx, descriptor)
			require.NoError(t, err)
			_, err = foreignArchive.Import(ctx, spec)
			require.NoError(t, err)
			_, err = foreignArchive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			foreignSession, err := foreignDB.GetSessionFull(ctx, owner+"~"+id)
			require.NoError(t, err)
			require.NotNil(t, foreignSession)
			foreignSession.Machine = machine
			require.NoError(t, foreignDB.UpsertSession(ctx, *foreignSession))
			_, err = foreignArchive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.ErrorContains(t, err, "identity conflicts")
			kept, err := foreignDB.GetSessionFull(ctx, foreignSession.ID)
			require.NoError(t, err)
			require.NotNil(t, kept)
			assert.Equal(t, machine, kept.Machine)
		})
	}
}
