package parser

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexCursorCache(t *testing.T) {
	digest := sha256.Sum256([]byte("first prompt"))
	state := codexCursorState{
		model:                    "gpt-5",
		cwd:                      "/workspace/project-a",
		firstUserDigest:          digest,
		firstUserSeen:            true,
		sawUserTurnAfterFirst:    false,
		mayReplayFirstUserPrompt: true,
		forkGate: codexForkGate{
			active:    true,
			createdMs: 1720000000000,
		},
		lastTaskEvent: "turn_aborted",
	}

	t.Run("put and get exact cleaned key", func(t *testing.T) {
		cache := newCodexCursorCache(4, 4096)
		rawPath := filepath.Join(t.TempDir(), "sessions", "..", "rollout.jsonl")

		require.True(t, cache.Put(rawPath, 17, 101, 202, state))
		got, ok := cache.Get(filepath.Clean(rawPath), 17, 101, 202)

		require.True(t, ok)
		assert.Equal(t, state, got)
	})

	t.Run("requires exact offset and known identity", func(t *testing.T) {
		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		require.True(t, cache.Put(path, 40, 101, 202, state))

		_, offsetOK := cache.Get(path, 41, 101, 202)
		_, inodeOK := cache.Get(path, 40, 102, 202)
		_, deviceOK := cache.Get(path, 40, 101, 203)

		assert.False(t, offsetOK)
		assert.False(t, inodeOK)
		assert.False(t, deviceOK)
	})

	t.Run("same key replacement returns newest state", func(t *testing.T) {
		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		replacement := state
		replacement.model = "gpt-5.1"
		replacement.lastTaskEvent = "task_complete"

		require.True(t, cache.Put(path, 40, 101, 202, state))
		require.True(t, cache.Put(path, 40, 101, 202, replacement))
		got, ok := cache.Get(path, 40, 101, 202)

		require.True(t, ok)
		assert.Equal(t, replacement, got)
	})

	t.Run("identical warm put promotes without allocations", func(t *testing.T) {
		cache := newCodexCursorCache(2, 4096)
		const path = "rollout.jsonl"
		other := state
		other.model = "gpt-5.1"
		third := state
		third.model = "gpt-5.2"

		require.True(t, cache.Put(path, 10, 101, 202, state))
		require.True(t, cache.Put(path, 20, 101, 202, other))
		putOK := false
		allocations := testing.AllocsPerRun(100, func() {
			putOK = cache.Put(path, 10, 101, 202, state)
		})

		require.True(t, putOK)
		assert.Zero(t, allocations)
		require.True(t, cache.Put(path, 30, 101, 202, third))
		firstGot, firstOK := cache.Get(path, 10, 101, 202)
		_, secondOK := cache.Get(path, 20, 101, 202)
		thirdGot, thirdOK := cache.Get(path, 30, 101, 202)
		require.True(t, firstOK)
		assert.Equal(t, state, firstGot)
		assert.False(t, secondOK)
		require.True(t, thirdOK)
		assert.Equal(t, third, thirdGot)
	})

	t.Run("offset versions coexist", func(t *testing.T) {
		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		newer := state
		newer.model = "gpt-5.2"

		require.True(t, cache.Put(path, 40, 101, 202, state))
		require.True(t, cache.Put(path, 80, 101, 202, newer))
		oldGot, oldOK := cache.Get(path, 40, 101, 202)
		newGot, newOK := cache.Get(path, 80, 101, 202)

		require.True(t, oldOK)
		require.True(t, newOK)
		assert.Equal(t, state, oldGot)
		assert.Equal(t, newer, newGot)
	})

	t.Run("least recently used count entry is evicted", func(t *testing.T) {
		cache := newCodexCursorCache(2, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		first := state
		first.model = "first"
		second := state
		second.model = "second"
		third := state
		third.model = "third"

		require.True(t, cache.Put(path, 10, 1, 2, first))
		require.True(t, cache.Put(path, 20, 1, 2, second))
		_, firstOK := cache.Get(path, 10, 1, 2)
		require.True(t, firstOK)
		require.True(t, cache.Put(path, 30, 1, 2, third))

		_, evictedOK := cache.Get(path, 20, 1, 2)
		firstGot, firstStillOK := cache.Get(path, 10, 1, 2)
		thirdGot, thirdOK := cache.Get(path, 30, 1, 2)
		assert.False(t, evictedOK)
		require.True(t, firstStillOK)
		require.True(t, thirdOK)
		assert.Equal(t, first, firstGot)
		assert.Equal(t, third, thirdGot)
	})

	t.Run("least recently used bytes are evicted", func(t *testing.T) {
		cache := newCodexCursorCache(10, 900)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		first := state
		first.cwd = strings.Repeat("a", 200)
		second := state
		second.cwd = strings.Repeat("b", 200)

		require.True(t, cache.Put(path, 10, 1, 2, first))
		require.True(t, cache.Put(path, 20, 1, 2, second))

		_, firstOK := cache.Get(path, 10, 1, 2)
		secondGot, secondOK := cache.Get(path, 20, 1, 2)
		assert.False(t, firstOK)
		require.True(t, secondOK)
		assert.Equal(t, second, secondGot)
	})

	t.Run("oversized entry is rejected without disturbing cache", func(t *testing.T) {
		cache := newCodexCursorCache(4, 900)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		oversized := state
		oversized.cwd = strings.Repeat("x", 901)

		require.True(t, cache.Put(path, 10, 1, 2, state))
		assert.False(t, cache.Put(path, 20, 1, 2, oversized))
		got, ok := cache.Get(path, 10, 1, 2)
		_, oversizedOK := cache.Get(path, 20, 1, 2)

		require.True(t, ok)
		assert.Equal(t, state, got)
		assert.False(t, oversizedOK)
	})
}

func TestCodexCursorSafeResumeOffset(t *testing.T) {
	t.Run("zero offset in empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.jsonl")
		require.NoError(t, os.WriteFile(path, nil, 0o644))

		safe, err := codexSafeResumeOffset(path, 0)

		require.NoError(t, err)
		assert.True(t, safe)
	})

	t.Run("newline terminated offset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "complete.jsonl")
		content := "{}\n"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		safe, err := codexSafeResumeOffset(path, int64(len(content)))

		require.NoError(t, err)
		assert.True(t, safe)
	})

	t.Run("valid JSON without newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unterminated.jsonl")
		content := "{}"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		safe, err := codexSafeResumeOffset(path, int64(len(content)))

		require.NoError(t, err)
		assert.False(t, safe)
	})
}

func TestCodexCursorConsumedSizeStopsBeforeValidEOF(t *testing.T) {
	complete := "{}\n"
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(complete+"{}"), 0o644))

	consumed, err := CodexTranscriptConsumedSize(path)

	require.NoError(t, err)
	assert.Equal(t, int64(len(complete)), consumed)
}

func TestCodexCursorFullParseSeedBoundaries(t *testing.T) {
	t.Run("empty file seeds offset zero", func(t *testing.T) {
		path := createTestFile(t, "empty.jsonl", "")
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Empty(t, messages)
		assert.Equal(t, int64(0), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(t, err)
		inode, device := sourceFileIdentity(info)
		_, ok := provider.cursorCache.Get(path, 0, inode, device)
		assert.True(t, ok)
	})

	t.Run("newline terminated EOF seeds raw size", func(t *testing.T) {
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				"seed-complete", "/workspace/project-a", "codex_cli_rs", tsEarly,
			),
			testjsonl.CodexMsgJSON("user", "complete prompt", tsEarlyS1),
		)
		path := createTestFile(t, "complete.jsonl", content)
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(t, err)
		require.NotNil(t, sess)
		require.Len(t, messages, 1)
		assert.Equal(t, int64(len(content)), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(t, err)
		inode, device := sourceFileIdentity(info)
		_, ok := provider.cursorCache.Get(
			path, int64(len(content)), inode, device,
		)
		assert.True(t, ok)
	})

	t.Run("newline-less valid EOF is parsed but not seeded", func(t *testing.T) {
		content := testjsonl.CodexMsgJSON("user", "unterminated prompt", tsEarlyS1)
		path := createTestFile(t, "unterminated.jsonl", content)
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(t, err)
		require.NotNil(t, sess)
		require.Len(t, messages, 1)
		assert.Equal(t, "unterminated prompt", messages[0].Content)
		assert.Equal(t, int64(len(content)), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(t, err)
		inode, device := sourceFileIdentity(info)
		_, ok := provider.cursorCache.Get(
			path, int64(len(content)), inode, device,
		)
		assert.False(t, ok)
	})
}
