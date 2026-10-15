package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAnalyticsMessageScope(t *testing.T) {
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
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			t.Context(),
			[]string{sessionA},
			AnalyticsFilter{},
			false,
		)
		require.NoError(t, err)
		assert.Nil(t, scope)
	})

	t.Run("matching session has roles and content; other model has no rows", func(t *testing.T) {
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			t.Context(),
			[]string{sessionA, sessionB},
			AnalyticsFilter{Model: model},
			true,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		bySession := scope

		// sessionA: user + assistant pair both emitted
		rowsA := bySession[sessionA]
		require.Len(t, rowsA, 2, "sessionA should have 2 matched rows")
		assert.Equal(t, "user", rowsA[0].Role)
		assert.Equal(t, "assistant", rowsA[1].Role)
		assert.Equal(t, "hello content", rowsA[0].Content)
		assert.Equal(t, "world content", rowsA[1].Content)

		// sessionB: no rows (assistant used a different model)
		assert.Empty(t, bySession[sessionB],
			"sessionB should yield no rows")
	})

	t.Run("includeContent=false omits content", func(t *testing.T) {
		d := setup(t)
		scope, err := d.resolveAnalyticsMessageScope(
			t.Context(),
			[]string{sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		rows := scope[sessionA]
		require.Len(t, rows, 2)
		for _, row := range rows {
			assert.Empty(t, row.Content, "content should be empty when includeContent=false")
		}
	})

	t.Run("deduplicates sessionIDs", func(t *testing.T) {
		d := setup(t)
		// Pass sessionA twice; resolver must not error or double-count.
		scope, err := d.resolveAnalyticsMessageScope(
			t.Context(),
			[]string{sessionA, sessionA},
			AnalyticsFilter{Model: model},
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, scope)

		rows := scope[sessionA]
		assert.Len(t, rows, 2, "dedup should keep exactly 2 rows")
	})
}
