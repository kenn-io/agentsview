package archivesource

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const sourceDevice = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func archiveFixture(t *testing.T, excluded ...string) (config.ArchiveSource, string) {
	t.Helper()
	ctx := t.Context()
	const device = sourceDevice
	const session = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	inputs := t.TempDir()
	data := filepath.Join(inputs, "application")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		for _, id := range excluded {
			if _, err := tx.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES(?)`, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES('claude:deleted-session')`)
		return err
	}))
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	claude := filepath.Join(inputs, "claude", "projects")
	dbtest.WriteTestFile(t, filepath.Join(claude, "project-a", session+".jsonl"), []byte(
		testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "saved Claude question", session).String()))
	codex := filepath.Join(inputs, "codex", "sessions")
	dbtest.WriteTestFile(t, filepath.Join(codex, "rollout-2026-01-01T00-00-00-"+session+".jsonl"), []byte(
		testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", session, "/workspace/project-a", "codex_cli_rs").
			AddCodexMessage("2026-01-01T00:00:01Z", "user", "saved Codex question").String()))
	extra := filepath.Join(inputs, "other-provider")
	dbtest.WriteTestFile(t, filepath.Join(extra, "original.txt"), []byte("supplemental original"))
	capture := filepath.Join(inputs, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "retired-desktop"}
	options := rawarchive.CaptureOptions{
		DataDir: data, Destination: capture, Settings: settings,
		Roots: []rawarchive.RootSpec{{Provider: "claude", Path: claude}, {Provider: "codex", Path: codex}, {Provider: "files", Path: extra}},
	}
	_, err = rawarchive.Capture(ctx, options)
	require.NoError(t, err)
	seed := filepath.Join(inputs, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	// A later changed capture retains an unaccepted manifest in the vault.
	options.IdentityFrom = filepath.Join(capture, "capture.json")
	options.Destination = filepath.Join(inputs, "capture-again")
	rollout := filepath.Join(codex, "rollout-2026-01-01T00-00-00-"+session+".jsonl")
	file, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(testjsonl.NewSessionBuilder().AddCodexMessage("2026-01-01T00:00:02Z", "user", "later question").String())
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = rawarchive.Capture(ctx, options)
	require.NoError(t, err)
	spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(options.Destination, "capture.json"))
	require.NoError(t, err)
	changed, err := archive.Import(ctx, spec)
	require.NoError(t, err)
	require.Len(t, changed.Gaps, 1)
	repository := filepath.Join(t.TempDir(), "repository")
	report, err := archive.Backup(ctx, repository, settings, "test")
	require.NoError(t, err)
	require.Equal(t, 2, report.Sources)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	return config.ArchiveSource{Name: "history", Repository: repository, Snapshot: report.SnapshotID}, inputs
}

func TestArchiveSourceBootstrapFromRepository(t *testing.T) {
	ctx := t.Context()
	source, inputs := archiveFixture(t)
	require.NoError(t, os.RemoveAll(inputs))
	cache := t.TempDir()
	index, err := OpenIndex(ctx, source, cache, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	assert.NotEmpty(t, index.Info.RepositoryID)
	assert.Equal(t, source.Snapshot, index.Info.SnapshotID)
	assert.NotEmpty(t, index.ArchiveID)
	sources, err := index.Sources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	providers := make([]string, 0, len(sources))
	for _, source := range sources {
		assert.Equal(t, sourceDevice, source.Root.DeviceID)
		assert.Equal(t, "retired-desktop", source.Root.Machine)
		assert.NotEmpty(t, source.ManifestID)
		providers = append(providers, source.Root.Provider)
	}
	assert.ElementsMatch(t, []string{"claude", "codex"}, providers)
	coverage, err := index.Coverage(ctx)
	require.NoError(t, err)
	assert.Positive(t, coverage.SupplementalFiles)
	suppressions, err := index.Suppressions(ctx, sourceDevice)
	require.NoError(t, err)
	assert.Equal(t, []db.RawArchiveSuppression{{ParserID: "claude:deleted-session", Kind: "deleted"}}, suppressions)
	var messageTables int
	require.NoError(t, index.database.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE name='messages'`).Scan(&messageTables))
	assert.Zero(t, messageTables, "the compact index must not copy parsed conversation content")
	require.NoError(t, index.Close())

	// Warm opens use the persisted compact index at a different mount path.
	moved := filepath.Join(t.TempDir(), "moved-repository")
	require.NoError(t, os.Rename(source.Repository, moved))
	source.Repository = moved
	index, err = OpenIndex(ctx, source, cache, nil)
	require.NoError(t, err)
	warm, err := index.Sources(ctx, "", 10)
	require.NoError(t, err)
	assert.Equal(t, sources, warm)
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "bootstrap leaves only its reusable compact index")
}
