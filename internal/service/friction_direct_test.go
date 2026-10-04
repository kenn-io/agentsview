package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func seedServiceDigest(t *testing.T, d *db.DB, date string) {
	t.Helper()
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1,
		SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{\n  \"schema_version\": 3\n}\n"),
		Markdown: []byte("# Friction Log — " + date + "\n"), MarkdownSHA256: "s", RunID: "r",
	}, nil, []db.FrictionPatternUpdate{
		{Fingerprint: "fl1:" + date, Kind: "error", Title: "t", Date: date, SubjectID: "claude:s1", Ordinal: new(2), Occurrences: 1},
	}))
}

func TestDirectFrictionService(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	svc := service.NewDirectBackend(d, nil)
	require.True(t, service.SupportsFriction(svc))
	require.True(t, service.SupportsFriction(service.NewReadOnlyBackend(d)))
	fs := svc.(service.FrictionService)

	_, err := fs.FrictionDigest(t.Context(), "")
	require.ErrorIs(t, err, service.ErrFrictionDigestNotFound)

	seedServiceDigest(t, d, "2026-09-13")
	seedServiceDigest(t, d, "2026-09-14")
	tests := []struct {
		name, in, wantDate string
		wantErr            error
	}{
		{"latest_by_default", "", "2026-09-14", nil},
		{"explicit", "2026-09-13", "2026-09-13", nil},
		{"missing", "2026-01-01", "", service.ErrFrictionDigestNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := fs.FrictionDigest(t.Context(), tt.in)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantDate, view.Date)
			assert.Equal(t, 1, view.Revision)
			assert.Equal(t, "# Friction Log — "+tt.wantDate+"\n", view.Markdown)
			assert.JSONEq(t, `{"schema_version":3}`, string(view.Summary))
			assert.Empty(t, view.WebURL, "direct reads have no browser address")
		})
	}
	t.Run("invalid_date", func(t *testing.T) {
		_, err := fs.FrictionDigest(t.Context(), "tomorrow")
		require.Error(t, err)
	})

	linked, unlinked := true, false
	patternTests := []struct {
		name   string
		filter service.FrictionPatternFilter
		want   int
	}{
		{"all", service.FrictionPatternFilter{}, 2},
		{"since", service.FrictionPatternFilter{Since: "2026-09-14"}, 1},
		{"linked_empty_until_filing_exists", service.FrictionPatternFilter{Linked: &linked}, 0},
		{"unlinked_all", service.FrictionPatternFilter{Linked: &unlinked}, 2},
		{"limit", service.FrictionPatternFilter{Limit: 1}, 1},
	}
	for _, tt := range patternTests {
		t.Run("patterns_"+tt.name, func(t *testing.T) {
			views, err := fs.FrictionPatterns(t.Context(), tt.filter)
			require.NoError(t, err)
			assert.Len(t, views, tt.want)
		})
	}
}

func TestFrictionPatternViewsCarryIssueLink(t *testing.T) {
	link := &db.FrictionIssueLink{
		Fingerprint: "fl1:a", State: db.FrictionLinkStateLinked,
		QualifiedID: "kata#12", WebURL: "https://kata.example.test/issues/12",
	}
	tests := []struct {
		name       string
		link       *db.FrictionIssueLink
		wantID     string
		wantWebURL string
	}{
		{name: "linked", link: link, wantID: "kata#12", wantWebURL: "https://kata.example.test/issues/12"},
		{name: "unlinked", link: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			views := service.FrictionPatternViews([]db.FrictionPattern{{Fingerprint: "fl1:a", Kind: "error", Link: tt.link}},
				func(string, *int) string { return "" })
			require.Len(t, views, 1)
			assert.Equal(t, tt.wantID, views[0].IssueQualifiedID)
			assert.Equal(t, tt.wantWebURL, views[0].IssueWebURL)
		})
	}
}
