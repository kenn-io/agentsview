//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseCommitLinksBeforeUpgradePush(t *testing.T) {
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	// Older mirrors have session tables but no commit_links table.
	_, err = store.DB().ExecContext(t.Context(), `RENAME TABLE commit_links TO future_commit_links`)
	require.NoError(t, err)
	t.Run("session", func(t *testing.T) {
		_, err := store.SessionCommitLinks(t.Context(), fixtureAlphaID)
		require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	})
	t.Run("commit", func(t *testing.T) {
		_, err := store.CommitSessionLinks(t.Context(), []string{"abc1234"})
		require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	})
}

func TestClickHouseCommitLinkParity(t *testing.T) {
	local, target := seedFixture(t)
	dbtest.SeedCommitLinks(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{Projects: []string{"commit-project"}})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	defer store.Close()
	dbtest.AssertCommitLinkParity(t, local, store)
	dbtest.AssertTrailerIncrementalPush(t, local, func() db.CommitLinkReader { return store }, func() {
		_, err := syncer.Push(t.Context(), false, nil)
		require.NoError(t, err)
	})
	_, err = store.DB().ExecContext(t.Context(), `DELETE FROM commit_links WHERE session_id='commit-live' SETTINGS mutations_sync=2`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key=? SETTINGS mutations_sync=2`, syncer.archiveKey(commitLinksReadyKeyBase))
	require.NoError(t, err)
	result, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, result.Full)
	assert.Zero(t, result.Errors)
	dbtest.AssertCommitLinkParity(t, local, store)
	require.NoError(t, local.DeleteSession(t.Context(), "commit-delete"))
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	// Deleting a published session must replace its live link with a retained link.
	wantDeleted, err := local.SessionCommitLinks(t.Context(), "commit-delete")
	require.NoError(t, err)
	gotDeleted, err := store.SessionCommitLinks(t.Context(), "commit-delete")
	require.NoError(t, err)
	assert.Equal(t, wantDeleted, gotDeleted)
	wantLookup, err := local.CommitSessionLinks(t.Context(), []string{"abc1234567890123456789012345678901234567"})
	require.NoError(t, err)
	gotLookup, err := store.CommitSessionLinks(t.Context(), []string{"abc1234567890123456789012345678901234567"})
	require.NoError(t, err)
	assert.Equal(t, wantLookup, gotLookup)
	syncer.projects = []string{"other"}
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	var count int
	require.NoError(t, store.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM commit_links WHERE session_id='commit-clash'").Scan(&count))
	assert.Zero(t, count, "scope narrowing must remove copied prompt text too")
}

func TestClickHouseRetainedCommitLinkScope(t *testing.T) {
	local, target := seedFixture(t)
	dbtest.SeedCommitLinks(t, local)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	require.NoError(t, syncer.EnsureSchema(t.Context()))
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	dbtest.AssertRetainedCommitLinkScope(t, local, func() db.CommitLinkReader { return store }, func(projects, exclude []string) {
		syncer.projects, syncer.excludeProjects = projects, exclude
		_, err := syncer.Push(t.Context(), true, nil)
		require.NoError(t, err)
	})
}
