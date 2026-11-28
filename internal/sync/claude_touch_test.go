package sync

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A running Claude session's transcript gets its modified time refreshed
// without any new bytes. That must not re-parse the transcript, whether the
// change arrives as a watched path or in a full pass.
func TestClaudeModifiedTimeTouchDoesNotReparse(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "alpha", "a")
	claudeCheckpointAppend(t, fx, path, "bravo", "b")
	revision, err := fx.db.TranscriptRevision(t.Context(), "checkpoint")
	require.NoError(t, err)

	touch := func(offset time.Duration) {
		t.Helper()
		info, err := os.Stat(path)
		require.NoError(t, err)
		touched := info.ModTime().Add(offset)
		require.NoError(t, os.Chtimes(path, touched, touched))
	}
	requireUntouched := func() {
		t.Helper()
		got, err := fx.db.TranscriptRevision(t.Context(), "checkpoint")
		require.NoError(t, err)
		assert.Equal(t, revision, got, "the stored transcript was rewritten")
		sess, err := fx.db.GetSessionFull(t.Context(), "checkpoint")
		require.NoError(t, err)
		assert.True(t, sess.LastWriteIncremental, "the transcript was fully re-parsed")
	}

	touch(time.Hour)
	fx.engine.SyncPaths([]string{path})
	requireUntouched()

	// The touch is remembered, so the next look costs no content read.
	read := fx.engine.contentCheckBytes.Load()
	fx.engine.SyncPaths([]string{path})
	assert.Equal(t, read, fx.engine.contentCheckBytes.Load())

	touch(2 * time.Hour)
	assert.Zero(t, fx.engine.SyncAll(t.Context(), nil).Synced)
	requireUntouched()

	// New bytes after a touch still append.
	claudeCheckpointAppend(t, fx, path, "charlie", "c")
	msgs, err := fx.db.GetAllMessages(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "charlie", msgs[len(msgs)-1].Content)
}

// A transcript rewritten in place at the same size, with a new modified time,
// holds different bytes and is still re-parsed.
func TestClaudeSameSizeRewriteWithNewModifiedTimeReparses(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := fx.writeClaudeSession(t, "proj", "checkpoint.jsonl", "first")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	claudeCheckpointAppend(t, fx, path, "alpha", "a")
	claudeCheckpointAppend(t, fx, path, "bravo", "b")

	info, err := os.Stat(path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "bravo", "delta", 1)), 0o600))
	later := info.ModTime().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, later, later))
	fx.engine.SyncPaths([]string{path})

	msgs, err := fx.db.GetAllMessages(t.Context(), "checkpoint")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	assert.Equal(t, "delta", msgs[len(msgs)-1].Content)
}
