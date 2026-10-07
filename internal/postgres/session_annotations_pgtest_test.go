//go:build pgtest

package postgres

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func annotatedTestPRLink(repo string, number int) db.PRLink {
	return db.PRLink{
		URL:        "https://github.com/" + repo + "/pull/" + strconv.Itoa(number),
		Host:       "github.com",
		Repository: repo,
		Number:     number,
		Source:     "transcript",
	}
}

// seedAnnotatedSession writes a session with one message, then its labels
// through the archive label API, the way a launcher would.
func seedAnnotatedSession(
	t *testing.T, local *db.DB, sess db.Session, labels ...string,
) {
	t.Helper()
	sess.Project = "proj"
	sess.Machine = "workstation"
	sess.Agent = "claude"
	sess.MessageCount = 1
	sess.UserMessageCount = 1
	sess.CreatedAt = "2026-01-01T00:00:00Z"
	if sess.StartedAt == nil {
		sess.StartedAt = strPtr("2026-01-01T00:00:00Z")
	}
	require.NoError(t, local.UpsertSession(t.Context(), sess), "UpsertSession")
	require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
		SessionID: sess.ID, Ordinal: 0, Role: "user",
		Content: "hello", ContentLength: 5,
	}}), "InsertMessages")
	if len(labels) > 0 {
		_, err := local.SetSessionLabels(t.Context(), sess.ID, labels)
		require.NoError(t, err, "SetSessionLabels")
	}
}

func sortedSessionIDs(sessions []db.Session) []string {
	ids := sessionIDs(sessions)
	slices.Sort(ids)
	return ids
}

func sidebarIDs(rows []db.SidebarSessionIndexRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestPGSessionPRLinksAndLabelsReadAndFilter(t *testing.T) {
	syncer, local, _, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_test",
	)
	pgURL := testPGURL(t)

	seedAnnotatedSession(t, local, db.Session{
		ID: "sid-a",
		PRLinks: []db.PRLink{
			annotatedTestPRLink("acme/widgets", 7),
		},
	}, "ticket=ABC-1", "role=lead")
	seedAnnotatedSession(t, local, db.Session{
		ID: "sid-a-child", ParentSessionID: strPtr("sid-a"),
		RelationshipType: "subagent",
		StartedAt:        strPtr("2026-01-01T00:05:00Z"),
	}, "role=worker")
	seedAnnotatedSession(t, local, db.Session{
		ID: "sid-b",
		PRLinks: []db.PRLink{
			annotatedTestPRLink("acme/widgets", 8),
			annotatedTestPRLink("other/repo", 3),
		},
	}, "ticket=XYZ-9")
	seedAnnotatedSession(t, local, db.Session{ID: "sid-plain"})

	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push")

	store, err := NewStore(pgURL, syncer.schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	t.Run("get paths match the archive", func(t *testing.T) {
		got, err := store.GetSession(ctx, "sid-a")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, []string{"role=lead", "ticket=ABC-1"}, got.Labels)
		require.Len(t, got.PRLinks, 1)
		assert.Equal(t, "acme/widgets", got.PRLinks[0].Repository)
		assert.Equal(t, 7, got.PRLinks[0].Number)

		plain, err := store.GetSession(ctx, "sid-plain")
		require.NoError(t, err)
		require.NotNil(t, plain)
		assert.Nil(t, plain.Labels)
		assert.Nil(t, plain.PRLinks)
	})

	tests := []struct {
		name    string
		filter  db.SessionFilter
		wantIDs []string
	}{
		{
			name:    "one label",
			filter:  db.SessionFilter{Labels: []string{"ticket=ABC-1"}},
			wantIDs: []string{"sid-a"},
		},
		{
			name:    "child label selects the child directly",
			filter:  db.SessionFilter{Labels: []string{"role=worker"}},
			wantIDs: []string{"sid-a-child"},
		},
		{
			name: "child label keeps its root's tree",
			filter: db.SessionFilter{
				Labels: []string{"role=worker"}, IncludeChildren: true,
			},
			wantIDs: []string{"sid-a", "sid-a-child"},
		},
		{
			name: "matching root keeps its subagents",
			filter: db.SessionFilter{
				Labels: []string{"ticket=ABC-1"}, IncludeChildren: true,
			},
			wantIDs: []string{"sid-a", "sid-a-child"},
		},
		{
			name:    "pr repository",
			filter:  db.SessionFilter{PR: db.PRFilter{Repository: "acme/widgets"}},
			wantIDs: []string{"sid-a", "sid-b"},
		},
		{
			name:    "pr repository ignores case",
			filter:  db.SessionFilter{PR: db.PRFilter{Repository: "ACME/Widgets"}},
			wantIDs: []string{"sid-a", "sid-b"},
		},
		{
			name: "pr url on another host",
			filter: db.SessionFilter{PR: db.PRFilter{
				Host: "forge.example.com", Repository: "acme/widgets", Number: 8,
			}},
			wantIDs: []string{},
		},
		{
			name: "pr url on the stored host",
			filter: db.SessionFilter{PR: db.PRFilter{
				Host: "github.com", Repository: "acme/widgets", Number: 8,
			}},
			wantIDs: []string{"sid-b"},
		},
		{
			name: "pr number in another repository",
			filter: db.SessionFilter{PR: db.PRFilter{
				Repository: "other/repo", Number: 7,
			}},
			wantIDs: []string{},
		},
	}
	for _, tt := range tests {
		t.Run("list "+tt.name, func(t *testing.T) {
			f := tt.filter
			got, err := store.ListSessions(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, tt.wantIDs, sortedSessionIDs(got.Sessions))
			assert.Equal(t, len(tt.wantIDs), got.Total)

			want, err := local.ListSessions(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, sortedSessionIDs(want.Sessions), sortedSessionIDs(got.Sessions),
				"PostgreSQL and SQLite must select the same sessions")
		})
		t.Run("sidebar "+tt.name, func(t *testing.T) {
			got, err := store.GetSidebarSessionIndex(ctx, tt.filter)
			require.NoError(t, err)
			want, err := local.GetSidebarSessionIndex(ctx, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, sidebarIDs(want.Sessions), sidebarIDs(got.Sessions),
				"PostgreSQL and SQLite sidebars must select the same sessions")
			assert.Equal(t, want.Total, got.Total)

			paged := tt.filter
			paged.Limit = 10
			gotPage, err := store.GetSidebarSessionIndex(ctx, paged)
			require.NoError(t, err)
			wantPage, err := local.GetSidebarSessionIndex(ctx, paged)
			require.NoError(t, err)
			assert.Equal(t, sidebarIDs(wantPage.Sessions), sidebarIDs(gotPage.Sessions),
				"paged sidebars must select the same sessions")
		})
	}
}

func TestPGPushRepushesAnnotationOnlyChanges(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_repush_test",
	)

	sess := db.Session{
		ID: "sid-repush",
		PRLinks: []db.PRLink{
			annotatedTestPRLink("acme/widgets", 1),
		},
	}
	seedAnnotatedSession(t, local, sess, "ticket=ABC-1")
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err, "initial Push")

	readPG := func(t *testing.T) (string, []string) {
		t.Helper()
		var prLinks string
		var labels []string
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT pr_links, array_to_json(labels)::text
			 FROM sessions WHERE id = $1`, sess.ID,
		).Scan(&prLinks, db.LabelsScanner(&labels)))
		return prLinks, labels
	}

	unchanged, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "unchanged Push")
	assert.Equal(t, 0, unchanged.SessionsPushed)

	_, err = local.UpdateSessionLabels(ctx, sess.ID, []string{"role=lead"}, nil)
	require.NoError(t, err, "UpdateSessionLabels")
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "label-only Push")
	assert.Equal(t, 1, result.SessionsPushed)
	_, labels := readPG(t)
	assert.Equal(t, []string{"role=lead", "ticket=ABC-1"}, labels)

	stored, err := local.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	// A new pr-link record arrives with a transcript write, which moves the
	// source mtime into the push window; mtime is not part of the push
	// fingerprint, so only the link change can trigger the re-push.
	stored.PRLinks = append(stored.PRLinks, annotatedTestPRLink("acme/widgets", 2))
	stored.FileMtime = new(time.Now().UnixNano())
	require.NoError(t, local.UpsertSession(ctx, *stored), "UpsertSession")
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "pr-link-only Push")
	assert.Equal(t, 1, result.SessionsPushed)
	prLinks, _ := readPG(t)
	assert.Equal(t, db.EncodePRLinks(stored.PRLinks), prLinks)

	_, err = local.SetSessionLabels(ctx, sess.ID, nil)
	require.NoError(t, err, "clear labels")
	result, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "label-clearing Push")
	assert.Equal(t, 1, result.SessionsPushed)
	_, labels = readPG(t)
	assert.Nil(t, labels)
}

func TestPGPushAddsAnnotationColumnsToOlderSchema(t *testing.T) {
	syncer, local, pg, ctx := newSessionProvenancePushSync(
		t, "agentsview_session_annotations_upgrade_test",
	)
	pgURL := testPGURL(t)

	_, err := pg.ExecContext(ctx,
		`ALTER TABLE sessions DROP COLUMN pr_links, DROP COLUMN labels`)
	require.NoError(t, err, "drop annotation columns")
	require.False(t, pushSchemaCurrent(ctx, pg),
		"a schema without the annotation columns is not current")

	seedAnnotatedSession(t, local, db.Session{
		ID:      "sid-upgrade",
		PRLinks: []db.PRLink{annotatedTestPRLink("acme/widgets", 4)},
	}, "ticket=UP-1")
	syncer.schemaDone = false
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "Push migrates the schema")
	assert.True(t, pushSchemaCurrent(ctx, pg))

	store, err := NewStore(pgURL, syncer.schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	got, err := store.GetSession(ctx, "sid-upgrade")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"ticket=UP-1"}, got.Labels)
	require.Len(t, got.PRLinks, 1)
	assert.Equal(t, 4, got.PRLinks[0].Number)
}
