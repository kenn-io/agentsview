package filing_test

import (
	"net/http"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func TestReopenRecurringPattern(t *testing.T) {
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	zoneClose := time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)
	la, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	tests := []struct {
		name, date, status, reason string
		closed                     *time.Time
		loc                        *time.Location
		want                       bool
	}{
		{"later", "2026-09-16", "closed", "done", &closed, nil, true},
		{"catchup", "2026-09-14", "closed", "done", &closed, nil, false},
		{"same day", "2026-09-15", "closed", "done", &closed, nil, false},
		{"wontfix", "2026-09-16", "closed", "wontfix", &closed, nil, false},
		{"open", "2026-09-16", "open", "done", &closed, nil, false},
		{"missing close", "2026-09-16", "closed", "done", nil, nil, false},
		{"local day", "2026-09-16", "closed", "done", &zoneClose, la, true},
		{"utc day", "2026-09-16", "closed", "done", &zoneClose, time.UTC, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			if tt.date != digestDate {
				seedDigestOn(t, d, tt.date, correction)
			}
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: tt.status, ClosedReason: tt.reason, ClosedAt: tt.closed})
			_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
			require.NoError(t, err)
			s.ResetRequests()
			n, err := f.Reopen(t.Context(), "2026-09-01", tt.loc)
			require.NoError(t, err)
			if tt.want {
				assert.Equal(t, 1, n)
				got := mustIssue(t, s, is.UID)
				assert.Equal(t, "open", got.Status)
				assert.Nil(t, got.ClosedAt)
				require.Len(t, got.Comments, 1)
				assert.Contains(t, got.Comments[0], tt.date)
				assert.Contains(t, got.Comments[0], "https://av.example.test/base/friction?date="+tt.date)
				_, err = f.Reopen(t.Context(), "2026-09-01", tt.loc)
				require.NoError(t, err)
				assert.Len(t, s.RequestsMatching(http.MethodPost, "/comments"), 1)
				assert.Len(t, s.RequestsMatching(http.MethodPost, "/actions/reopen"), 1)
			} else {
				assert.Zero(t, n)
				assert.Empty(t, s.RequestsMatching(http.MethodPost, "/comments"))
			}
		})
	}
}

func TestReopenRetryDoesNotDuplicateComment(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	seedDigestOn(t, d, "2026-09-16", correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	s.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "closed", mustIssue(t, s, is.UID).Status)
	assert.Equal(t, kata.StateReady, f.Kata.Status(t.Context(), true).State)
	n, err = f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	assert.Len(t, got.Comments, 1)
}

func TestReopenWhenKataNotReady(t *testing.T) {
	s := katatest.New(t)
	f, _ := newFiler(t, s, "missing", correction)
	n, err := f.Reopen(t.Context(), "", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, s.RequestsMatching(http.MethodGet, "/issues"))
}
