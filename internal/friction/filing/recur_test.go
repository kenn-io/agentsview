package filing_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func seedRecurrence(t *testing.T, d *db.DB, date, id string, at *time.Time) {
	t.Helper()
	dbtest.SeedSession(t, d, id, "proj")
	session, err := d.GetSession(t.Context(), id)
	require.NoError(t, err)
	applied, err := d.ReplaceSessionFrictionAtRevision(t.Context(), *session, db.SessionFrictionUpdate{
		RulesVersion: friction.RulesVersion, Findings: []db.FrictionFinding{{SessionID: id, Kind: string(correction.Kind), Detector: correction.Detector, Fingerprint: correction.Fingerprint(), OccurredAt: at, RulesVersion: friction.RulesVersion}},
	})
	require.NoError(t, err)
	require.True(t, applied)
	digest, err := d.GetFrictionDigest(t.Context(), date)
	require.NoError(t, err)
	if digest == nil {
		digest = &db.FrictionDigest{Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion, BuiltAt: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("digest"), MarkdownSHA256: "sha", RunID: "run"}
	}
	digest.Revision++
	require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, []db.FrictionDigestSubject{{SubjectID: id, Date: date, SubjectKind: "session"}}, []db.FrictionPatternUpdate{{Fingerprint: correction.Fingerprint(), Kind: string(correction.Kind), Title: correction.Title(), Date: date, SubjectID: id, Occurrences: 1}}))
}

func TestReopenRecurringPattern(t *testing.T) {
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, date, status, reason string
		occurred                   *time.Time
		closed                     *time.Time
		want                       bool
	}{
		{"catchup", "2026-09-14", "closed", "done", new(closed.Add(-time.Hour)), &closed, false},
		{"pre-close next day", "2026-09-16", "closed", "done", new(closed.Add(-time.Hour)), &closed, false},
		{"same day after close", "2026-09-15", "closed", "done", new(closed.Add(time.Hour)), &closed, true},
		{"missing time later day", "2026-09-16", "closed", "done", nil, &closed, false},
		{"wontfix", "2026-09-16", "closed", "wontfix", nil, &closed, false},
		{"missing close", "2026-09-16", "closed", "done", nil, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			seedRecurrence(t, d, tt.date, "claude:s1", tt.occurred)
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: tt.status, ClosedReason: tt.reason, ClosedAt: tt.closed})
			_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
			require.NoError(t, err)
			n, err := f.Reopen(t.Context(), "2026-09-01", nil)
			require.NoError(t, err)
			got := mustIssue(t, s, is.UID)
			if tt.want {
				assert.Equal(t, 1, n)
				assert.Equal(t, "open", got.Status)
				require.Len(t, got.Comments, 1)
			} else {
				assert.Zero(t, n)
				assert.Empty(t, got.Comments)
			}
		})
	}
}

func TestReopenReportsOccurrenceDate(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	occurred := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-17", "claude:s1", &occurred)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)

	n, err := f.Reopen(t.Context(), "2026-09-01", time.FixedZone("UTC-4", -4*60*60))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "This pattern recurred on 2026-09-16.", got.Comments[0])
}

func TestReopenFindingInEarlierDigest(t *testing.T) {
	const date = "2026-09-01"
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	occurred := time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, date, "claude:s1", &occurred)
	seedRecurrence(t, d, "2026-09-10", "claude:s2", nil)
	require.Equal(t, "2026-09-10", patternRow(t, d, correction.Fingerprint()).LastSeenDate)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)

	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "This pattern recurred on 2026-09-15.", got.Comments[0])
}

func TestReopenClosuresWithinSameSecond(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 100, time.UTC)
	occurred := closed.Add(time.Hour)
	seedRecurrence(t, d, "2026-09-15", "claude:s1", &occurred)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	s.CloseIssue(is.UID, closed.Add(100*time.Nanosecond))
	n, err = f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, mustIssue(t, s, is.UID).Comments, 2)
}

func TestReopenEarlierSurvivingDigest(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(time.Hour)))
	seedRecurrence(t, d, "2026-09-17", "claude:s2", new(closed.Add(48*time.Hour)))
	require.NoError(t, d.SoftDeleteSession(t.Context(), "claude:s2"))
	require.Equal(t, "2026-09-17", patternRow(t, d, correction.Fingerprint()).LastSeenDate)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	require.Len(t, got.Comments, 1)
	assert.Contains(t, got.Comments[0], "2026-09-15")
}

func TestReopenReadsAllFindingPages(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-15", "claude:s1", nil)
	session, err := d.GetSession(t.Context(), "claude:s1")
	require.NoError(t, err)
	findings := make([]db.FrictionFinding, db.MaxFrictionListLimit+1)
	for i := range findings {
		findings[i] = db.FrictionFinding{SessionID: "claude:s1", Kind: string(correction.Kind), Detector: correction.Detector, Fingerprint: correction.Fingerprint(), Seq: i, RulesVersion: friction.RulesVersion}
	}
	findings[len(findings)-1].OccurredAt = new(closed.Add(time.Hour))
	applied, err := d.ReplaceSessionFrictionAtRevision(t.Context(), *session, db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Findings: findings})
	require.NoError(t, err)
	require.True(t, applied)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err = f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "open", mustIssue(t, s, is.UID).Status)
}

func TestReopenRetry(t *testing.T) {
	for _, scenario := range []string{"outage", "retry"} {
		t.Run(scenario, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(26*time.Hour)))
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
			_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
			require.NoError(t, err)
			s.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})
			n, err := f.Reopen(t.Context(), "2026-09-01", nil)
			require.NoError(t, err)
			if scenario == "outage" {
				assert.Zero(t, n)
				assert.Equal(t, "closed", mustIssue(t, s, is.UID).Status)
				return
			}
			n, err = f.Reopen(t.Context(), "2026-09-01", nil)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			got := mustIssue(t, s, is.UID)
			assert.Equal(t, "open", got.Status)
			assert.Len(t, got.Comments, 1)
		})
	}
}

func TestReopenWhenKataNotReady(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, "missing", correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(time.Hour)))
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	require.NoError(t, d.SaveFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: correction.Fingerprint(), KataInstanceUID: "instance", KataProjectUID: "project", IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, Source: db.FrictionIssueSourceManual, LinkedAt: closed}))
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "closed", got.Status)
	assert.Empty(t, got.Comments)
}
