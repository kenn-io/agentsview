package sync_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// writeRooCodeSyncFixture writes a RooCode task directory with a
// history_item.json and a ui_messages.json holding the given raw JSON
// array, stamping both files with mtime. It returns the two paths.
func writeRooCodeSyncFixture(
	t *testing.T, root, taskID, messagesJSON string, mtime time.Time,
) (historyPath, messagesPath string) {
	t.Helper()

	taskDir := filepath.Join(root, "tasks", taskID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))

	historyPath = filepath.Join(taskDir, "history_item.json")
	require.NoError(t, os.WriteFile(historyPath, []byte(
		`{"id":"`+taskID+`","number":1,"ts":1688836851000,`+
			`"task":"Fixture task","tokensIn":10,"tokensOut":20,`+
			`"workspace":"/tmp/roocode-project"}`,
	), 0o644))

	messagesPath = filepath.Join(taskDir, "ui_messages.json")
	require.NoError(t, os.WriteFile(messagesPath, []byte(messagesJSON), 0o644))

	require.NoError(t, os.Chtimes(historyPath, mtime, mtime))
	require.NoError(t, os.Chtimes(messagesPath, mtime, mtime))
	return historyPath, messagesPath
}

// A RooCode transcript pairs later command_output records back into
// the earlier tool-call message. Sync must persist that update: with
// an append-only write the second sync would only add new ordinals
// and the stored tool call would stay pending forever.
func TestSyncRooCodeLateCommandResultUpdatesStoredToolCall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	rooDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooDir},
		},
		Machine: "local",
	})

	base := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	_, messagesPath := writeRooCodeSyncFixture(t, rooDir, "task-late-result",
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Run tests"},`+
			`{"ts":1688836860000,"type":"ask","ask":"command","text":"npm test"}]`,
		base,
	)

	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	sessionID := "roocode:task-late-result"
	msgs, err := testDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "execute_command", msgs[1].ToolCalls[0].ToolName)
	assert.Empty(t, msgs[1].ToolCalls[0].ResultEvents,
		"the command has not produced output yet")

	// The command's failure arrives later: RooCode appends the
	// command_output record, which the parser pairs into the earlier
	// tool-call message rather than emitting a new ordinal.
	require.NoError(t, os.WriteFile(messagesPath, []byte(
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Run tests"},`+
			`{"ts":1688836860000,"type":"ask","ask":"command","text":"npm test"},`+
			`{"ts":1688836870000,"type":"say","say":"command_output",`+
			`"text":"error: 2 tests failed with exit code 1"}]`,
	), 0o644))
	later := base.Add(time.Minute)
	require.NoError(t, os.Chtimes(messagesPath, later, later))

	stats = engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	msgs, err = testDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2,
		"the paired output must not appear as an extra message")
	require.Len(t, msgs[1].ToolCalls, 1)
	events := msgs[1].ToolCalls[0].ResultEvents
	require.Len(t, events, 1,
		"the stored tool call must carry the late result event")
	assert.Equal(t, "errored", events[0].Status)
	assert.Contains(t, events[0].Content, "exit code 1")
}

// SyncAllSince must exclude unchanged RooCode tasks on stat information
// alone. Routing the cutoff filter through the provider fingerprint
// would content-hash both session files for every discovered task,
// scaling quick-sync I/O with the whole archive instead of the changed
// batch. The unchanged task's files are made unreadable, so any path
// that reads them fails loudly: pre-fix the fingerprint errors, the
// task survives the cutoff, and the downstream stat gate counts it as
// skipped instead of it never being considered at all.
func TestSyncAllSinceRooCodeCutoffIsStatOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unreadable files are not enforced on windows")
	}

	rooDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooDir},
		},
		Machine: "local",
	})

	base := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	historyPath, messagesPath := writeRooCodeSyncFixture(t, rooDir, "task-cutoff",
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Old task"}]`,
		base,
	)

	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	require.NoError(t, os.Chmod(historyPath, 0o000))
	require.NoError(t, os.Chmod(messagesPath, 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(historyPath, 0o644)
		_ = os.Chmod(messagesPath, 0o644)
	})

	cutoff := base.Add(time.Hour)
	stats = engine.SyncAllSince(t.Context(), cutoff, nil)
	assert.Equal(t, 0, stats.Synced)
	assert.Equal(t, 0, stats.Failed)
	assert.Equal(t, 0, stats.Skipped,
		"an unchanged task must be excluded by the cutoff on stat alone, "+
			"not carried into the sync pass and skipped there")

	// A sibling-only transcript change past the cutoff must still look
	// fresh: the composite folds in ui_messages.json's mtime.
	require.NoError(t, os.Chmod(historyPath, 0o644))
	require.NoError(t, os.Chmod(messagesPath, 0o644))
	require.NoError(t, os.WriteFile(messagesPath, []byte(
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Old task"},`+
			`{"ts":1688836852000,"type":"say","say":"text","text":"Appended"}]`,
	), 0o644))
	appended := cutoff.Add(time.Minute)
	require.NoError(t, os.Chtimes(messagesPath, appended, appended))

	stats = engine.SyncAllSince(t.Context(), cutoff, nil)
	assert.Equal(t, 1, stats.Synced,
		"a transcript append past the cutoff must re-sync the task")
}

// A vanished ui_messages.json parses as a zero-message session while
// history_item.json keeps the task discoverable. Sync must preserve
// the archived transcript instead of replacing it with nothing, while
// a genuinely new metadata-only task still syncs normally.
func TestSyncRooCodeMissingTranscriptPreservesArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	rooDir := t.TempDir()
	testDB := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), testDB, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooDir},
		},
		Machine: "local",
	})

	base := time.Date(2026, time.June, 4, 10, 0, 0, 0, time.UTC)
	historyPath, messagesPath := writeRooCodeSyncFixture(t, rooDir, "task-vanish",
		`[{"ts":1688836851000,"type":"say","say":"text","text":"Do the thing"},`+
			`{"ts":1688836860000,"type":"say","say":"text","text":"Working on it"}]`,
		base,
	)

	stats := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	sessionID := "roocode:task-vanish"
	msgs, err := testDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	// The transcript disappears (deleted or temporarily unavailable);
	// the composite stat changes, so the session re-parses as empty.
	require.NoError(t, os.Remove(messagesPath))
	later := base.Add(time.Minute)
	require.NoError(t, os.Chtimes(historyPath, later, later))

	engine.SyncAll(t.Context(), nil)

	msgs, err = testDB.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Len(t, msgs, 2,
		"the archived transcript must survive the missing ui_messages.json")
	sess, err := testDB.GetSessionFull(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"session counts must not be corrupted by the empty re-parse")

	// A brand-new metadata-only task (history_item.json written before
	// any transcript exists) has no archived messages and must still
	// sync: the preserve guard only protects existing archives.
	_, newMessagesPath := writeRooCodeSyncFixture(t, rooDir,
		"task-metadata-only", `[]`, base.Add(2*time.Minute))
	require.NoError(t, os.Remove(newMessagesPath))
	engine.SyncAll(t.Context(), nil)
	newSess, err := testDB.GetSessionFull(
		t.Context(), "roocode:task-metadata-only",
	)
	require.NoError(t, err)
	require.NotNil(t, newSess, "metadata-only tasks must still sync")
	assert.Equal(t, 0, newSess.MessageCount)
}
