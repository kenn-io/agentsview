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
			active:          true,
			parentSessionID: "parent-1",
			parentResolved:  true,
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
		assert := assert.New(t)

		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		require.True(t, cache.Put(path, 40, 101, 202, state))

		_, offsetOK := cache.Get(path, 41, 101, 202)
		_, inodeOK := cache.Get(path, 40, 102, 202)
		_, deviceOK := cache.Get(path, 40, 101, 203)

		assert.False(offsetOK)
		assert.False(inodeOK)
		assert.False(deviceOK)
	})

	t.Run("same key replacement returns newest state", func(t *testing.T) {
		require := require.New(t)

		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		replacement := state
		replacement.model = "gpt-5.1"
		replacement.lastTaskEvent = "task_complete"

		require.True(cache.Put(path, 40, 101, 202, state))
		require.True(cache.Put(path, 40, 101, 202, replacement))
		got, ok := cache.Get(path, 40, 101, 202)

		require.True(ok)
		assert.Equal(t, replacement, got)
	})

	t.Run("identical warm put promotes without allocations", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cache := newCodexCursorCache(2, 4096)
		const path = "rollout.jsonl"
		other := state
		other.model = "gpt-5.1"
		third := state
		third.model = "gpt-5.2"

		require.True(cache.Put(path, 10, 101, 202, state))
		require.True(cache.Put(path, 20, 101, 202, other))
		putOK := false
		allocations := testing.AllocsPerRun(100, func() {
			putOK = cache.Put(path, 10, 101, 202, state)
		})

		require.True(putOK)
		assert.Zero(allocations)
		require.True(cache.Put(path, 30, 101, 202, third))
		firstGot, firstOK := cache.Get(path, 10, 101, 202)
		_, secondOK := cache.Get(path, 20, 101, 202)
		thirdGot, thirdOK := cache.Get(path, 30, 101, 202)
		require.True(firstOK)
		assert.Equal(state, firstGot)
		assert.False(secondOK)
		require.True(thirdOK)
		assert.Equal(third, thirdGot)
	})

	t.Run("offset versions coexist", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cache := newCodexCursorCache(4, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		newer := state
		newer.model = "gpt-5.2"

		require.True(cache.Put(path, 40, 101, 202, state))
		require.True(cache.Put(path, 80, 101, 202, newer))
		oldGot, oldOK := cache.Get(path, 40, 101, 202)
		newGot, newOK := cache.Get(path, 80, 101, 202)

		require.True(oldOK)
		require.True(newOK)
		assert.Equal(state, oldGot)
		assert.Equal(newer, newGot)
	})

	t.Run("least recently used count entry is evicted", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		cache := newCodexCursorCache(2, 4096)
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		first := state
		first.model = "first"
		second := state
		second.model = "second"
		third := state
		third.model = "third"

		require.True(cache.Put(path, 10, 1, 2, first))
		require.True(cache.Put(path, 20, 1, 2, second))
		_, firstOK := cache.Get(path, 10, 1, 2)
		require.True(firstOK)
		require.True(cache.Put(path, 30, 1, 2, third))

		_, evictedOK := cache.Get(path, 20, 1, 2)
		firstGot, firstStillOK := cache.Get(path, 10, 1, 2)
		thirdGot, thirdOK := cache.Get(path, 30, 1, 2)
		assert.False(evictedOK)
		require.True(firstStillOK)
		require.True(thirdOK)
		assert.Equal(first, firstGot)
		assert.Equal(third, thirdGot)
	})

	t.Run("least recently used bytes are evicted", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		first := state
		first.cwd = strings.Repeat("a", 200)
		second := state
		second.cwd = strings.Repeat("b", 200)
		firstBytes := estimateCodexCursorEntryBytes(newCodexCursorKey(path, 10, 1, 2), first)
		secondBytes := estimateCodexCursorEntryBytes(newCodexCursorKey(path, 20, 1, 2), second)
		cache := newCodexCursorCache(
			10, max(firstBytes, secondBytes)+min(firstBytes, secondBytes)-1,
		)

		require.True(cache.Put(path, 10, 1, 2, first))
		require.True(cache.Put(path, 20, 1, 2, second))

		_, firstOK := cache.Get(path, 10, 1, 2)
		secondGot, secondOK := cache.Get(path, 20, 1, 2)
		assert.False(firstOK)
		require.True(secondOK)
		assert.Equal(second, secondGot)
	})

	t.Run("oversized entry is rejected without disturbing cache", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		maxBytes := estimateCodexCursorEntryBytes(newCodexCursorKey(path, 10, 1, 2), state) + 32
		cache := newCodexCursorCache(4, maxBytes)
		oversized := state
		oversized.cwd = strings.Repeat("x", int(maxBytes)+1)

		require.True(cache.Put(path, 10, 1, 2, state))
		assert.False(cache.Put(path, 20, 1, 2, oversized))
		got, ok := cache.Get(path, 10, 1, 2)
		_, oversizedOK := cache.Get(path, 20, 1, 2)

		require.True(ok)
		assert.Equal(state, got)
		assert.False(oversizedOK)
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
		assert := assert.New(t)
		require := require.New(t)

		path := createTestFile(t, "empty.jsonl", "")
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(err)
		require.NotNil(sess)
		assert.Empty(messages)
		assert.Equal(int64(0), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(err)
		inode, device := sourceFileIdentityForPath(path, info)
		_, ok := provider.cursorCache.Get(path, 0, inode, device)
		assert.True(ok)
	})

	t.Run("newline terminated EOF seeds raw size", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				"seed-complete", "/workspace/project-a", "codex_cli_rs", tsEarly,
			),
			testjsonl.CodexMsgJSON("user", "complete prompt", tsEarlyS1),
		)
		path := createTestFile(t, "complete.jsonl", content)
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(err)
		require.NotNil(sess)
		require.Len(messages, 1)
		assert.Equal(int64(len(content)), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(err)
		inode, device := sourceFileIdentityForPath(path, info)
		_, ok := provider.cursorCache.Get(
			path, int64(len(content)), inode, device,
		)
		assert.True(ok)
	})

	t.Run("newline-less valid EOF is parsed but not seeded", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		content := testjsonl.CodexMsgJSON("user", "unterminated prompt", tsEarlyS1)
		path := createTestFile(t, "unterminated.jsonl", content)
		provider := newCodexTestProvider(t)

		sess, messages, err := provider.parseSession(path, "local", false)

		require.NoError(err)
		require.NotNil(sess)
		require.Len(messages, 1)
		assert.Equal("unterminated prompt", messages[0].Content)
		assert.Equal(int64(len(content)), sess.File.Size)
		info, err := os.Stat(path)
		require.NoError(err)
		inode, device := sourceFileIdentityForPath(path, info)
		_, ok := provider.cursorCache.Get(
			path, int64(len(content)), inode, device,
		)
		assert.False(ok)
	})
}
