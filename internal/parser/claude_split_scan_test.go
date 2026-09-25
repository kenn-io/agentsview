package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeSplitRunStart(t *testing.T) {
	line := func(s string) string { return s + "\n" }
	offsetAfter := func(parts ...string) int64 {
		return int64(len(strings.Join(parts, "")))
	}

	t.Run("returns the first record of the run", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"text","text":"one"}]}}`)
		run2 := line(`{"type":"assistant","uuid":"a2","message":{"id":"m","content":[{"type":"text","text":"one two"}]}}`)
		path := createTestFile(t, "split.jsonl", user+run1+run2)

		got, ok := ClaudeSplitRunStart(path, offsetAfter(user, run1), "m")
		require.True(t, ok)
		assert.Equal(t, offsetAfter(user), got)
	})

	t.Run("skips attachment records between run chunks", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"text","text":"one"}]}}`)
		attach := line(`{"type":"attachment","uuid":"at1","content":"queued"}`)
		run2 := line(`{"type":"assistant","uuid":"a2","message":{"id":"m","content":[{"type":"text","text":"one two"}]}}`)
		path := createTestFile(t, "split.jsonl", user+run1+attach+run2)

		got, ok := ClaudeSplitRunStart(path, offsetAfter(user, run1, attach), "m")
		require.True(t, ok)
		assert.Equal(t, offsetAfter(user), got)
	})

	t.Run("stops at a user record", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"text","text":"one"}]}}`)
		path := createTestFile(t, "split.jsonl", user+run1)

		got, ok := ClaudeSplitRunStart(path, offsetAfter(user, run1), "m")
		require.True(t, ok)
		assert.Equal(t, offsetAfter(user), got)
	})

	t.Run("run starting at the file start", func(t *testing.T) {
		run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"text","text":"one"}]}}`)
		run2 := line(`{"type":"assistant","uuid":"a2","message":{"id":"m","content":[{"type":"text","text":"one two"}]}}`)
		path := createTestFile(t, "split.jsonl", run1+run2)

		got, ok := ClaudeSplitRunStart(path, offsetAfter(run1), "m")
		require.True(t, ok)
		assert.Equal(t, int64(0), got)
	})

	t.Run("stored tail is not part of the run", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		run1 := line(`{"type":"assistant","uuid":"a1","message":{"id":"m","content":[{"type":"text","text":"one"}]}}`)
		other := line(`{"type":"assistant","uuid":"a2","message":{"id":"m2","content":[{"type":"text","text":"other"}]}}`)
		path := createTestFile(t, "split.jsonl", user+run1+other)

		_, ok := ClaudeSplitRunStart(path, offsetAfter(user, run1, other), "m")
		assert.False(t, ok)
	})

	t.Run("malformed record keeps the fallback", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		broken := line(`{"type":"assistant","uuid":"a1"`)
		path := createTestFile(t, "split.jsonl", user+broken)

		_, ok := ClaudeSplitRunStart(path, offsetAfter(user, broken), "m")
		assert.False(t, ok)
	})

	t.Run("run spans a read chunk", func(t *testing.T) {
		user := line(`{"type":"user","uuid":"u1","message":{"content":"hi"}}`)
		var b strings.Builder
		b.WriteString(user)
		for i := 0; i < 4096; i++ {
			b.WriteString(line(`{"type":"assistant","uuid":"a` + strings.Repeat("x", 8) +
				`","message":{"id":"m","content":[{"type":"text","text":"` +
				strings.Repeat("y", 40) + `"}]}}`))
		}
		path := createTestFile(t, "split.jsonl", b.String())

		got, ok := ClaudeSplitRunStart(path, int64(b.Len()), "m")
		require.True(t, ok)
		assert.Equal(t, offsetAfter(user), got)
	})
}
