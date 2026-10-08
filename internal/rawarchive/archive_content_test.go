package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const usageAutomatedSession = "usage-automated"

// A usage-only archive keeps no transcript text, so the stored automation flag
// is the only record of the classification made when the session was written.
func writeUsageAutomatedSession(t *testing.T, path string) *db.DB {
	t.Helper()
	database, err := db.OpenWithArchiveContent(t.Context(), path, config.ArchiveContentUsage)
	require.NoError(t, err)
	prompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: usageAutomatedSession, Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &prompt, StartedAt: &startedAt, UserMessageCount: 1,
	}))
	return database
}

func requireUsageAutomation(t *testing.T, database *db.DB) {
	t.Helper()
	session, err := database.GetSessionFull(t.Context(), usageAutomatedSession)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.IsAutomated,
		"archive workflows cannot reclassify text a usage-only archive discarded")
}

func TestSeedKeepsUsageOnlyAutomation(t *testing.T) {
	opts := captureFixture(t)
	opts.Roots[0].Provider = "files"
	opts.Settings.ArchiveContent = config.ArchiveContentUsage
	dbtest.WriteTestFile(t, filepath.Join(opts.DataDir, "telemetry-install-id"),
		[]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	require.NoError(t, writeUsageAutomatedSession(t, filepath.Join(opts.DataDir, "sessions.db")).Close())
	_, err := Capture(t.Context(), opts)
	require.NoError(t, err)

	target := filepath.Join(t.TempDir(), "seed")
	_, err = Seed(t.Context(), filepath.Join(opts.Destination, "capture.json"), target, nil)
	require.NoError(t, err)

	seeded, err := db.OpenWithArchiveContent(t.Context(), filepath.Join(target, "sessions.db"), config.ArchiveContentUsage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, seeded.Close()) })
	requireUsageAutomation(t, seeded)
}

func TestSelectedReparseKeepsUnselectedUsageOnlyAutomation(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := writeUsageAutomatedSession(t, filepath.Join(data, "sessions.db"))
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.EnableArchiveOnly(ctx))
	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "project", id+".jsonl"), []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "foreign history", id).String()))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capture := newImportCapture(t, foreign, "foreign", RootSpec{Provider: "claude", Path: root})
	report, err := archive.Import(ctx, loadTestCapture(t, &capture))
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	require.NoError(t, os.RemoveAll(root))
	sources, err := database.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)

	report, err = archive.Reparse(ctx, ReparseOptions{
		ManifestIDs: []string{sources[0].ManifestID}, ScratchBytes: 1 << 20,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Parsed)
	foreignSession, err := database.GetSessionFull(ctx, foreign+"~"+id)
	require.NoError(t, err)
	require.NotNil(t, foreignSession)
	requireUsageAutomation(t, database)
}
