//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestPGCommitLinkParity(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_links_test")
	dbtest.SeedCommitLinks(t, local)
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
	dbtest.AssertCommitLinkParity(t, local, store)
	_, err = pg.ExecContext(ctx, `UPDATE messages SET content='different revision' WHERE session_id='commit-live' AND ordinal=0`)
	require.NoError(t, err)
	links, err := store.SessionCommitLinks(ctx, "commit-live")
	require.NoError(t, err)
	assert.Equal(t, "Save the change", links.Commits[0].Turn.Text)
	// The rollout marker must republish sessions whose cursor already exists.
	_, err = pg.ExecContext(ctx, `DELETE FROM commit_links WHERE session_id='commit-live'`)
	require.NoError(t, err)
	require.NoError(t, local.SetSyncState(ctx, commitLinksBackfillStateKey, ""))
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Errors)
	dbtest.AssertCommitLinkParity(t, local, store)
	require.NoError(t, local.DeleteSession(ctx, "commit-delete"))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	dbtest.AssertCommitLinkParity(t, local, store)
	// Refresh retained metadata, including an already retained row.
	_, err = pg.ExecContext(ctx, `UPDATE commit_links SET subject='stale' WHERE session_id='commit-delete'`)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	dbtest.AssertCommitLinkParity(t, local, store)
	// PostgreSQL's restore decision reveals the prompt copied while locally trashed.
	_, err = store.RestoreSession(ctx, "commit-trash")
	require.NoError(t, err)
	links, err = store.SessionCommitLinks(ctx, "commit-trash")
	require.NoError(t, err)
	require.Len(t, links.Commits, 1)
	require.NotNil(t, links.Commits[0].Turn)
	assert.False(t, links.Commits[0].Missing)
	assert.Equal(t, "Save the change", links.Commits[0].Turn.Text)
	_, err = pg.ExecContext(ctx, `DROP TABLE commit_links`)
	require.NoError(t, err)
	_, err = store.SessionCommitLinks(ctx, "commit-live")
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	_, err = store.CommitSessionLinks(ctx, []string{"abc1234"})
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	_, hosted := any(&HostedStore{}).(db.CommitLinkReader)
	assert.False(t, hosted)
}

func TestPGCommitLinkScopeAndOwnership(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_link_scope_test")
	dbtest.SeedCommitLinks(t, local)
	syncer.projects = []string{"commit-project"}
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, local.DeleteSession(ctx, "commit-delete"))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	dbtest.AssertCommitLinkParity(t, local, store)
	// A resident row belonging to another archive must keep its own summary.
	_, err = pg.ExecContext(ctx, `UPDATE sessions SET owner_marker='another-archive' WHERE id='commit-live'`)
	require.NoError(t, err)
	require.NoError(t, local.DeleteSession(ctx, "commit-live"))
	_, err = pg.ExecContext(ctx, `UPDATE commit_links SET subject='Other archive' WHERE session_id='commit-live'`)
	require.NoError(t, err)
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	links, err := store.SessionCommitLinks(ctx, "commit-live")
	require.NoError(t, err)
	require.Len(t, links.Commits, 1)
	assert.Equal(t, "Other archive", links.Commits[0].Subject)
	assert.False(t, links.Commits[0].Missing)
	// Moving a local session out of scope removes its visible link.
	session, err := local.GetSession(ctx, "commit-clash")
	require.NoError(t, err)
	session.Project = "other"
	require.NoError(t, local.UpsertSession(ctx, *session))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	links, err = store.SessionCommitLinks(ctx, "commit-clash")
	require.NoError(t, err)
	assert.Empty(t, links.Commits)
	// Narrowing scope removes retained rows using their stored project.
	syncer.projects = []string{"other"}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	links, err = store.SessionCommitLinks(ctx, "commit-delete")
	require.NoError(t, err)
	assert.Empty(t, links.Commits)
}
