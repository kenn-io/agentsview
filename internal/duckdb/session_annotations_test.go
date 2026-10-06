//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"path/filepath"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAnnotatedPushFixture seeds three local sessions: sess-1 and sess-2
// carry pull request links and labels, sess-3 carries neither. It returns
// the local db and an unpushed mirror path.
func newAnnotatedPushFixture(t *testing.T) (*db.DB, string) {
	t.Helper()
	ctx := t.Context()
	local := newLocalDB(t)
	prLinks := map[string][]db.PRLink{
		"sess-1": {
			{
				URL:        "https://github.com/Example-Org/Widgets/pull/12",
				Host:       "github.com",
				Repository: "Example-Org/Widgets",
				Number:     12,
				Source:     "transcript",
			},
			{
				URL:        "https://github.com/example-org/gadgets/pull/7",
				Host:       "github.com",
				Repository: "example-org/gadgets",
				Number:     7,
				Source:     "transcript",
			},
		},
		"sess-2": {{
			URL:        "https://github.com/example-org/widgets/pull/13",
			Host:       "github.com",
			Repository: "example-org/widgets",
			Number:     13,
			Source:     "transcript",
		}},
	}
	writes := make([]db.SessionBatchWrite, 0, 3)
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("sess-%d", i)
		ts := fmt.Sprintf("2026-02-01T00:%02d:00.000Z", i)
		sess := syncSession(id, "alpha", "first "+id, ts, 2)
		sess.PRLinks = prLinks[id]
		writes = append(writes, db.SessionBatchWrite{
			Session: sess,
			Messages: []db.Message{
				syncMessage(id, 0, "user", "first "+id, ts),
				syncMessage(id, 1, "assistant", "reply "+id, ts),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(ctx, writes)
	require.NoError(t, err)
	_, err = local.SetSessionLabels(ctx, "sess-1", []string{"worker", "ticket-42"})
	require.NoError(t, err)
	_, err = local.SetSessionLabels(ctx, "sess-2", []string{"worker"})
	require.NoError(t, err)
	return local, filepath.Join(t.TempDir(), "mirror.duckdb")
}

func openPushedStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := NewStore(t.Context(), path)
	require.NoError(t, err)
	return store
}

func listSessionIDs(t *testing.T, store *Store, f db.SessionFilter) []string {
	t.Helper()
	page, err := store.ListSessions(t.Context(), f)
	require.NoError(t, err)
	ids := make([]string, 0, len(page.Sessions))
	for _, sess := range page.Sessions {
		ids = append(ids, sess.ID)
	}
	return ids
}

func TestPushMirrorsSessionLabelsAndPRLinks(t *testing.T) {
	ctx := t.Context()
	local, path := newAnnotatedPushFixture(t)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	localSess, err := local.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, localSess)
	require.Len(t, localSess.PRLinks, 2)

	got, err := store.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, localSess.PRLinks, got.PRLinks)
	assert.Equal(t, []string{"ticket-42", "worker"}, got.Labels)

	full, err := store.GetSessionFull(ctx, "sess-2")
	require.NoError(t, err)
	require.NotNil(t, full)
	assert.Equal(t, []string{"worker"}, full.Labels)
	require.Len(t, full.PRLinks, 1)
	assert.Equal(t, 13, full.PRLinks[0].Number)

	bare, err := store.GetSession(ctx, "sess-3")
	require.NoError(t, err)
	require.NotNil(t, bare)
	assert.Nil(t, bare.PRLinks)
	assert.Nil(t, bare.Labels)

	page, err := store.ListSessions(ctx, db.SessionFilter{})
	require.NoError(t, err)
	byID := make(map[string]db.Session, len(page.Sessions))
	for _, sess := range page.Sessions {
		byID[sess.ID] = sess
	}
	require.Contains(t, byID, "sess-1")
	assert.Equal(t, localSess.PRLinks, byID["sess-1"].PRLinks)
	assert.Equal(t, []string{"ticket-42", "worker"}, byID["sess-1"].Labels)
}

func TestDuckDBFiltersSessionsByLabelAndPR(t *testing.T) {
	ctx := t.Context()
	local, path := newAnnotatedPushFixture(t)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	tests := []struct {
		name   string
		filter db.SessionFilter
		want   []string
	}{
		{
			name:   "one label",
			filter: db.SessionFilter{Labels: []string{"worker"}},
			want:   []string{"sess-1", "sess-2"},
		},
		{
			name:   "every listed label",
			filter: db.SessionFilter{Labels: []string{"worker", "ticket-42"}},
			want:   []string{"sess-1"},
		},
		{
			name:   "unknown label",
			filter: db.SessionFilter{Labels: []string{"missing"}},
			want:   []string{},
		},
		{
			name: "repository ignores case",
			filter: db.SessionFilter{
				PR: db.PRFilter{Repository: "EXAMPLE-ORG/widgets"},
			},
			want: []string{"sess-1", "sess-2"},
		},
		{
			name: "repository and number",
			filter: db.SessionFilter{
				PR: db.PRFilter{Repository: "example-org/widgets", Number: 13},
			},
			want: []string{"sess-2"},
		},
		{
			name: "second link on a session",
			filter: db.SessionFilter{
				PR: db.PRFilter{Repository: "example-org/gadgets", Number: 7},
			},
			want: []string{"sess-1"},
		},
		{
			name: "number on another repository",
			filter: db.SessionFilter{
				PR: db.PRFilter{Repository: "example-org/gadgets", Number: 12},
			},
			want: []string{},
		},
		{
			name: "url on another host",
			filter: db.SessionFilter{
				PR: db.PRFilter{Host: "forge.example.com", Repository: "example-org/widgets", Number: 13},
			},
			want: []string{},
		},
		{
			name: "url on the stored host",
			filter: db.SessionFilter{
				PR: db.PRFilter{Host: "github.com", Repository: "example-org/widgets", Number: 13},
			},
			want: []string{"sess-2"},
		},
		{
			name: "label and pr together",
			filter: db.SessionFilter{
				Labels: []string{"worker"},
				PR:     db.PRFilter{Repository: "example-org/widgets", Number: 12},
			},
			want: []string{"sess-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, listSessionIDs(t, store, tt.filter))

			sidebar, err := store.GetSidebarSessionIndex(ctx, tt.filter)
			require.NoError(t, err)
			sidebarIDs := make([]string, 0, len(sidebar.Sessions))
			for _, row := range sidebar.Sessions {
				sidebarIDs = append(sidebarIDs, row.ID)
			}
			assert.ElementsMatch(t, tt.want, sidebarIDs)
			assert.Equal(t, len(tt.want), sidebar.Total)

			localPage, err := local.ListSessions(ctx, tt.filter)
			require.NoError(t, err)
			localIDs := make([]string, 0, len(localPage.Sessions))
			for _, sess := range localPage.Sessions {
				localIDs = append(localIDs, sess.ID)
			}
			assert.ElementsMatch(t, localIDs, tt.want,
				"the mirror must select the same sessions as the archive")
		})
	}
}

func TestPushIncrementalMirrorsLabelOnlyChange(t *testing.T) {
	ctx := t.Context()
	local, path := newAnnotatedPushFixture(t)
	_, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	_, err = local.UpdateSessionLabels(ctx, "sess-3", []string{"late"}, nil)
	require.NoError(t, err)
	res, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Diagnostics.Full)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total,
		"a label-only change must re-push exactly that session")

	store := openPushedStore(t, path)
	got, err := store.GetSession(ctx, "sess-3")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"late"}, got.Labels)
	assert.Equal(t, []string{"sess-3"},
		listSessionIDs(t, store, db.SessionFilter{Labels: []string{"late"}}))
	require.NoError(t, store.Close())

	_, err = local.SetSessionLabels(ctx, "sess-1", nil)
	require.NoError(t, err)
	res, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Diagnostics.PushedSessions.Total)

	store = openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cleared, err := store.GetSession(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, cleared)
	assert.Nil(t, cleared.Labels)
	assert.Empty(t,
		listSessionIDs(t, store, db.SessionFilter{Labels: []string{"ticket-42"}}))
	assert.Len(t, cleared.PRLinks, 2,
		"clearing labels must not drop the session's pull request links")
}

func TestDuckDBChildLabelSelectsChildAndKeepsTree(t *testing.T) {
	ctx := t.Context()
	local, path := newAnnotatedPushFixture(t)
	// sess-3 becomes a launched worker of sess-1 and carries its own label.
	_, err := local.SetSessionExternalParent(ctx, "sess-3", "sess-1", "")
	require.NoError(t, err)
	_, err = local.SetSessionLabels(ctx, "sess-3", []string{"reviewer"})
	require.NoError(t, err)
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	f := db.SessionFilter{Labels: []string{"reviewer"}}
	assert.Equal(t, []string{"sess-3"}, listSessionIDs(t, store, f))

	sidebar, err := store.GetSidebarSessionIndex(ctx, f)
	require.NoError(t, err)
	ids := make([]string, 0, len(sidebar.Sessions))
	for _, row := range sidebar.Sessions {
		ids = append(ids, row.ID)
	}
	assert.ElementsMatch(t, []string{"sess-1", "sess-3"}, ids)
	assert.Equal(t, 1, sidebar.Total)
}
