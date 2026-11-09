package filing_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func seedRecurrence(t *testing.T, d *db.DB, date, id string, at *time.Time) {
	t.Helper()
	seedRecurrenceSignal(t, d, date, id, at, correction)
}

func seedRecurrenceSignal(t *testing.T, d *db.DB, date, id string, at *time.Time, signal friction.Signal) {
	t.Helper()
	dbtest.SeedSession(t, d, id, "proj")
	session, err := d.GetSession(t.Context(), id)
	require.NoError(t, err)
	applied, err := d.ReplaceSessionFrictionAtRevision(t.Context(), *session, db.SessionFrictionUpdate{
		RulesVersion: friction.RulesVersion, Findings: []db.FrictionFinding{{SessionID: id, Kind: string(signal.Kind), Detector: signal.Detector, Fingerprint: signal.Fingerprint(), OccurredAt: at, RulesVersion: friction.RulesVersion}},
	})
	require.NoError(t, err)
	require.True(t, applied)
	digest, err := d.GetFrictionDigest(t.Context(), date)
	require.NoError(t, err)
	if digest == nil {
		digest = &db.FrictionDigest{Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion, BuiltAt: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), SnapshotJSON: []byte("{}"), SummaryJSON: []byte("{}"), Markdown: []byte("digest"), MarkdownSHA256: "sha", RunID: "run"}
	}
	sig := signal
	sig.SubjectID = id
	if at != nil {
		sig.OccurredAt = *at
	}
	snap, err := review.EncodeSnapshot(review.Snapshot{Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion, Signals: []friction.Signal{sig}})
	require.NoError(t, err)
	digest.SnapshotJSON = snap
	digest.Revision++
	require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, []db.FrictionDigestSubject{{SubjectID: id, Date: date, SubjectKind: "session"}}, []db.FrictionPatternUpdate{{Fingerprint: sig.Fingerprint(), Kind: string(sig.Kind), Title: sig.Title(), Date: date, SubjectID: id, Occurrences: 1}}))
}

func TestReopenRecurringPattern(t *testing.T) {
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, date, reason string
		occurred           *time.Time
		closed             *time.Time
		want               bool
	}{
		{"pre-close next day", "2026-09-16", "done", new(closed.Add(-time.Hour)), &closed, false},
		{"same day after close", "2026-09-15", "done", new(closed.Add(time.Hour)), &closed, true},
		{"missing time later day", "2026-09-16", "done", nil, &closed, false},
		{"wontfix", "2026-09-16", "wontfix", new(closed.Add(25 * time.Hour)), &closed, false},
		{"missing close", "2026-09-16", "done", new(closed.Add(25 * time.Hour)), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			seedRecurrence(t, d, tt.date, "claude:s1", tt.occurred)
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: tt.reason, ClosedAt: tt.closed})
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

type unlinkOnDigestStore struct {
	*db.DB
	fingerprint string
}

func (s unlinkOnDigestStore) GetFrictionDigest(ctx context.Context, date string) (*db.FrictionDigest, error) {
	_, err := s.DeleteFrictionIssueLink(ctx, s.fingerprint)
	if err != nil {
		return nil, err
	}
	return s.DB.GetFrictionDigest(ctx, date)
}

func TestReopenSkipsUnlinkedPattern(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(26*time.Hour)))
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	f.Store = unlinkOnDigestStore{DB: d, fingerprint: correction.Fingerprint()}

	n, err := f.Reopen(t.Context(), "2026-09-16", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Nil(t, patternRow(t, d, correction.Fingerprint()).Issue)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "closed", got.Status)
	assert.Empty(t, got.Comments)
}

func TestReopenUsesFrozenDigest(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-15", "claude:s1", new(closed.Add(-time.Hour)))
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)

	session, err := d.GetSession(t.Context(), "claude:s1")
	require.NoError(t, err)
	applied, err := d.ReplaceSessionFrictionAtRevision(t.Context(), *session, db.SessionFrictionUpdate{
		Hash: "resumed", RulesVersion: friction.RulesVersion, Findings: []db.FrictionFinding{
			{SessionID: "claude:s1", Kind: string(correction.Kind), Detector: correction.Detector, Fingerprint: correction.Fingerprint(), OccurredAt: new(closed.Add(-time.Hour)), RulesVersion: friction.RulesVersion},
			{SessionID: "claude:s1", Kind: string(correction.Kind), Detector: correction.Detector, Fingerprint: correction.Fingerprint(), OccurredAt: new(closed.Add(time.Hour)), Seq: 1, RulesVersion: friction.RulesVersion},
		},
	})
	require.NoError(t, err)
	require.True(t, applied)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "closed", mustIssue(t, s, is.UID).Status)
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
	sig := toolError("go: cannot find main module", "claude:s1")
	f, d := newFiler(t, s, katatest.DefaultProjectName, sig)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrenceSignal(t, d, "2026-09-16", "claude:s1", new(closed.Add(26*time.Hour)), sig)
	seedRecurrenceSignal(t, d, "2026-09-17", "claude:s2", new(closed.Add(50*time.Hour)), sig)
	digest, err := d.GetFrictionDigest(t.Context(), "2026-09-17")
	require.NoError(t, err)
	digest.SnapshotJSON = []byte("{}")
	digest.Revision++
	require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, nil, nil))
	require.Equal(t, "2026-09-17", patternRow(t, d, sig.Fingerprint()).LastSeenDate)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err = f.Link(t.Context(), sig.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err := f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "This pattern recurred on 2026-09-16.", got.Comments[0])
}

func TestReopenRetry(t *testing.T) {
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
	assert.Zero(t, n)
	assert.Equal(t, "closed", mustIssue(t, s, is.UID).Status)
	n, err = f.Reopen(t.Context(), "2026-09-01", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	assert.Len(t, got.Comments, 1)
}

type failingGetKata struct {
	filing.Kata
	err   error
	calls int
}

func (k *failingGetKata) GetIssue(ctx context.Context, ref string) (kata.Issue, error) {
	k.calls++
	return kata.Issue{}, k.err
}

func TestReopenWhenKataNotReady(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{{"not ready", kata.ErrNotReady}, {"service unavailable", &kata.APIError{Status: 503}}} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			other := toolError("go: cannot find main module", "claude:s2")
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction, other)
			closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			for _, sig := range []friction.Signal{correction, other} {
				is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
				require.NoError(t, d.SaveFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), KataInstanceUID: "instance", KataProjectUID: "project", IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, Source: db.FrictionIssueSourceManual, LinkedAt: closed}))
			}
			spy := &failingGetKata{Kata: f.Kata, err: tt.err}
			f.Kata = spy
			n, err := f.Reopen(t.Context(), "2026-09-01", nil)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Equal(t, 1, spy.calls)
		})
	}
}
