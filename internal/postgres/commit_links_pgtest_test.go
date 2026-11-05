//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestPGPurgeWithoutCommitLinksTable(t *testing.T) {
	for _, emptyTrash := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete-session", true: "empty-trash"}[emptyTrash], func(t *testing.T) {
			syncer, _, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_purge_old_test")
			_, err := pg.ExecContext(ctx, `DROP TABLE commit_links;
				INSERT INTO sessions(id,project,machine,agent,deleted_at)
				VALUES ('trashed','project','test','claude',NOW());
				INSERT INTO sessions(id,project,machine,agent)
				VALUES ('live','project','test','claude')`)
			require.NoError(t, err)
			store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			if emptyTrash {
				deleted, err := store.EmptyTrash(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, deleted)
			} else {
				deleted, err := store.DeleteSessionIfTrashed(ctx, "trashed")
				require.NoError(t, err)
				assert.EqualValues(t, 1, deleted)
			}
			var id string
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT id FROM sessions`).Scan(&id))
			assert.Equal(t, "live", id)
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT id FROM excluded_sessions`).Scan(&id))
			assert.Equal(t, "trashed", id)
		})
	}
}

func TestPGPurgeKeepsMissingCommitLinks(t *testing.T) {
	for _, emptyTrash := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete-session", true: "empty-trash"}[emptyTrash], func(t *testing.T) {
			syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_purge_test")
			dbtest.SeedCommitLinks(t, local)
			_, err := syncer.Push(ctx, false, nil)
			require.NoError(t, err)
			store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
			require.NoError(t, err)
			defer store.Close()
			err = store.SoftDeleteSession(ctx, "commit-live")
			require.NoError(t, err)
			if emptyTrash {
				_, err = store.EmptyTrash(ctx)
			} else {
				_, err = store.DeleteSessionIfTrashed(ctx, "commit-live")
			}
			require.NoError(t, err)
			for _, afterPush := range []bool{false, true} {
				if afterPush {
					_, err = syncer.Push(ctx, true, nil)
					require.NoError(t, err)
				}
				links, err := store.SessionCommitLinks(ctx, "commit-live")
				require.NoError(t, err)
				require.Len(t, links.Commits, 1)
				assert.Equal(t, "abc1234567890123456789012345678901234567", links.Commits[0].SHA)
				assert.True(t, links.Commits[0].Missing)
				assert.Nil(t, links.Commits[0].Turn)
				matches, err := store.CommitSessionLinks(ctx, []string{"abc1234567890123456789012345678901234567"})
				require.NoError(t, err)
				assert.Contains(t, matches.Commits[0].Links, links.Commits[0])
				var ordinal sql.NullInt64
				var prompt string
				require.NoError(t, pg.QueryRowContext(ctx, `SELECT turn_ordinal,turn_text FROM commit_links WHERE session_id='commit-live'`).Scan(&ordinal, &prompt))
				assert.False(t, ordinal.Valid)
				assert.Empty(t, prompt)
			}
		})
	}
}

func TestPGIncrementalPushRetainsDeletedCommitLink(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_commit_incremental_test")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	dbtest.SeedSession(t, local, "deleted", "project")
	require.NoError(t, local.ReplaceSessionMessages(ctx, "deleted", []db.Message{
		{SessionID: "deleted", Ordinal: 0, Role: "user", Content: "Save the change"},
		{SessionID: "deleted", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m change"}`, ResultContent: "[main abc1234] Save the change", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Save the change"}}}}},
	}))
	require.NoError(t, local.DeleteSession(ctx, "deleted"))
	localLinks, err := local.SessionCommitLinks(ctx, "deleted")
	require.NoError(t, err)
	require.Len(t, localLinks.Commits, 1)
	assert.True(t, localLinks.Commits[0].Missing)

	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, result.SessionsPushed)
	store, err := NewStore(ctx, testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	matches, err := store.CommitSessionLinks(ctx, []string{"abc1234"})
	require.NoError(t, err)
	require.Len(t, matches.Commits, 1)
	require.Len(t, matches.Commits[0].Links, 1)
	assert.Equal(t, "deleted", matches.Commits[0].Links[0].SessionID)
	assert.True(t, matches.Commits[0].Links[0].Missing)
}

func TestPGCommitLinkParity(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_links_test")
	dbtest.SeedCommitLinks(t, local)
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
	dbtest.AssertCommitLinkParity(t, local, store)
	dbtest.AssertTrailerIncrementalPush(t, local, func() db.CommitLinkReader { return store }, func() {
		_, err := syncer.Push(t.Context(), false, nil)
		require.NoError(t, err)
	})
	_, err = pg.ExecContext(ctx, `UPDATE messages SET content='different revision' WHERE session_id='commit-live' AND ordinal=0`)
	require.NoError(t, err)
	links, err := store.SessionCommitLinks(ctx, "commit-live")
	require.NoError(t, err)
	assert.Equal(t, "Record the parser fix", links.Commits[0].Turn.Text)
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
	// Deleting a published session must replace its live link with a retained link.
	wantDeleted, err := local.SessionCommitLinks(ctx, "commit-delete")
	require.NoError(t, err)
	gotDeleted, err := store.SessionCommitLinks(ctx, "commit-delete")
	require.NoError(t, err)
	assert.Equal(t, wantDeleted, gotDeleted)
	wantLookup, err := local.CommitSessionLinks(ctx, []string{"abc1234567890123456789012345678901234567"})
	require.NoError(t, err)
	gotLookup, err := store.CommitSessionLinks(ctx, []string{"abc1234567890123456789012345678901234567"})
	require.NoError(t, err)
	assert.Equal(t, wantLookup, gotLookup)
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
	assert.Equal(t, "Record the parser fix", links.Commits[0].Turn.Text)
}

func TestPGCommitLinksBeforeUpgradePush(t *testing.T) {
	syncer, _, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_links_old_test")
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = pg.ExecContext(ctx, `DROP TABLE commit_links`)
	require.NoError(t, err)
	_, err = store.SessionCommitLinks(ctx, "commit-live")
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
	_, err = store.CommitSessionLinks(ctx, []string{"abc1234"})
	require.ErrorIs(t, err, db.ErrCommitLinksUnavailable)
}

func TestPGCommitLinkScopeAndOwnership(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(t, "agentsview_commit_link_scope_test")
	dbtest.SeedCommitLinks(t, local)
	syncer.projects = []string{"commit-project"}
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer store.Close()
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
}

func TestPGRetainedCommitLinkScope(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(t, "agentsview_retained_commit_scope_test")
	dbtest.SeedCommitLinks(t, local)
	require.NoError(t, syncer.EnsureSchema(ctx))
	store, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	dbtest.AssertRetainedCommitLinkScope(t, local, func() db.CommitLinkReader { return store }, func(projects, exclude []string) {
		syncer.projects, syncer.excludeProjects = projects, exclude
		_, err := syncer.Push(ctx, true, nil)
		require.NoError(t, err)
	})
}
