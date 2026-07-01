package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAnalyticsMessageScope(t *testing.T) {
	t.Parallel()
	const (
		sessionA = "scope-sess-a"
		sessionB = "scope-sess-b"
		model    = "claude-sonnet"
		otherM   = "gpt-4o"
	)

	setup := func(t *testing.T) *DB {
		t.Helper()
		d := testDB(t)

		insertSession(t, d, sessionA, "proj")
		insertSession(t, d, sessionB, "proj")

		// sessionA: user turn then selected-model assistant turn
		msgs := []Message{
			{
				SessionID:     sessionA,
				Ordinal:       1,
				Role:          "user",
				Content:       "hello content",
				ContentLength: len("hello content"),
				Timestamp:     tsZero,
			},
			{
				SessionID:     sessionA,
				Ordinal:       2,
				Role:          "assistant",
				Model:         model,
				Content:       "world content",
				ContentLength: len("world content"),
				Timestamp:     tsZeroS1,
			},
		}

		// sessionB: user turn then NON-selected-model assistant turn
		msgs = append(msgs,
			Message{
				SessionID:     sessionB,
				Ordinal:       1,
				Role:          "user",
				Content:       "ignored user",
				ContentLength: len("ignored user"),
				Timestamp:     tsZero,
			},
			Message{
				SessionID:     sessionB,
				Ordinal:       2,
				Role:          "assistant",
				Model:         otherM,
				Content:       "ignored asst",
				ContentLength: len("ignored asst"),
				Timestamp:     tsZeroS1,
			},
		)

		insertMessages(t, d, msgs...)
		return d
	}

	t.Run("blank model returns nil", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA},
			AnalyticsFilter{},
			false,
		)
		require.NoError(t, err)
		assert.Nil(t, scope)
	})

	t.Run("matching session has rows", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA, sessionB},
			AnalyticsFilter{Model: model},
			true,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		bySession := scope.MessagesBySession()

		// sessionA: user + assistant pair both emitted
		rowsA := bySession[sessionA]
		require.Len(t, rowsA, 2, "sessionA should have 2 matched rows")
		assert.Equal(t, "user", rowsA[0].Role)
		assert.Equal(t, "assistant", rowsA[1].Role)

		// sessionB: no rows (assistant used a different model)
		assert.Empty(t, bySession[sessionB],
			"sessionB should yield no rows")
	})

	t.Run("StatsBySession counts user and assistant", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		stats := scope.StatsBySession()
		sA, ok := stats[sessionA]
		require.True(t, ok, "sessionA stats must exist")
		assert.Equal(t, 1, sA.UserMessages, "user message count")
		assert.Equal(t, 1, sA.AssistantMessages, "assistant message count")
		assert.Equal(t, 2, sA.Messages, "total message count")
	})

	t.Run("TimingBySession returns one entry per row", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		timing := scope.TimingBySession()
		tA := timing[sessionA]
		assert.Len(t, tA, 2, "timing should have 2 entries")
	})

	t.Run("includeContent=false omits content", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		rows := scope.MessagesBySession()[sessionA]
		require.Len(t, rows, 2)
		for _, row := range rows {
			assert.Empty(t, row.Content, "content should be empty when includeContent=false")
		}
	})

	t.Run("includeContent=true populates content", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA},
			AnalyticsFilter{Model: model},
			true,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		rows := scope.MessagesBySession()[sessionA]
		require.Len(t, rows, 2)
		assert.Equal(t, "hello content", rows[0].Content,
			"user row content should be populated")
		assert.Equal(t, "world content", rows[1].Content,
			"assistant row content should be populated")
	})

	t.Run("deduplicates sessionIDs", func(t *testing.T) {
		t.Parallel()
		d := setup(t)
		// Pass sessionA twice; resolver must not error or double-count.
		scope, err := d.resolveAnalyticsMessageScope(
			context.Background(),
			[]string{sessionA, sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		rows := scope.MessagesBySession()[sessionA]
		assert.Len(t, rows, 2, "dedup should keep exactly 2 rows")
	})
}
