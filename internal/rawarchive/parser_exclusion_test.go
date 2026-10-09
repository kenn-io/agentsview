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

// The Claude parser intentionally drops content-free /usage probes, such as
// those a usage monitor creates. An accepted probe transcript yields no
// session, and that must not abort the rest of the reparse batch.
func TestReparseSkipsParserExcludedSources(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const probe = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const conversation = "019eb791-cf7d-75c1-8439-9ed74c122e03"
	const usage = "<command-name>/usage</command-name>\n<command-message>usage</command-message>\n<command-args></command-args>"
	root := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(root, "project", probe+".jsonl"), []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", usage, probe).String()))
	dbtest.WriteTestFile(t, filepath.Join(root, "project", conversation+".jsonl"), []byte(testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "real question", conversation).
		AddClaudeAssistant("2026-01-01T00:00:01Z", "real answer").String()))
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capture := newImportCapture(t, foreign, "foreign", RootSpec{Provider: "claude", Path: root})
	report, err := archive.Import(ctx, loadTestCapture(t, &capture))
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	require.Equal(t, 2, report.Sources)
	require.NoError(t, os.RemoveAll(root))

	report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err, "an intentionally excluded probe must not abort the batch")
	assert.Equal(t, 2, report.Parsed)
	messages, err := database.GetAllMessages(ctx, foreign+"~"+conversation)
	require.NoError(t, err)
	assert.Len(t, messages, 2)
	probeSession, err := database.GetSessionFull(ctx, foreign+"~"+probe)
	require.NoError(t, err)
	assert.Nil(t, probeSession, "the probe stays out of the archive")
}
