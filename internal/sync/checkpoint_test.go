package sync_test

import (
	"bytes"
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
	assert := assert.New(t)
	require := require.New(t)

	env := setupTestEnv(t)
	initial := checkpointCodexInitial()
	writeCheckpointCodexSession(t, env, initial)

	env.engine.SyncAll(t.Context(), nil)

	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok, "a full Codex parse must persist a checkpoint")
	blobs, ok, err := env.db.GetParserCheckpointBlobs("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(len(initial)), cp.Offset)
	assert.NotEmpty(blobs.Cursor)
	assert.NotEmpty(blobs.HashState)
	assert.NotEmpty(cp.Hash)
	assert.NotEmpty(cp.TailAnchorDigest)
	assert.Equal(2, cp.NextOrdinal)
	assert.Equal(db.ParserCheckpointVersion, cp.Version)
}

func TestCodexCheckpointIncrementalResumeAdvancesCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupTestEnv(t)
	ctx := t.Context()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	before, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	beforeBlobs, ok, err := env.db.GetParserCheckpointBlobs("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)

	appended := testjsonl.JoinJSONL(
		testjsonl.CodexFunctionCallOutputJSON(
			"call_cp", "done", "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2024-01-01T10:00:04Z", 100_000, 250, 64_000,
		),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(err)
	_, err = f.WriteString(appended)
	require.NoError(err)
	require.NoError(f.Close())

	stats := env.engine.SyncAll(ctx, nil)
	require.Equal(1, stats.Synced)

	sess, err := env.db.GetSessionFull(ctx, "codex:"+checkpointTestUUID)
	require.NoError(err)
	require.NotNil(sess)
	assert.True(sess.LastWriteIncremental,
		"a checkpoint-resumed append must stay incremental")

	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(msgs, 2)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("done", msgs[1].ToolCalls[0].ResultContent)
	require.Len(msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(100_000, msgs[1].ContextTokens)
	assert.Equal(250, msgs[1].OutputTokens)
	assert.NotEmpty(msgs[1].TokenUsage,
		"the token_count following a late result must update the committed assistant")

	after, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	afterBlobs, ok, err := env.db.GetParserCheckpointBlobs("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(before.Offset+int64(len(appended)), after.Offset)
	assert.NotEqual(beforeBlobs.Cursor, afterBlobs.Cursor)
	assert.NotEqual(beforeBlobs.HashState, afterBlobs.HashState)
	assert.NotEqual(before.Hash, after.Hash,
		"the checkpointed hash must advance to the new full-file hash")
	require.NotNil(sess.FileHash)
	assert.Equal(after.Hash, *sess.FileHash,
		"stored hash must equal the checkpointed full-file hash")
}

func TestCodexCheckpointTruncationFallsBackToFullParse(t *testing.T) {
	require := require.New(t)

	env := setupTestEnv(t)
	ctx := t.Context()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	_, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)

	// Truncate at a safe line boundary (drop the final function_call line,
	// keeping the newline-terminated user message) so the rebuilt checkpoint
	// is a legal resume offset.
	truncated := int64(strings.LastIndex(initial, "\n") + 1)
	require.NoError(os.Truncate(path, truncated))

	stats := env.engine.SyncAll(ctx, nil)
	require.Equal(1, stats.Synced,
		"a truncated transcript must be authoritatively reparsed")
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(t, truncated, cp.Offset,
		"the checkpoint must be rebuilt for the truncated file")
}

func TestCodexCheckpointAnchorMismatchFallsBackToFullParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupTestEnv(t)
	ctx := t.Context()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	info, err := os.Stat(path)
	require.NoError(err)
	origMtime := info.ModTime()

	// Corrupt one byte inside the anchor region while keeping the JSON valid
	// (call_cp -> call_cX), then append a real message so the size grows and
	// EOF stays a safe boundary. Restoring the mtime isolates the anchor
	// check: stat and identity look safe, the anchor must not.
	raw, err := os.ReadFile(path)
	require.NoError(err)
	idx := bytes.Index(raw, []byte(`"call_cp"`))
	require.Positive(idx)
	raw[idx+7] = 'X'
	raw = append(raw, []byte(testjsonl.JoinJSONL(testjsonl.CodexMsgJSON(
		"user", "after corruption", "2024-01-01T10:00:04Z",
	)))...)
	require.NoError(os.WriteFile(path, raw, 0o644))
	require.NoError(os.Chtimes(path, time.Now(), origMtime))

	env.engine.SyncPaths([]string{path})
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(len(raw)), cp.Offset)
	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(msgs, 3)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("call_cX", msgs[1].ToolCalls[0].ToolUseID,
		"the corrupted call id must come from the authoritative full parse")
}

func TestCodexCheckpointInPlaceRewriteSameSizeSameMtimeIsRejected(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	env := setupTestEnv(t)
	ctx := t.Context()
	initial := checkpointCodexInitial()
	path := writeCheckpointCodexSession(t, env, initial)
	env.engine.SyncAll(ctx, nil)
	info, err := os.Stat(path)
	require.NoError(err)
	origMtime := info.ModTime()

	// In-place rewrite with restored size and mtime: the stored
	// change-time no longer matches, so the checkpoint no-op path must
	// decline and the engine must re-parse the rewritten bytes instead of
	// trusting the stale checkpoint.
	raw, err := os.ReadFile(path)
	require.NoError(err)
	// Flip the final character of the user message's text so the JSON
	// stays valid and the re-parse must produce different content.
	textEnd := bytes.Index(raw, []byte("run command")) + len("run command")
	require.Greater(textEnd, len("run command"))
	raw[textEnd-1] ^= 0x01
	require.NoError(os.WriteFile(path, raw, 0o644))
	require.NoError(os.Chtimes(path, time.Now(), origMtime))

	env.engine.SyncPaths([]string{path})

	msgs := fetchMessages(t, env.db, "codex:"+checkpointTestUUID)
	require.Len(msgs, 2)
	assert.NotEqual("run command", msgs[0].Content,
		"a same-stat rewrite must be re-parsed, not trusted")
	cp, ok, err := env.db.GetParserCheckpoint("codex:" + checkpointTestUUID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(int64(len(initial)), cp.Offset,
		"the rewritten bytes are the same length, so the checkpoint offset stays")
}
