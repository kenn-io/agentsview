package sync_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const checkpointTestUUID = "019eb791-cf7d-75c1-8439-9ed74c122c01"

func writeCheckpointCodexSession(
	t *testing.T, env *testEnv, content string,
) string {
	t.Helper()
	return env.writeCodexSession(
		t,
		filepath.Join("2024", "01", "01"),
		"rollout-2024-01-01T10-00-00-"+checkpointTestUUID+".jsonl",
		content,
	)
}

func checkpointCodexInitial() string {
	return testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			checkpointTestUUID, "/tmp/proj", "codex_cli_rs",
			"2024-01-01T10:00:00Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "run command", "2024-01-01T10:00:01Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_cp", nil, "2024-01-01T10:00:02Z",
		),
	)
}

func TestCodexCheckpointFullParsePersistsCheckpoint(t *testing.T) {
	env := setupTestEnv(t)
	initial := checkpointCodexInitial()
	writeCheckpointCodexSession(t, env, initial)

	env.engine.SyncAll(context.Background(), nil)

	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok, "a full Codex parse must persist a checkpoint")
	assert.Equal(t, int64(len(initial)), cp.Offset)
	assert.NotEmpty(t, cp.Cursor)
	assert.NotEmpty(t, cp.HashState)
	assert.NotEmpty(t, cp.Hash)
	assert.Equal(t, 2, cp.NextOrdinal)
	assert.Equal(t, db.ParserCheckpointVersion, cp.Version)
}

func TestCodexCheckpointIncrementalResumeAdvancesCheckpoint(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	before, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)

	appended := testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON(
		"call_cp", "done", "2024-01-01T10:00:03Z",
	))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	stats := env.engine.SyncAll(ctx, nil)
	require.Equal(t, 1, stats.Synced)

	sess, err := env.db.GetSessionFull(ctx, "codex:"+checkpointTestUUID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.True(t, sess.LastWriteIncremental,
		"a checkpoint-resumed append must stay incremental")

	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "done", msgs[1].ToolCalls[0].ResultContent)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)

	after, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, before.Offset+int64(len(appended)), after.Offset)
	assert.NotEqual(t, before.Cursor, after.Cursor)
	assert.NotEqual(t, before.HashState, after.HashState)
	assert.NotEqual(t, before.Hash, after.Hash,
		"the checkpointed hash must advance to the new full-file hash")
	require.NotNil(t, sess.FileHash)
	assert.Equal(t, after.Hash, *sess.FileHash,
		"stored hash must equal the checkpointed full-file hash")
}

func TestCodexCheckpointTruncationFallsBackToFullParse(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	_, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)

	// Truncate at a safe line boundary (drop the final function_call line,
	// keeping the newline-terminated user message) so the rebuilt checkpoint
	// is a legal resume offset.
	truncated := int64(strings.LastIndex(initial, "\n") + 1)
	require.NoError(t, os.Truncate(path, truncated))

	stats := env.engine.SyncAll(ctx, nil)
	require.Equal(t, 1, stats.Synced,
		"a truncated transcript must be authoritatively reparsed")
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, truncated, cp.Offset,
		"the checkpoint must be rebuilt for the truncated file")
}

func TestCodexCheckpointAnchorMismatchFallsBackToFullParse(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	info, err := os.Stat(path)
	require.NoError(t, err)
	origMtime := info.ModTime()

	// Corrupt one byte inside the anchor region while keeping the JSON valid
	// (call_cp -> call_cX), then append a real message so the size grows and
	// EOF stays a safe boundary. Restoring the mtime isolates the anchor
	// check: stat and identity look safe, the anchor must not.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	idx := bytes.Index(raw, []byte(`"call_cp"`))
	require.Positive(t, idx)
	raw[idx+7] = 'X'
	raw = append(raw, []byte(testjsonl.JoinJSONL(testjsonl.CodexMsgJSON(
		"user", "after corruption", "2024-01-01T10:00:04Z",
	)))...)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	require.NoError(t, os.Chtimes(path, time.Now(), origMtime))

	env.engine.SyncPaths([]string{path})
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(len(raw)), cp.Offset)
	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(t, msgs, 3)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "call_cX", msgs[1].ToolCalls[0].ToolUseID,
		"the corrupted call id must come from the authoritative full parse")
}

func TestCodexCheckpointInPlaceRewriteSameSizeSameMtimeIsTrusted(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	info, err := os.Stat(path)
	require.NoError(t, err)
	origMtime := info.ModTime()

	// In-place rewrite, same size, same mtime: append-trust mode skips it via
	// the checkpoint stat gate. This is the documented tradeoff; a periodic
	// full audit catches such rewrites.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	raw[0] ^= 0x01
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	require.NoError(t, os.Chtimes(path, time.Now(), origMtime))

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(t, msgs, 2)
	assert.Equal(t, "run command", msgs[0].Content,
		"the trusted skip must not rewrite stored messages")
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(len(initial)), cp.Offset,
		"the trusted skip must not advance the checkpoint")
}
