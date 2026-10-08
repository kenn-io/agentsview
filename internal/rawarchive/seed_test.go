package rawarchive

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSeedPreservesDeletionAndMachineEvidence(t *testing.T) {
	opts := captureFixture(t)
	opts.Roots[0].Provider = "files"
	const installation = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"), []byte(installation))
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	for _, id := range []string{"trashed", "removed", "active"} {
		require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude", Project: "project-a", Machine: installation}))
	}
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "historical", Agent: "claude", Project: "project-a", Machine: "historical-label"}))
	require.NoError(t, database.SoftDeleteSession(t.Context(), "trashed"))
	require.NoError(t, database.DeleteSession(t.Context(), "removed"))
	require.NoError(t, database.SetSyncState(t.Context(), db.MachineAliasKeyPrefix+"known-old-label", installation))
	require.NoError(t, database.Close())
	captured, err := Capture(t.Context(), opts)
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "seed")
	report, err := Seed(t.Context(), filepath.Join(opts.Destination, "capture.json"), target, nil)
	require.NoError(t, err)
	assert.Equal(t, captured.CaptureID, report.CaptureID)
	require.NotNil(t, report.Preflight)
	assert.EqualValues(t, 1, *report.Preflight.Counts["trashed"])
	assert.EqualValues(t, 1, *report.Preflight.Counts["deleted"])
	assert.Equal(t, []db.MachineIdentityCandidate{{Machine: "historical-label", Sessions: 1}}, report.UnownedMachines)
	database, err = db.OpenIsolatedContext(t.Context(), filepath.Join(target, "sessions.db"))
	require.NoError(t, err)
	defer database.Close()
	assert.True(t, database.IsSessionTrashed(t.Context(), "trashed"))
	assert.True(t, database.IsSessionExcluded(t.Context(), "removed"))
	historical, err := database.GetSessionFull(t.Context(), "historical")
	require.NoError(t, err)
	require.NotNil(t, historical)
	assert.Equal(t, "historical-label", historical.Machine)
	aliases, err := database.GetMachineAliases(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"known-old-label": installation}, aliases)
	require.ErrorIs(t, database.RequireSourceSync(t.Context()), db.ErrArchiveOnly)
	// Assembly reads the package without rewriting its SQLite file or inventory.
	_, err = LoadCapture(t.Context(), filepath.Join(opts.Destination, "capture.json"))
	require.NoError(t, err)
}

func TestSeedDoesNotPublishFailure(t *testing.T) {
	for fault, wantError := range map[string]string{
		"existing": "requires a new data directory", "nested": "outside the capture",
		"absent-db": "preflight is unknown", "artifact": "artifact evidence",
		"owner": "database owner differs", "raw-state": "already contains raw archive state",
		"canceled": "context canceled", "changed": "seed file changed after inventory verification",
	} {
		t.Run(fault, func(t *testing.T) {
			opts := captureFixture(t)
			opts.Roots[0].Provider = "files"
			switch fault {
			case "absent-db":
				require.NoError(t, os.Remove(filepath.Join(opts.DataDir, "sessions.db")))
			case "artifact", "owner", "raw-state":
				database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(opts.DataDir, "sessions.db"))
				require.NoError(t, err)
				switch fault {
				case "artifact":
					require.NoError(t, database.SetSyncState(t.Context(), "artifact_origin_id", "source-123abc"))
				case "owner":
					require.NoError(t, database.SetSyncState(t.Context(), "artifact_local_installation_id", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
				case "raw-state":
					require.NoError(t, database.RegisterRawArchiveRoot(t.Context(), db.RawArchiveRoot{ID: "retained", ConfiguredRootID: "retained", DeviceID: "source", Machine: "source", Provider: "files", OriginalPath: "/example"}))
				}
				require.NoError(t, database.Close())
			}
			_, err := Capture(t.Context(), opts)
			require.NoError(t, err)
			target := filepath.Join(t.TempDir(), "seed")
			switch fault {
			case "existing":
				dbtest.WriteTestFile(t, filepath.Join(target, "keep"), []byte("existing data"))
			case "nested":
				target = filepath.Join(opts.Destination, "nested")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var progress func(string)
			if fault == "canceled" || fault == "changed" {
				progress = func(string) {
					if fault == "canceled" {
						cancel()
					} else {
						dbtest.WriteTestFile(t, filepath.Join(opts.Destination, "roots", "application", "telemetry-install-id"), []byte("cccccccccccccccccccccccccccccccc"))
					}
				}
			}
			_, err = Seed(ctx, filepath.Join(opts.Destination, "capture.json"), target, progress)
			require.ErrorContains(t, err, wantError)
			if fault == "existing" {
				kept, err := os.ReadFile(filepath.Join(target, "keep"))
				require.NoError(t, err)
				assert.Equal(t, "existing data", string(kept))
			} else {
				assert.NoDirExists(t, target)
			}
			staging, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".archive-seed-*"))
			require.NoError(t, err)
			assert.Empty(t, staging)
		})
	}
}
