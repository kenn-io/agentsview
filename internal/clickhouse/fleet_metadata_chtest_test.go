//go:build chtest

package clickhouse

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestFleetBatchAndSourceMetadata(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := t.Context()
	const rawID = "codex:shared_%"
	const copyID = "node-a~codex:shared_%"
	for _, id := range []string{rawID, copyID, "node-a~codex:shared-other", "node-a~codex:SHARED_%"} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Project: "app", Machine: "node-a", Agent: "codex", MessageCount: 0, IsAutomated: true, RelationshipType: "subagent", FilePath: new("/archive/session.jsonl"), FileSize: new(int64(1234))}))
	}
	require.NoError(t, local.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET local_modified_at='2026-09-01T12:00:00Z' WHERE project='app'`)
		return err
	}))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	for _, source := range []bool{false, true} {
		page, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{rawID}, IncludeSource: source, ExcludeOneShot: true, ExcludeAutomated: true})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 2)
		ids := []string{}
		for _, s := range page.Sessions {
			ids = append(ids, s.ID)
			if source {
				require.NotNil(t, s.FileSize)
				assert.Equal(t, int64(1234), *s.FileSize)
				require.NotNil(t, s.LocalModifiedAt)
				assert.Equal(t, "2026-09-01T12:00:00Z", *s.LocalModifiedAt)
			} else {
				assert.Nil(t, s.FilePath)
				assert.Nil(t, s.FileSize)
				assert.Nil(t, s.LocalModifiedAt)
			}
		}
		assert.ElementsMatch(t, []string{rawID, copyID}, ids)
	}
	page, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{copyID}})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, copyID, page.Sessions[0].ID)
	empty, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{}})
	require.NoError(t, err)
	assert.Empty(t, empty.Sessions)
}
