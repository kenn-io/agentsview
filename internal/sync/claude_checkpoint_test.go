package sync

import (
	"crypto/sha256"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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

func TestClaudeCheckpointRemoteArchiveExcluded(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	fx.engine.pathRewriter = func(path string) string { return "remote:" + path }
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "alpha", "a")
	claudeCheckpointAppend(t, fx, path, "bravo", "b")
	_, ok, err := fx.db.GetParserCheckpoint(t.Context(), "checkpoint")
	require.NoError(t, err)
	assert.False(t, ok)
}

// appendPaddedLine appends head, padding, then a "note" field holding marker,
// sized so the file ends at rem modulo 64. With rem 63, a SHA-256 state saved
// at the end of the file would buffer the marker.
func appendPaddedLine(t *testing.T, path, head, marker string, rem int64) {
	t.Helper()
	line := func(pad int) string {
		return head + `"pad":"` + strings.Repeat("p", pad) + `","note":"` + marker + "\"}}\n"
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	pad := (rem - (info.Size()+int64(len(line(0))))%64 + 64) % 64
	appendFile(t, path, line(int(pad)))
}

const claudeNoteHead = `{"type":"progress","timestamp":"2024-01-01T00:00:06Z","data":{`

// requireBlockAlignedState checks that state is the clean SHA-256 state of
// the file's first bytes up to a 64-byte boundary, with nothing buffered.
func requireBlockAlignedState(t *testing.T, path string, state []byte) {
	t.Helper()
	n, ok := db.SHA256StateOffset(state)
	require.True(t, ok)
	require.Zero(t, n%sha256.BlockSize)
	want, _, err := codexResumeHash(path, 0, n, nil)
	require.NoError(t, err)
	assert.Equal(t, want, state)
}

func TestClaudeCheckpointRestrictedArchives(t *testing.T) {
	const marker = "SOURCE-MARKER-7f3a"
	for _, content := range []config.ArchiveContent{
		config.ArchiveContentFull,
		config.ArchiveContentTranscripts,
		config.ArchiveContentUsage,
	} {
		t.Run(string(content), func(t *testing.T) {
			fx := newEngineFixture(t)
			t.Cleanup(fx.engine.Close)
			fx.db.SetArchiveContent(content)
			path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", strings.Repeat("x", 2<<20))
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			claudeCheckpointAppend(t, fx, path, "bootstrap", "a")
			requireClaudeCheckpointHash(t, fx, path)

			original := codexResumeHashFn
			t.Cleanup(func() { codexResumeHashFn = original })
			var hashed int64
			codexResumeHashFn = func(path string, offset, size int64, state []byte) ([]byte, string, error) {
				hashed += size - offset
				return original(path, offset, size, state)
			}
			before := fx.engine.contentCheckBytes.Load()
			for i, appendLine := range []func(){
				func() {
					appendPaddedLine(t, path, claudeNoteHead, marker, 63)
					fx.engine.SyncPaths([]string{path})
				},
				func() { claudeCheckpointAppend(t, fx, path, "resumed", "b") },
			} {
				info, err := os.Stat(path)
				require.NoError(t, err)
				hashed = 0
				appendLine()
				grown, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, before, fx.engine.contentCheckBytes.Load(), "append %d", i)
				assert.LessOrEqual(t, hashed, grown.Size()-info.Size()+sha256.BlockSize, "append %d", i)
				requireClaudeCheckpointHash(t, fx, path)
				if i > 0 {
					continue
				}
				require.Equal(t, int64(sha256.BlockSize-1), grown.Size()%sha256.BlockSize)
				blobs, ok, err := fx.db.GetParserCheckpointBlobs(t.Context(), "checkpoint")
				require.NoError(t, err)
				require.True(t, ok)
				if content == config.ArchiveContentFull {
					assert.Contains(t, string(blobs.HashState), marker)
				} else {
					assert.NotContains(t, string(blobs.HashState), marker)
					requireBlockAlignedState(t, path, blobs.HashState)
				}
			}
			sess, err := fx.db.GetSessionFull(t.Context(), "checkpoint")
			require.NoError(t, err)
			assert.True(t, sess.LastWriteIncremental)
		})
	}
}

func TestClaudeCheckpointRestrictedReplacesFullModeState(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	appendPaddedLine(t, path, claudeNoteHead, "SOURCE-MARKER-7f3a", 63)
	fx.engine.SyncPaths([]string{path})
	requireClaudeCheckpointHash(t, fx, path)

	fx.db.SetArchiveContent(config.ArchiveContentTranscripts)
	claudeCheckpointAppend(t, fx, path, "after switch", "a")
	requireClaudeCheckpointHash(t, fx, path)
	blobs, ok, err := fx.db.GetParserCheckpointBlobs(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotContains(t, string(blobs.HashState), "SOURCE-MARKER-7f3a")

	before := fx.engine.contentCheckBytes.Load()
	claudeCheckpointAppend(t, fx, path, "resumed", "b")
	assert.Equal(t, before, fx.engine.contentCheckBytes.Load())
	requireClaudeCheckpointHash(t, fx, path)
}

func TestClaudeCheckpointRestrictedBlockBoundary(t *testing.T) {
	const marker = "SOURCE-MARKER-7f3a"
	for _, kind := range []string{"offset on a block boundary", "rewrite before offset"} {
		t.Run(kind, func(t *testing.T) {
			fx := newEngineFixture(t)
			t.Cleanup(fx.engine.Close)
			fx.db.SetArchiveContent(config.ArchiveContentTranscripts)
			path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", strings.Repeat("x", 200<<10))
			require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
			claudeCheckpointAppend(t, fx, path, "bootstrap", "a")
			appendPaddedLine(t, path, claudeNoteHead, marker, 63)
			fx.engine.SyncPaths([]string{path})
			requireClaudeCheckpointHash(t, fx, path)
			ctx := t.Context()

			if kind == "offset on a block boundary" {
				appendPaddedLine(t, path, claudeNoteHead, "aligned", 0)
				fx.engine.SyncPaths([]string{path})
				cp, ok, err := fx.db.GetParserCheckpoint(ctx, "checkpoint")
				require.NoError(t, err)
				require.True(t, ok)
				require.Zero(t, cp.Offset%sha256.BlockSize)
				requireClaudeCheckpointHash(t, fx, path)
				before := fx.engine.contentCheckBytes.Load()
				claudeCheckpointAppend(t, fx, path, "resumed", "b")
				assert.Equal(t, before, fx.engine.contentCheckBytes.Load())
				requireClaudeCheckpointHash(t, fx, path)
				return
			}
			// Change bytes between the saved state's block boundary and the
			// committed offset, then refresh the anchor digest so only the
			// re-read of that range can reject the checkpoint.
			cp, ok, err := fx.db.GetParserCheckpoint(ctx, "checkpoint")
			require.NoError(t, err)
			require.True(t, ok)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			at := strings.LastIndex(string(raw), marker)
			require.GreaterOrEqual(t, int64(at), cp.Offset-cp.Offset%sha256.BlockSize)
			copy(raw[at:], "SOURCE-MARKER-0000")
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			cp.TailAnchorDigest, err = codexCheckpointAnchorDigest(path, cp.Offset)
			require.NoError(t, err)
			blobs, ok, err := fx.db.GetParserCheckpointBlobs(ctx, "checkpoint")
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, fx.db.UpsertParserCheckpoint(ctx, *cp, blobs))

			before := fx.engine.contentCheckBytes.Load()
			claudeCheckpointAppend(t, fx, path, "after rewrite", "b")
			assert.Greater(t, fx.engine.contentCheckBytes.Load(), before)
			requireClaudeCheckpointHash(t, fx, path)
		})
	}
}

func TestClaudeCheckpointFullArchiveResumesBlockAlignedState(t *testing.T) {
	// A state written by a restricted archive records its own offset, so the
	// same archive opened in full mode resumes from it.
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", strings.Repeat("x", 200<<10))
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "bootstrap", "a")
	appendPaddedLine(t, path, claudeNoteHead, "SOURCE-MARKER-7f3a", 63)
	fx.engine.SyncPaths([]string{path})
	ctx := t.Context()
	cp, ok, err := fx.db.GetParserCheckpoint(ctx, "checkpoint")
	require.NoError(t, err)
	require.True(t, ok)
	blobs, ok, err := fx.db.GetParserCheckpointBlobs(ctx, "checkpoint")
	require.NoError(t, err)
	require.True(t, ok)
	blobs.HashState, _, err = codexResumeHash(path, 0, cp.Offset-cp.Offset%sha256.BlockSize, nil)
	require.NoError(t, err)
	require.NoError(t, fx.db.UpsertParserCheckpoint(ctx, *cp, blobs))

	before := fx.engine.contentCheckBytes.Load()
	claudeCheckpointAppend(t, fx, path, "resumed", "b")
	assert.Equal(t, before, fx.engine.contentCheckBytes.Load())
	requireClaudeCheckpointHash(t, fx, path)
}
