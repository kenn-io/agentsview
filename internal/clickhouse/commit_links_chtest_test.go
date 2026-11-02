//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

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
	_, err = store.DB().ExecContext(t.Context(), `ALTER TABLE messages UPDATE content='different revision' WHERE session_id='commit-live' AND ordinal=0 SETTINGS mutations_sync=2`)
	require.NoError(t, err)
	links, err := store.SessionCommitLinks(t.Context(), "commit-live")
	require.NoError(t, err)
	assert.Equal(t, "Save the change", links.Commits[0].Turn.Text)
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
	dbtest.AssertCommitLinkParity(t, local, store)
	syncer.projects = []string{"other"}
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	links, err = store.SessionCommitLinks(t.Context(), "commit-delete")
	require.NoError(t, err)
	assert.Empty(t, links.Commits)
	var count int
	require.NoError(t, store.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM commit_links WHERE session_id='commit-clash'").Scan(&count))
	assert.Zero(t, count, "scope narrowing must remove copied prompt text too")
}
