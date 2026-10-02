//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestFleetBatchAndSearchMetadata(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	for _, id := range []string{"codex:shared", "node-a~codex:shared", "unrelated"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "app", Machine: "node-a", Agent: "codex", SessionName: new("Provider title"), FilePath: new("/archive/session.jsonl"), FileSize: new(int64(1234)), Cwd: "/workspace/app", MessageCount: 2, UserMessageCount: 2}))
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "fleetneedle", ContentLength: 11}}))
	}
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET local_modified_at = '2026-09-01T12:00:00Z'`)
		return err
	}))
	syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err := syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())
	page, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{"codex:shared"}, IncludeSource: true, ExcludeOneShot: true, ExcludeAutomated: true})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2)
	for _, s := range page.Sessions {
		require.NotNil(t, s.FileSize)
		assert.Equal(t, int64(1234), *s.FileSize)
		require.NotNil(t, s.LocalModifiedAt)
		assert.Equal(t, "2026-09-01T12:00:00Z", *s.LocalModifiedAt)
	}
	empty, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{}})
	require.NoError(t, err)
	assert.Empty(t, empty.Sessions)
	for _, mode := range []string{"substring", "regex", "fts"} {
		t.Run(mode, func(t *testing.T) {
			result, err := store.SearchContent(ctx, db.ContentSearchFilter{Pattern: "fleetneedle", Mode: mode, Sources: []string{"messages"}, Limit: 20})
			require.NoError(t, err)
			require.Len(t, result.Matches, 3)
			for _, match := range result.Matches {
				assert.Equal(t, "node-a", match.Machine)
				require.NotNil(t, match.DisplayName)
				assert.Equal(t, "Provider title", *match.DisplayName)
			}
		})
	}
	search, err := store.Search(ctx, db.SearchFilter{Query: "fleetneedle", Limit: 20})
	require.NoError(t, err)
	require.Len(t, search.Results, 3)
	for _, result := range search.Results {
		assert.Equal(t, "node-a", result.Machine)
	}
}
