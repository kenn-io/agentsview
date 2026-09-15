//go:build !(windows && arm64)

// ABOUTME: Read-path parity for duplicate badges: DuckDB session and
// ABOUTME: sidebar reads must decorate rows from the mirrored membership
// ABOUTME: table exactly like the SQLite archive does.
package duckdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDuckSessionReadsDecorateDuplicateRoles(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	started := "2026-08-10T08:00:00Z"
	first := "I need you to research and create a comprehensive plan."
	writes := []db.SessionBatchWrite{
		{Session: db.Session{
			ID: "duck:canonical", Project: "p", Machine: "local",
			Agent: "goose", StartedAt: &started, FirstMessage: &first,
			MessageCount: 5,
		}, DataVersion: 1},
		{Session: db.Session{
			ID: "duck:copy", Project: "p", Machine: "local",
			Agent: "augure-desktop", StartedAt: &started, FirstMessage: &first,
			MessageCount: 4,
		}, DataVersion: 1},
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)

	result, err := local.RebuildDuplicateGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, result.Groups)
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	page, err := store.ListSessions(ctx, db.SessionFilter{})
	require.NoError(t, err, "ListSessions")
	byID := map[string]db.Session{}
	for _, sess := range page.Sessions {
		byID[sess.ID] = sess
	}
	assert.Equal(t, "canonical", byID["duck:canonical"].DuplicateRole)
	assert.Equal(t, 2, byID["duck:canonical"].DuplicateMemberCount)
	assert.Equal(t, "duplicate", byID["duck:copy"].DuplicateRole)
	assert.Equal(t, "duck:canonical", byID["duck:copy"].DuplicateCanonicalID)

	sess, err := store.GetSession(ctx, "duck:copy")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess)
	assert.Equal(t, "duplicate", sess.DuplicateRole)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{})
	require.NoError(t, err, "GetSidebarSessionIndex")
	var sidebarCopy *db.SidebarSessionIndexRow
	for i := range index.Sessions {
		if index.Sessions[i].ID == "duck:copy" {
			sidebarCopy = &index.Sessions[i]
		}
	}
	require.NotNil(t, sidebarCopy, "seeded session appears in sidebar")
	assert.Equal(t, "duplicate", sidebarCopy.DuplicateRole)
	assert.Equal(t, 2, sidebarCopy.DuplicateMemberCount)
}
