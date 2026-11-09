package filing_test

import (
	"context"
	"net/http"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

type findingStore struct {
	filing.Store
	findings []db.FrictionFinding
}

func (s findingStore) ListFrictionFindings(ctx context.Context, f db.FrictionFindingFilter) ([]db.FrictionFinding, string, error) {
	return s.findings, "", nil
}

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
			f.Store = findingStore{Store: d, findings: []db.FrictionFinding{{}}}
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
	f.Store = findingStore{Store: d, findings: []db.FrictionFinding{{}}}
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

func TestReopenUsesFindingTime(t *testing.T) {
	closed := time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, date string
		occurred   time.Time
		want       bool
	}{
		{"session crosses midnight", "2026-09-16", time.Date(2026, 9, 15, 10, 5, 0, 0, time.UTC), false},
		{"same day after close", "2026-09-15", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			seedDigestOn(t, d, tt.date, correction)
			f.Store = findingStore{Store: d, findings: []db.FrictionFinding{{OccurredAt: &tt.occurred}}}
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
			_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
			require.NoError(t, err)
			n, err := f.Reopen(t.Context(), "2026-09-01", nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, n == 1)
			assert.Equal(t, tt.want, mustIssue(t, s, is.UID).Status == "open")
		})
	}
}

type pagedFindingStore struct {
	filing.Store
	t           *testing.T
	fingerprint string
	occurred    time.Time
}

func (s pagedFindingStore) ListFrictionFindings(_ context.Context, f db.FrictionFindingFilter) ([]db.FrictionFinding, string, error) {
	assert.Equal(s.t, s.fingerprint, f.Fingerprint)
	assert.Equal(s.t, "2026-09-15", f.Date)
	switch f.Cursor {
	case "":
		return []db.FrictionFinding{{}}, "next", nil
	case "next":
		return []db.FrictionFinding{{OccurredAt: &s.occurred}}, "", nil
	default:
		s.t.Errorf("unexpected cursor %q", f.Cursor)
		return nil, "", nil
	}
}

func TestReopenReadsAllFindingPages(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	seedDigestOn(t, d, "2026-09-15", correction)
	closed := time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)
	f.Store = pagedFindingStore{Store: d, t: t, fingerprint: correction.Fingerprint(), occurred: closed.Add(time.Hour)}
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "open", mustIssue(t, s, is.UID).Status)
}
