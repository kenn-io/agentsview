//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestResolveAnalyticsMessageScope(t *testing.T) {
	const (
		sessionA = "duck-scope-a"
		sessionB = "duck-scope-b"
		model    = "claude-3-5-sonnet"
		otherM   = "gpt-4o"
		ts       = "2024-06-03T09:00:00Z" // Monday 09:00 UTC
	)

	setup := func(t *testing.T) *Store {
		t.Helper()
		// sessionA: user turn then SELECTED-model assistant turn (pairs).
		// sessionB: user turn then NON-selected-model assistant turn (drops).
		return newDuckAnalyticsStore(t, []db.SessionBatchWrite{
			{
				Session: syncSession(sessionA, "alpha", "u", ts, 2),
				Messages: []db.Message{
					// Empty model so the reducer buffers this user turn and
					// flushes it when the selected-model assistant arrives.
					duckModelMessage(sessionA, 0, "user", "hello", ts, ""),
					duckModelMessage(sessionA, 1, "assistant", "world", ts, model),
				},
				DataVersion:     1,
				ReplaceMessages: true,
			},
			{
				Session: syncSession(sessionB, "alpha", "u", ts, 2),
				Messages: []db.Message{
					duckModelMessage(sessionB, 0, "user", "hi", ts, ""),
					duckModelMessage(sessionB, 1, "assistant", "there", ts, otherM),
				},
				DataVersion:     1,
				ReplaceMessages: true,
			},
		})
	}

	store := setup(t)

	t.Run("blank model returns nil", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA}, db.AnalyticsFilter{}, false)
		require.NoError(t, err)
		assert.Nil(t, scope)
	})

	t.Run("selected model pairs user+assistant; other model yields none", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA, sessionB},
			db.AnalyticsFilter{Model: model}, false)
		require.NoError(t, err)
		require.NotNil(t, scope)
		stats := scope.StatsBySession()
		sA, ok := stats[sessionA]
		require.True(t, ok)
		assert.Equal(t, 1, sA.UserMessages)
		assert.Equal(t, 1, sA.AssistantMessages)
		assert.Equal(t, 2, sA.Messages)
		assert.Zero(t, stats[sessionB].Messages, "non-selected model contributes nothing")
	})

	t.Run("TimingBySession returns one entry per matched row", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA}, db.AnalyticsFilter{Model: model}, false)
		require.NoError(t, err)
		require.NotNil(t, scope)
		assert.Len(t, scope.TimingBySession()[sessionA], 2)
	})

	t.Run("deduplicates sessionIDs", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA, sessionA},
			db.AnalyticsFilter{Model: model}, false)
		require.NoError(t, err)
		require.NotNil(t, scope)
		assert.Equal(t, 2, scope.StatsBySession()[sessionA].Messages, "dedup keeps exactly 2 rows")
	})

	t.Run("hour filter drops non-matching rows", func(t *testing.T) {
		h := 14 // rows are at 09:00 UTC
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA},
			db.AnalyticsFilter{Model: model, Hour: &h, Timezone: "UTC"}, false)
		require.NoError(t, err)
		require.NotNil(t, scope)
		assert.Zero(t, scope.StatsBySession()[sessionA].Messages, "hour 14 drops 09:00 rows")
	})

	t.Run("includeContent=false leaves content empty", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA}, db.AnalyticsFilter{Model: model}, false)
		require.NoError(t, err)
		require.NotNil(t, scope)
		rows := scope.MessagesBySession()[sessionA]
		require.Len(t, rows, 2)
		for _, r := range rows {
			assert.Empty(t, r.Content)
		}
	})

	t.Run("includeContent=true populates content", func(t *testing.T) {
		scope, err := store.resolveAnalyticsMessageScope(
			context.Background(), []string{sessionA}, db.AnalyticsFilter{Model: model}, true)
		require.NoError(t, err)
		require.NotNil(t, scope)
		rows := scope.MessagesBySession()[sessionA]
		require.Len(t, rows, 2)
		assert.Equal(t, "hello", rows[0].Content)
		assert.Equal(t, "world", rows[1].Content)
	})
}
