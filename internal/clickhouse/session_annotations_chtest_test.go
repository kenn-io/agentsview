//go:build chtest

package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

var fixturePRLinks = []db.PRLink{
	{
		URL: "https://github.com/Acme/Widgets/pull/42", Host: "github.com",
		Repository: "Acme/Widgets", Number: 42, Source: "transcript",
		FirstSeenAt: "2026-01-10T00:01:00Z",
	},
	{
		URL: "https://gitlab.com/acme/tools/-/merge_requests/7", Host: "gitlab.com",
		Repository: "acme/tools", Number: 7, Source: "transcript",
	},
}

// setLocalPRLinks rewrites a session's PR links in the SQLite archive and
// bumps local_modified_at so an incremental push selects it.
func setLocalPRLinks(t *testing.T, local *db.DB, sessionID string, links []db.PRLink) {
	t.Helper()
	ctx := context.Background()
	sess, err := local.GetSessionFull(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	msgs, err := local.GetAllMessages(ctx, sessionID)
	require.NoError(t, err)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	sess.PRLinks = links
	sess.LocalModifiedAt = &now
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:         *sess,
		Messages:        msgs,
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
}

func listIDs(t *testing.T, store *Store, f db.SessionFilter) []string {
	t.Helper()
	page, err := store.ListSessions(t.Context(), f)
	require.NoError(t, err)
	ids := make([]string, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestPushServesSessionLabelsAndPRLinks(t *testing.T) {
	ctx := t.Context()
	local, target := seedFixture(t)
	setLocalPRLinks(t, local, fixtureAlphaID, fixturePRLinks)
	_, err := local.SetSessionLabels(ctx, fixtureAlphaID, []string{"ticket-1", "role:lead"})
	require.NoError(t, err)
	_, err = local.SetSessionLabels(ctx, fixtureBetaID, []string{"ticket-1"})
	require.NoError(t, err)
	_, err = local.SetSessionLabels(ctx, fixtureChildID, []string{"role:worker"})
	require.NoError(t, err)

	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	t.Run("detail reads", func(t *testing.T) {
		got, err := store.GetSession(ctx, fixtureAlphaID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, fixturePRLinks, got.PRLinks)
		assert.Equal(t, []string{"role:lead", "ticket-1"}, got.Labels)

		beta, err := store.GetSession(ctx, fixtureBetaID)
		require.NoError(t, err)
		require.NotNil(t, beta)
		assert.Nil(t, beta.PRLinks, "a session without links reads back as nil")
		assert.Equal(t, []string{"ticket-1"}, beta.Labels)
	})

	tests := []struct {
		name   string
		filter db.SessionFilter
		want   []string
	}{
		{"one label", db.SessionFilter{Labels: []string{"ticket-1"}},
			[]string{fixtureAlphaID, fixtureBetaID}},
		{"repository ignores case", db.SessionFilter{PR: db.PRFilter{Repository: "acme/widgets"}},
			[]string{fixtureAlphaID}},
		{"repository and number", db.SessionFilter{PR: db.PRFilter{Repository: "acme/tools", Number: 7}},
			[]string{fixtureAlphaID}},
		{"number from another link", db.SessionFilter{PR: db.PRFilter{Repository: "acme/tools", Number: 42}},
			nil},
		{"unlinked repository", db.SessionFilter{PR: db.PRFilter{Repository: "acme/other"}}, nil},
		{"url on another host", db.SessionFilter{PR: db.PRFilter{
			Host: "forge.example.com", Repository: "acme/widgets", Number: 42,
		}}, nil},
		{"url on the stored host", db.SessionFilter{PR: db.PRFilter{
			Host: "github.com", Repository: "acme/widgets", Number: 42,
		}}, []string{fixtureAlphaID}},
	}
	for _, tt := range tests {
		t.Run("filter "+tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, listIDs(t, store, tt.filter))
		})
	}

	t.Run("sidebar index filters by label and pr", func(t *testing.T) {
		index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Labels: []string{"role:lead"}})
		require.NoError(t, err)
		ids := make([]string, 0, len(index.Sessions))
		for _, row := range index.Sessions {
			ids = append(ids, row.ID)
		}
		assert.Contains(t, ids, fixtureAlphaID)
		assert.NotContains(t, ids, fixtureBetaID)
		assert.Equal(t, 1, index.Total)

		index, err = store.GetSidebarSessionIndex(ctx,
			db.SessionFilter{PR: db.PRFilter{Repository: "acme/widgets"}})
		require.NoError(t, err)
		ids = ids[:0]
		for _, row := range index.Sessions {
			ids = append(ids, row.ID)
		}
		assert.Contains(t, ids, fixtureAlphaID)
		assert.NotContains(t, ids, fixtureBetaID)
	})

	t.Run("child label", func(t *testing.T) {
		f := db.SessionFilter{Labels: []string{"role:worker"}}
		assert.Equal(t, []string{fixtureChildID}, listIDs(t, store, f),
			"a flat list selects the labeled child itself")

		index, err := store.GetSidebarSessionIndex(ctx, f)
		require.NoError(t, err)
		ids := make([]string, 0, len(index.Sessions))
		for _, row := range index.Sessions {
			ids = append(ids, row.ID)
		}
		assert.ElementsMatch(t, []string{fixtureAlphaID, fixtureChildID}, ids,
			"the sidebar keeps the tree whose child matches")
		assert.Equal(t, 1, index.Total)

		want, err := local.GetSidebarSessionIndex(ctx, f)
		require.NoError(t, err)
		assert.Equal(t, want.Total, index.Total)
	})
}

func TestPushRepublishesLabelOnlyAndPRLinkChanges(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newPushedStore(t)

	_, err := local.UpdateSessionLabels(ctx, fixtureBetaID, []string{"ticket-9"}, nil)
	require.NoError(t, err)
	res, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "a label-only change re-pushes its session")
	assert.Equal(t, []string{fixtureBetaID}, listIDs(t, store,
		db.SessionFilter{Labels: []string{"ticket-9"}}))

	_, err = local.SetSessionLabels(ctx, fixtureBetaID, nil)
	require.NoError(t, err)
	res, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed, "removing every label re-pushes its session")
	beta, err := store.GetSession(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.NotNil(t, beta)
	assert.Nil(t, beta.Labels)
	assert.Empty(t, listIDs(t, store, db.SessionFilter{Labels: []string{"ticket-9"}}))

	setLocalPRLinks(t, local, fixtureBetaID, fixturePRLinks[:1])
	res, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, []string{fixtureBetaID}, listIDs(t, store,
		db.SessionFilter{PR: db.PRFilter{Repository: "acme/widgets", Number: 42}}))
}

// A mirror pushed before the annotation columns existed gains them in
// place, and its existing rows read back as unlabeled and unlinked.
func TestEnsureSchemaAddsAnnotationColumnsToOlderMirror(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newPushedStore(t)
	conn := store.DB()
	for _, column := range []string{"pr_links", "labels"} {
		_, err := conn.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN "+column)
		require.NoError(t, err)
	}
	err := CheckSchemaCompat(ctx, conn)
	require.Error(t, err)
	assert.ErrorContains(t, err, "sessions.labels")
	assert.ErrorContains(t, err, "sessions.pr_links")

	require.NoError(t, EnsureSchemaOn(ctx, conn))
	require.NoError(t, CheckSchemaCompat(ctx, conn))
	got, err := store.GetSession(ctx, fixtureAlphaID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.Labels)
	assert.Nil(t, got.PRLinks)
}
