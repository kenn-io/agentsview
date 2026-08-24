//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestIncludeDeletedListAndSearchUsesDuckDBExclusionTable(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	writes := []db.SessionBatchWrite{}
	for _, id := range []string{"duck-active", "duck-deleted", "duck-permanent"} {
		writes = append(writes, db.SessionBatchWrite{
			Session: syncSession(id, "proj", "needle "+id,
				"2026-05-01T10:00:00.000Z", 1),
			Messages: []db.Message{syncMessage(id, 0, "user", "needle "+id,
				"2026-05-01T10:00:00.000Z")},
			DataVersion: 1, ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	_, err = syncer.DB().ExecContext(ctx, `
		UPDATE sessions SET deleted_at = TIMESTAMP '2026-05-02 00:00:00'
		WHERE id IN ('duck-deleted', 'duck-permanent')`)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx,
		`INSERT INTO excluded_sessions (id) VALUES ('duck-permanent')`)
	require.NoError(t, err)

	page, err := store.ListSessions(ctx, db.SessionFilter{
		IncludeDeleted: true, Limit: 50,
	})
	require.NoError(t, err, "DuckDB include-deleted list")
	assert.ElementsMatch(t, []string{"duck-active", "duck-deleted"},
		includeDeletedDuckSessionIDs(page.Sessions))

	search, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "needle", Mode: "substring", Sources: []string{"messages"},
		IncludeOneShot: true, IncludeDeleted: true, Limit: 50,
	})
	require.NoError(t, err, "DuckDB include-deleted search")
	assert.ElementsMatch(t, []string{"duck-active", "duck-deleted"},
		includeDeletedDuckMatchIDs(search.Matches))
}

func includeDeletedDuckSessionIDs(sessions []db.Session) []string {
	ids := make([]string, len(sessions))
	for i := range sessions {
		ids[i] = sessions[i].ID
	}
	return ids
}

func includeDeletedDuckMatchIDs(matches []db.ContentMatch) []string {
	ids := make([]string, len(matches))
	for i := range matches {
		ids[i] = matches[i].SessionID
	}
	return ids
}
