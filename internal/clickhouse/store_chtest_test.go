//go:build chtest

package clickhouse

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func newPushedStore(t *testing.T) (*Store, *Sync, *db.DB) {
	t.Helper()
	ctx := context.Background()
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, SyncOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, syncer, local
}

func TestStoreSessionsMessagesAndSearch(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()

	t.Run("list_sessions_default_sort_and_cursor", func(t *testing.T) {
		page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 1})
		require.NoError(t, err)
		assert.Equal(t, 2, page.Total)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, fixtureBetaID, page.Sessions[0].ID)
		require.NotEmpty(t, page.NextCursor)

		next, err := store.ListSessions(ctx, db.SessionFilter{
			Limit: 1, Cursor: page.NextCursor,
		})
		require.NoError(t, err)
		require.Len(t, next.Sessions, 1)
		assert.Equal(t, fixtureAlphaID, next.Sessions[0].ID)
		assert.Equal(t, 2, next.Total)
	})

	t.Run("sidebar_includes_child_under_parent", func(t *testing.T) {
		index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
		require.NoError(t, err)
		assert.Equal(t, 1, index.Total)
		ids := make([]string, len(index.Sessions))
		for i, row := range index.Sessions {
			ids[i] = row.ID
		}
		assert.ElementsMatch(t, []string{fixtureAlphaID, fixtureChildID}, ids)
	})

	t.Run("get_session_and_find", func(t *testing.T) {
		sess, err := store.GetSession(ctx, fixtureAlphaID)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "alpha", sess.Project)

		ids, err := store.FindSessionIDsByPartial(ctx, "ch-sync-alpha", 5)
		require.NoError(t, err)
		assert.Contains(t, ids, fixtureAlphaID)
	})

	t.Run("messages_window_and_tool_result", func(t *testing.T) {
		asc, err := store.GetMessages(ctx, fixtureAlphaID, 0, 10, true)
		require.NoError(t, err)
		require.Len(t, asc, 2)
		assert.Equal(t, []int{0, 1}, []int{asc[0].Ordinal, asc[1].Ordinal})
		require.Len(t, asc[1].ToolCalls, 1)
		require.Len(t, asc[1].ToolCalls[0].ResultEvents, 1)
		assert.Equal(t, "clickhouse result", asc[1].ToolCalls[0].ResultEvents[0].Content)
		assert.Equal(t, "clickhouse result", asc[1].ToolCalls[0].ResultContent)

		desc, err := store.GetMessages(ctx, fixtureAlphaID, 1, 10, false)
		require.NoError(t, err)
		require.Len(t, desc, 2)
		assert.Equal(t, []int{1, 0}, []int{desc[0].Ordinal, desc[1].Ordinal})

		anchor := 1
		window, err := store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
			Around: &anchor, Before: 1, After: 1,
		})
		require.NoError(t, err)
		require.NotEmpty(t, window)
		assert.Equal(t, 1, window[len(window)-1].Ordinal)

		counts, err := store.GetResumeModelCounts(ctx, fixtureAlphaID)
		require.NoError(t, err)
		assert.Equal(t, []db.ModelCount{{Model: "claude-test", Count: 1}}, counts)

		timing, err := store.GetSessionTiming(ctx, fixtureAlphaID)
		require.NoError(t, err)
		require.NotNil(t, timing)
	})

	t.Run("search_and_secrets", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{Query: "secret token", Limit: 5})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, fixtureAlphaID, page.Results[0].SessionID)

		content, err := store.SearchContent(ctx, db.ContentSearchFilter{
			Pattern:        "clickhouse result",
			Sources:        []string{"tool_result"},
			IncludeOneShot: true,
			Limit:          5,
		})
		require.NoError(t, err)
		require.NotEmpty(t, content.Matches)
		assert.Equal(t, "tool_result", content.Matches[0].Location)
		assert.Equal(t, fixtureAlphaID, content.Matches[0].SessionID)

		findings, err := store.ListSecretFindings(ctx, db.SecretFindingFilter{
			Project: "alpha", Limit: 10,
		})
		require.NoError(t, err)
		require.Len(t, findings.Findings, 1)
		source, ok, err := store.SecretFindingSource(ctx, findings.Findings[0].SecretFinding)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Contains(t, source, "secret token")
	})

	t.Run("metadata_stars_and_pins", func(t *testing.T) {
		stats, err := store.GetStats(ctx, false, false)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.SessionCount)
		assert.Equal(t, 3, stats.MessageCount)

		projects, err := store.GetProjects(ctx, false, false)
		require.NoError(t, err)
		names := make([]string, len(projects))
		for i, p := range projects {
			names[i] = p.Name
		}
		assert.Equal(t, []string{"alpha", "beta"}, names)

		agents, err := store.GetAgents(ctx, false, false)
		require.NoError(t, err)
		require.Len(t, agents, 1)
		assert.Equal(t, "claude", agents[0].Name)

		machines, err := store.GetMachines(ctx, false, false)
		require.NoError(t, err)
		assert.Equal(t, []string{fixtureMachine}, machines)

		branches, err := store.GetBranches(ctx, false, false)
		require.NoError(t, err)
		foundMain := false
		for _, b := range branches {
			if b.Project == "alpha" && b.Branch == "main" {
				foundMain = true
			}
		}
		assert.True(t, foundMain)

		_, err = store.GetMachineLabels(ctx)
		require.NoError(t, err)
		_, err = store.GetMachineAliases(ctx)
		require.NoError(t, err)

		stars, err := store.ListStarredSessionIDs(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{fixtureAlphaID}, stars)

		pins, err := store.ListPinnedMessages(ctx, fixtureAlphaID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		require.NotNil(t, pins[0].Note)
		assert.Equal(t, "pin alpha", *pins[0].Note)
	})

	t.Run("writes_are_read_only", func(t *testing.T) {
		ok, err := store.StarSession(fixtureBetaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.False(t, ok)
		require.ErrorIs(t, store.UnstarSession(fixtureAlphaID), db.ErrReadOnly)
		require.ErrorIs(t, store.BulkStarSessions([]string{fixtureBetaID}), db.ErrReadOnly)
		pinID, err := store.PinMessage(fixtureAlphaID, 1, nil)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.Zero(t, pinID)
		require.ErrorIs(t, store.UnpinMessage(fixtureAlphaID, 1), db.ErrReadOnly)
		require.ErrorIs(t, store.RenameSession(fixtureAlphaID, nil), db.ErrReadOnly)
		require.ErrorIs(t, store.SoftDeleteSession(fixtureAlphaID), db.ErrReadOnly)
		_, err = store.RestoreSession(fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.DeleteSessionIfTrashed(fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.EmptyTrash()
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.InsertInsight(db.Insight{})
		require.ErrorIs(t, err, db.ErrReadOnly)
	})
}

func TestStoreGetSessionHidesTrash(t *testing.T) {
	ctx := context.Background()
	store, syncer, local := newPushedStore(t)
	require.NoError(t, local.SoftDeleteSession(fixtureBetaID))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	hidden, err := store.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	assert.Nil(t, hidden)

	full, err := store.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.NotNil(t, full.DeletedAt)
	assert.Equal(t, fixtureBetaID, full.ID)
}

func TestStoreGetSessionVersionChangesAfterPush(t *testing.T) {
	ctx := context.Background()
	store, syncer, local := newPushedStore(t)
	count, version, ok := store.GetSessionVersion(fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 2, count)

	appendMessage(t, local, fixtureAlphaID, "a later message", "2026-01-10T00:03:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	count2, version2, ok := store.GetSessionVersion(fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 3, count2)
	assert.NotEqual(t, version, version2)
}

func TestStoreUnimplementedAnalyticsIsLoud(t *testing.T) {
	store, _, _ := newPushedStore(t)
	_, err := store.GetAnalyticsSummary(context.Background(), db.AnalyticsFilter{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errNotImplemented))
	assert.Contains(t, err.Error(), "GetAnalyticsSummary")
}
