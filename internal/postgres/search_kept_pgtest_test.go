//go:build pgtest

package postgres

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchKeptMessagePush(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	ctx := t.Context()
	write := func(content string) {
		t.Helper()
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: "session", Project: "project", Machine: "device", Agent: "claude", MessageCount: 1}))
		require.NoError(t, local.ReplaceSessionMessages(ctx, "session", []db.Message{{SessionID: "session", Role: "user", Content: content}}))
		pgKeptPush(t, syncer, false)
	}
	write("needle first")
	var loads atomic.Int32
	store.runtimeState().search.Size = func(db.SearchPage) int64 { loads.Add(1); return 1 }
	filter := db.SearchFilter{Query: "needle"}
	page, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "first")
	repeat, err := store.Search(ctx, filter)
	require.NoError(t, err)
	require.Equal(t, page, repeat)
	require.EqualValues(t, 1, loads.Load())
	write("needle second")
	page, err = store.Search(ctx, filter)
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Contains(t, page.Results[0].Snippet, "second")
}
