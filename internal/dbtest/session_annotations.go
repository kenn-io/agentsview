package dbtest

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Session IDs written by SeedSessionAnnotations.
const (
	AnnotatedLauncherID = "annotated-launcher"
	AnnotatedWorkerID   = "annotated-worker"
	AnnotatedOtherID    = "annotated-other"
	AnnotatedPlainID    = "annotated-plain"
)

// AnnotatedPRLink returns the GitHub pull request link the annotation
// fixtures use.
func AnnotatedPRLink(repo string, number int) db.PRLink {
	return db.PRLink{
		URL:  "https://github.com/" + repo + "/pull/" + strconv.Itoa(number),
		Host: "github.com", Repository: repo, Number: number,
	}
}

// SeedSessionAnnotations writes a launcher with a launched worker, an
// unrelated labeled session, and a plain session, each with one message,
// then records labels and the worker's parent through the archive API the
// way a launcher would.
func SeedSessionAnnotations(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := t.Context()
	seed := func(id, startedAt string, prLinks ...db.PRLink) {
		SeedSessionWithMessages(t, d, id, "annotations",
			[]db.Message{{
				SessionID: id, Ordinal: 0, Role: "user",
				Content: "hello", ContentLength: 5, Timestamp: startedAt,
			}},
			func(s *db.Session) {
				s.MessageCount = 1
				s.UserMessageCount = 1
				s.StartedAt = Ptr(startedAt)
				s.PRLinks = prLinks
			})
	}
	seed(AnnotatedLauncherID, "2026-01-01T00:00:00Z",
		AnnotatedPRLink("Acme/Widgets", 7))
	seed(AnnotatedWorkerID, "2026-01-01T00:05:00Z")
	seed(AnnotatedOtherID, "2026-01-01T00:10:00Z",
		AnnotatedPRLink("acme/widgets", 8), AnnotatedPRLink("other/repo", 3))
	seed(AnnotatedPlainID, "2026-01-01T00:15:00Z")

	for id, labels := range map[string][]string{
		AnnotatedLauncherID: {"ticket=ABC-1", "role=lead"},
		AnnotatedWorkerID:   {"role=worker"},
		AnnotatedOtherID:    {"ticket=XYZ-9"},
	} {
		_, err := d.SetSessionLabels(ctx, id, labels)
		require.NoError(t, err, "SetSessionLabels %s", id)
	}
	_, err := d.SetSessionExternalParent(ctx, AnnotatedWorkerID, AnnotatedLauncherID)
	require.NoError(t, err, "SetSessionExternalParent")
}

// annotationFilterCases cover label and pull request matching, direct
// child selection in flat lists, and tree selection in sidebars.
var annotationFilterCases = []struct {
	name   string
	filter db.SessionFilter
}{
	{"one label", db.SessionFilter{Labels: []string{"ticket=ABC-1"}}},
	{"every label", db.SessionFilter{Labels: []string{"ticket=ABC-1", "role=lead"}}},
	{"unknown label", db.SessionFilter{Labels: []string{"missing"}}},
	{"worker label", db.SessionFilter{Labels: []string{"role=worker"}}},
	{"worker label tree", db.SessionFilter{
		Labels: []string{"role=worker"}, IncludeChildren: true,
	}},
	{"repository ignores case", db.SessionFilter{PR: db.PRFilter{Repository: "ACME/widgets"}}},
	{"second link", db.SessionFilter{PR: db.PRFilter{Repository: "other/repo", Number: 3}}},
	{"number from another repository", db.SessionFilter{PR: db.PRFilter{
		Repository: "other/repo", Number: 7,
	}}},
	{"label and pull request", db.SessionFilter{
		Labels: []string{"ticket=ABC-1"}, PR: db.PRFilter{Repository: "acme/widgets", Number: 7},
	}},
}

// AssertSessionAnnotationParity checks that a mirror pushed from an archive
// seeded by SeedSessionAnnotations reads labels and pull request links back
// and selects the same sessions as the archive for every annotation filter,
// in flat lists and in paged and unpaged sidebars.
func AssertSessionAnnotationParity(t *testing.T, archive, mirror db.Store) {
	t.Helper()
	ctx := t.Context()

	launcher, err := mirror.GetSession(ctx, AnnotatedLauncherID)
	require.NoError(t, err)
	require.NotNil(t, launcher)
	assert.Equal(t, []string{"role=lead", "ticket=ABC-1"}, launcher.Labels)
	assert.Equal(t, []db.PRLink{AnnotatedPRLink("Acme/Widgets", 7)}, launcher.PRLinks)
	worker, err := mirror.GetSession(ctx, AnnotatedWorkerID)
	require.NoError(t, err)
	require.NotNil(t, worker)
	require.NotNil(t, worker.ParentSessionID)
	assert.Equal(t, AnnotatedLauncherID, *worker.ParentSessionID)
	plain, err := mirror.GetSession(ctx, AnnotatedPlainID)
	require.NoError(t, err)
	require.NotNil(t, plain)
	assert.Nil(t, plain.Labels)
	assert.Nil(t, plain.PRLinks)

	sortedIDs := func(sessions []db.Session) []string {
		ids := make([]string, 0, len(sessions))
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		slices.Sort(ids)
		return ids
	}
	sidebarIDs := func(rows []db.SidebarSessionIndexRow) []string {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids
	}
	for _, tt := range annotationFilterCases {
		t.Run(tt.name, func(t *testing.T) {
			want, err := archive.ListSessions(ctx, tt.filter)
			require.NoError(t, err)
			got, err := mirror.ListSessions(ctx, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, sortedIDs(want.Sessions), sortedIDs(got.Sessions), "list")
			assert.Equal(t, want.Total, got.Total, "list total")

			for _, limit := range []int{0, 10} {
				f := tt.filter
				f.Limit = limit
				want, err := archive.GetSidebarSessionIndex(ctx, f)
				require.NoError(t, err)
				got, err := mirror.GetSidebarSessionIndex(ctx, f)
				require.NoError(t, err)
				assert.Equal(t, sidebarIDs(want.Sessions), sidebarIDs(got.Sessions),
					"sidebar limit %d", limit)
				assert.Equal(t, want.Total, got.Total, "sidebar total limit %d", limit)
			}
		})
	}
}
