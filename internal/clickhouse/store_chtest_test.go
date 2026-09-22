//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestReadStatusHonorsProjectFilters(t *testing.T) {
	_, syncer, local := newPushedStore(t)
	ctx := context.Background()
	archiveID, err := local.GetArchiveID(ctx)
	require.NoError(t, err)

	all, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, all.Sessions)
	assert.Equal(t, 4, all.Messages)

	alpha, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, []string{"alpha"}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, alpha.Sessions, "alpha root and its child")
	assert.Equal(t, 3, alpha.Messages)

	notAlpha, err := ReadStatus(ctx, syncer.target, fixtureMachine, archiveID, nil, []string{"alpha"})
	require.NoError(t, err)
	assert.Equal(t, 1, notAlpha.Sessions)
	assert.Equal(t, 1, notAlpha.Messages)
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

	t.Run("orphan_subagent_with_null_parent_is_sidebar_root", func(t *testing.T) {
		store, syncer, local := newPushedStore(t)
		orphanID := "ch-orphan-child"
		orphan := fixtureSession(orphanID, "alpha", "orphan first", "2026-01-10T00:06:00.000Z", 1)
		orphan.RelationshipType = "subagent"
		orphan.ParentSessionID = nil
		_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session: orphan,
			Messages: []db.Message{
				fixtureMessage(orphanID, 0, "user", "orphan first", "2026-01-10T00:06:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
		_, err = syncer.Push(ctx, false, nil)
		require.NoError(t, err)

		index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Project: "alpha"})
		require.NoError(t, err)
		ids := make([]string, len(index.Sessions))
		for i, row := range index.Sessions {
			ids[i] = row.ID
		}
		assert.Contains(t, ids, orphanID)
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
		require.Len(t, asc[1].ToolCalls, 2)
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
		ok, err := store.StarSession(t.Context(), fixtureBetaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.False(t, ok)
		require.ErrorIs(t, store.UnstarSession(t.Context(), fixtureAlphaID), db.ErrReadOnly)
		require.ErrorIs(t, store.BulkStarSessions(t.Context(), []string{fixtureBetaID}), db.ErrReadOnly)
		pinID, err := store.PinMessage(t.Context(), fixtureAlphaID, 1, nil)
		require.ErrorIs(t, err, db.ErrReadOnly)
		assert.Zero(t, pinID)
		require.ErrorIs(t, store.UnpinMessage(t.Context(), fixtureAlphaID, 1), db.ErrReadOnly)
		require.ErrorIs(t, store.RenameSession(t.Context(), fixtureAlphaID, nil), db.ErrReadOnly)
		require.ErrorIs(t, store.SoftDeleteSession(t.Context(), fixtureAlphaID), db.ErrReadOnly)
		_, err = store.RestoreSession(t.Context(), fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.DeleteSessionIfTrashed(t.Context(), fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.EmptyTrash(t.Context())
		require.ErrorIs(t, err, db.ErrReadOnly)
		_, err = store.InsertInsight(t.Context(), db.Insight{})
		require.ErrorIs(t, err, db.ErrReadOnly)
	})
}

func TestSearchTreatsUnderscoreAsLiteral(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := context.Background()
	appendMessage(t, local, fixtureAlphaID, "hello_world unique token", "2026-01-10T00:04:00.000Z")
	appendMessage(t, local, fixtureBetaID, "helloXworld unique token", "2026-01-11T00:04:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	literal, err := store.Search(ctx, db.SearchFilter{Query: "hello_world", Limit: 5})
	require.NoError(t, err)
	require.Len(t, literal.Results, 1,
		"hello_world must match the underscore session and not helloXworld")
	assert.Equal(t, fixtureAlphaID, literal.Results[0].SessionID)
}

func TestGetSessionFullReturnsFilePath(t *testing.T) {
	store, _, local := newPushedStore(t)
	ctx := context.Background()
	want, err := local.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, want)
	require.NotNil(t, want.FilePath)

	got, err := store.GetSessionFull(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.FilePath)
	assert.Equal(t, *want.FilePath, *got.FilePath)
	assert.Nil(t, got.FileHash)
	assert.Nil(t, got.LocalModifiedAt)

	listed, err := store.GetSession(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, listed)
	assert.Nil(t, listed.FilePath)
}

func TestStoreGetSessionHidesTrash(t *testing.T) {
	ctx := context.Background()
	store, syncer, local := newPushedStore(t)
	require.NoError(t, local.SoftDeleteSession(t.Context(), fixtureBetaID))
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
	count, version, ok := store.GetSessionVersion(t.Context(), fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 2, count)

	appendMessage(t, local, fixtureAlphaID, "a later message", "2026-01-10T00:03:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	count2, version2, ok := store.GetSessionVersion(t.Context(), fixtureAlphaID)
	require.True(t, ok)
	assert.Equal(t, 3, count2)
	assert.NotEqual(t, version, version2)
}

func TestStoreMessageWindowReportsRevisionWithRows(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()

	from := 0
	revision := ""
	msgs, err := store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
		From: &from, Limit: 10, Asc: true,
		Roles:            []string{"user", "assistant"},
		ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{msgs[0].Ordinal, msgs[1].Ordinal})
	assert.Equal(t, "1", revision,
		"linear page must report the fixture session revision")

	anchor := 1
	revision = ""
	msgs, err = store.GetMessagesWindow(ctx, fixtureAlphaID, db.MessageWindow{
		Around: &anchor, Before: 5, After: 5,
		Roles:            []string{"user", "assistant"},
		ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{msgs[0].Ordinal, msgs[1].Ordinal})
	assert.Equal(t, "1", revision,
		"around window must report the fixture session revision")

	revision = ""
	msgs, err = store.GetMessagesWindow(ctx, "missing", db.MessageWindow{
		Around: &anchor, Before: 5, After: 5, ObservedRevision: &revision,
	})
	require.NoError(t, err)
	assert.Empty(t, msgs)
	assert.Empty(t, revision, "no rows means no revision to describe them")
}
