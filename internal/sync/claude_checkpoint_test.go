package sync

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func claudeCheckpointAppend(t *testing.T, fx *engineFixture, path, text, messageID string) {
	t.Helper()
	line := testjsonl.NewSessionBuilder().AddClaudeAssistantUsage("2024-01-01T00:00:05Z", text,
		testjsonl.ClaudeAssistantUsage{MessageID: messageID, RequestID: messageID, Model: "claude-sonnet-4-20250514", InputTokens: 10, OutputTokens: 1}).String()
	appendFile(t, path, line)
	fx.engine.SyncPaths([]string{path})
}

func requireClaudeCheckpointHash(t *testing.T, fx *engineFixture, path string) {
	t.Helper()
	ctx := t.Context()
	sess, err := fx.db.GetSessionFull(ctx, "checkpoint")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.FileHash)
	info, err := os.Stat(path)
	require.NoError(t, err)
	hash, err := ComputeFileHashPrefix(path, info.Size())
	require.NoError(t, err)
	assert.Equal(t, hash, *sess.FileHash)
	cp, ok, err := fx.db.GetParserCheckpoint(ctx, "checkpoint")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, hash, cp.Hash)
	assert.Equal(t, info.Size(), cp.Offset)
	inc, ok := fx.db.GetSessionForIncremental(ctx, path, "claude")
	require.True(t, ok)
	assert.Equal(t, inc.NextOrdinal, cp.NextOrdinal)
	blobs, ok, err := fx.db.GetParserCheckpointBlobs(ctx, "checkpoint")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Empty(t, blobs.Cursor)
	assert.NotEmpty(t, blobs.HashState)
}

func TestClaudeCheckpointAppendHashesOnlyNewBytes(t *testing.T) {
	for _, padding := range []int{10 << 10, 2 << 20} {
		t.Run(strconv.Itoa(padding), func(t *testing.T) {
			fx := newEngineFixture(t)
			t.Cleanup(fx.engine.Close)
			path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", strings.Repeat("x", padding))
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			claudeCheckpointAppend(t, fx, path, "bootstrap", "a")
			requireClaudeCheckpointHash(t, fx, path)
			info, err := os.Stat(path)
			require.NoError(t, err)
			before := fx.engine.contentCheckBytes.Load()
			original := codexResumeHashFn
			t.Cleanup(func() { codexResumeHashFn = original })
			var offsets []int64
			codexResumeHashFn = func(path string, offset, size int64, state []byte) ([]byte, string, error) {
				offsets = append(offsets, offset)
				return original(path, offset, size, state)
			}
			claudeCheckpointAppend(t, fx, path, "resumed", "b")
			assert.Equal(t, before, fx.engine.contentCheckBytes.Load())
			assert.Equal(t, []int64{info.Size()}, offsets)
			requireClaudeCheckpointHash(t, fx, path)
			sess, err := fx.db.GetSessionFull(t.Context(), "checkpoint")
			require.NoError(t, err)
			assert.True(t, sess.LastWriteIncremental)
		})
	}
}

func TestClaudeCheckpointFullFallbackThenSameStatRewrite(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "OLD-MARKER")
	appendFile(t, path, testjsonl.NewSessionBuilder().AddClaudeAssistantUsage("2024-01-01T00:00:01Z", strings.Repeat("x", 200<<10),
		testjsonl.ClaudeAssistantUsage{MessageID: "padding"}).String())
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "bootstrap", "a")
	claudeCheckpointAppend(t, fx, path, "resumed", "b")
	requireClaudeCheckpointHash(t, fx, path)

	// Edit outside the checkpoint's trailing anchor, then force a full parse.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Greater(t, len(raw)-strings.Index(string(raw), "OLD-MARKER"), codexCheckpointAnchorSize)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "OLD-MARKER", "NEW-MARKER", 1)), 0o600))
	appendFile(t, path, "{\"type\":\"custom-title\",\"customTitle\":\"renamed\",\"sessionId\":\"checkpoint\"}\n")
	fx.engine.SyncPaths([]string{path})
	sess, err := fx.db.GetSessionFull(t.Context(), "checkpoint")
	require.NoError(t, err)
	assert.False(t, sess.LastWriteIncremental)
	msgs, err := fx.db.GetAllMessages(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.Equal(t, "NEW-MARKER", msgs[0].Content)

	// Restore the earlier text while preserving the full parse's size and mtime.
	info, err := os.Stat(path)
	require.NoError(t, err)
	raw, err = os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "NEW-MARKER", "OLD-MARKER", 1)), 0o600))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	fx.engine.SyncPaths([]string{path})
	msgs, err = fx.db.GetAllMessages(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "OLD-MARKER", msgs[0].Content)
}

func TestClaudeCheckpointFallbacks(t *testing.T) {
	for _, kind := range []string{"same-stat rewrite", "anchor mismatch"} {
		t.Run(kind, func(t *testing.T) {
			fx := newEngineFixture(t)
			t.Cleanup(fx.engine.Close)
			path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			claudeCheckpointAppend(t, fx, path, "alpha", "a")
			claudeCheckpointAppend(t, fx, path, "bravo", "b")
			requireClaudeCheckpointHash(t, fx, path)
			info, err := os.Stat(path)
			require.NoError(t, err)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			raw = []byte(strings.Replace(string(raw), "bravo", "delta", 1))
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			if kind == "anchor mismatch" {
				claudeCheckpointAppend(t, fx, path, "after rewrite", "c")
			} else {
				require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
				fx.engine.SyncPaths([]string{path})
			}
			sess, err := fx.db.GetSessionFull(t.Context(), "checkpoint")
			require.NoError(t, err)
			require.NotNil(t, sess.FileHash)
			info, err = os.Stat(path)
			require.NoError(t, err)
			hash, err := ComputeFileHashPrefix(path, info.Size())
			require.NoError(t, err)
			assert.Equal(t, hash, *sess.FileHash)
			if kind == "same-stat rewrite" {
				assert.False(t, sess.LastWriteIncremental)
			}
			msgs, err := fx.db.GetAllMessages(t.Context(), "checkpoint")
			require.NoError(t, err)
			if kind == "same-stat rewrite" {
				require.Len(t, msgs, 3)
				assert.Equal(t, "delta", msgs[2].Content)
			}
			// A fallback may remove the checkpoint or leave it stale. The next
			// append must bootstrap from the committed prefix in either case.
			claudeCheckpointAppend(t, fx, path, "after fallback", "d")
			requireClaudeCheckpointHash(t, fx, path)
		})
	}
}

func TestClaudeCheckpointProgressAndSplitAdvance(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "Hello", "a")
	claudeCheckpointAppend(t, fx, path, "Hello world", "a")
	requireClaudeCheckpointHash(t, fx, path)
	msgs, err := fx.db.GetAllMessages(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Hello world", msgs[1].Content)
	appendFile(t, path, "{\"type\":\"progress\",\"timestamp\":\"2024-01-01T00:00:06Z\",\"data\":{}}\n")
	fx.engine.SyncPaths([]string{path})
	requireClaudeCheckpointHash(t, fx, path)
	claudeCheckpointAppend(t, fx, path, "next", "b")
	requireClaudeCheckpointHash(t, fx, path)
}

func TestClaudeCheckpointExcludedArchives(t *testing.T) {
	for _, kind := range []string{"remote", "transcripts", "usage"} {
		t.Run(kind, func(t *testing.T) {
			fx := newEngineFixture(t)
			t.Cleanup(fx.engine.Close)
			switch kind {
			case "remote":
				fx.engine.pathRewriter = func(path string) string { return "remote:" + path }
			case "transcripts":
				fx.db.SetArchiveContent(config.ArchiveContentTranscripts)
			case "usage":
				fx.db.SetArchiveContent(config.ArchiveContentUsage)
			}
			path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			claudeCheckpointAppend(t, fx, path, "alpha", "a")
			claudeCheckpointAppend(t, fx, path, "bravo", "b")
			_, ok, err := fx.db.GetParserCheckpoint(t.Context(), "checkpoint")
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}
