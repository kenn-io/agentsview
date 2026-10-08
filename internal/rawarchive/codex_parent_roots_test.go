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

// A Codex fork can name a parent that lives in another captured Codex home.
// Import must retain that parent with the fork so reparse can resolve the
// replayed parent turns without the original directories.
func TestCodexForkParentFromAnotherCapturedRoot(t *testing.T) {
	ctx := t.Context()
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const parentID = "11111111-1111-4111-8111-111111111111"
	const childID = "22222222-2222-4222-8222-222222222222"
	childHome := t.TempDir()
	parentHome := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(parentHome, "sessions", "2026", "09", "08",
		"rollout-2026-09-08T10-00-00-"+parentID+".jsonl"), []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(parentID, "/work/project", "codex_cli_rs", "2026-09-08T10:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-09-08T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "parent task", "2026-09-08T10:00:02Z"),
	)))
	dbtest.WriteTestFile(t, filepath.Join(childHome, "sessions", "2026", "09", "09",
		"rollout-2026-09-09T10-00-00-"+childID+".jsonl"), []byte(testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(childID, parentID, "/work/project", "codex_cli_rs", "2026-09-09T10:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "parent-turn", "2026-09-09T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "replayed parent task", "2026-09-09T10:00:02Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5.4", "child-turn", "2026-09-09T10:01:00Z"),
		testjsonl.CodexMsgJSON("user", "child task", "2026-09-09T10:01:01Z"),
		testjsonl.CodexMsgJSON("assistant", "child answer", "2026-09-09T10:01:02Z"),
	)))
	data := t.TempDir()
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, data, nil)
	require.NoError(t, err)
	defer archive.Close()
	capture := newImportCapture(t, foreign, "foreign",
		RootSpec{Provider: "codex", Path: childHome}, RootSpec{Provider: "codex", Path: parentHome})
	report, err := archive.Import(ctx, loadTestCapture(t, &capture))
	require.NoError(t, err)
	require.Empty(t, report.Gaps)
	assert.Equal(t, 2, report.Sources, "each root keeps only its own primary sources")
	require.NoError(t, os.RemoveAll(childHome))
	require.NoError(t, os.RemoveAll(parentHome))

	_, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	child, err := database.GetSessionFull(ctx, foreign+"~codex:"+childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	messages, err := database.GetAllMessages(ctx, child.ID)
	require.NoError(t, err)
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	assert.Equal(t, []string{"child task", "child answer"}, contents,
		"replayed parent turns are resolved against the retained parent")
}
