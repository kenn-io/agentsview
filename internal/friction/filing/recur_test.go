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
			n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
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

type unlinkOnPatternListStore struct {
	*db.DB
	fingerprint string
}

func (s unlinkOnPatternListStore) ListFrictionPatterns(ctx context.Context, filter db.FrictionPatternFilter) ([]db.FrictionPattern, string, error) {
	rows, next, err := s.DB.ListFrictionPatterns(ctx, filter)
	if err != nil {
		return nil, "", err
	}
	_, err = s.DeleteFrictionIssueLink(ctx, s.fingerprint)
	if err != nil {
		return nil, "", err
	}
	return rows, next, nil
}

func TestReopenSkipsUnlinkedPattern(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(26*time.Hour)))
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	f.EvaluatedOccurrences = make(map[string]time.Time)
	f.Store = unlinkOnPatternListStore{DB: d, fingerprint: correction.Fingerprint()}

	n, err := f.Reopen(t.Context(), "2026-09-16", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Nil(t, patternRow(t, d, correction.Fingerprint()).Issue)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "closed", got.Status)
	assert.Empty(t, got.Comments)

	f.Store = d
	_, err = f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	n, err = f.Reopen(t.Context(), "2026-09-16", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestReopenFromRebuiltDigestWithoutPatternUpdate(t *testing.T) {
	closed := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		live, saved time.Time
		wantOpen    bool
	}{
		{"live finding older than digest", time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC), true},
		{"live finding later than digest", time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName)
			old := toolError("go: cannot find main module", "claude:s1")
			sig := toolError("go: cannot find main module", "claude:s2")
			require.Equal(t, old.Fingerprint(), sig.Fingerprint())
			seedRecurrenceSignal(t, d, "2026-09-05", old.SubjectID, nil, old)
			dbtest.SeedSession(t, d, sig.SubjectID, "proj")
			digest, err := d.GetFrictionDigest(t.Context(), "2026-09-14")
			require.NoError(t, err)
			digest.Revision++
			require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, []db.FrictionDigestSubject{{SubjectID: sig.SubjectID, Date: digest.Date, SubjectKind: "session"}}, nil))
			session, err := d.GetSession(t.Context(), sig.SubjectID)
			require.NoError(t, err)
			applied, err := d.ReplaceSessionFrictionAtRevision(t.Context(), *session, db.SessionFrictionUpdate{
				RulesVersion: friction.RulesVersion, Findings: []db.FrictionFinding{{SessionID: sig.SubjectID, Kind: string(sig.Kind), Detector: sig.Detector, Fingerprint: sig.Fingerprint(), OccurredAt: &tt.live, RulesVersion: friction.RulesVersion}},
			})
			require.NoError(t, err)
			require.True(t, applied)
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
			_, err = f.Link(t.Context(), sig.Fingerprint(), is.UID)
			require.NoError(t, err)
			sig.OccurredAt = tt.saved
			digest.SnapshotJSON, err = review.EncodeSnapshot(review.Snapshot{Date: digest.Date, Timezone: "UTC", RulesVersion: friction.RulesVersion, Signals: []friction.Signal{sig}})
			require.NoError(t, err)
			digest.Revision++
			require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, []db.FrictionDigestSubject{{SubjectID: sig.SubjectID, Date: digest.Date, SubjectKind: "session"}}, nil))
			require.Equal(t, "2026-09-05", patternRow(t, d, sig.Fingerprint()).LastSeenDate)

			n, err := f.Reopen(t.Context(), "2026-09-10", "2026-09-18", nil)
			require.NoError(t, err)
			got := mustIssue(t, s, is.UID)
			if tt.wantOpen {
				assert.Equal(t, 1, n)
				assert.Equal(t, "open", got.Status)
				require.Len(t, got.Comments, 1)
				assert.Equal(t, "This pattern recurred on 2026-09-14.", got.Comments[0])
			} else {
				assert.Zero(t, n)
				assert.Equal(t, "closed", got.Status)
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

	n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", time.FixedZone("UTC-4", -4*60*60))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "This pattern recurred on 2026-09-16.", got.Comments[0])
}

func TestReopenUsesLatestOccurrenceInWindow(t *testing.T) {
	s := katatest.New(t)
	sig := toolError("go: cannot find main module", "claude:s1")
	f, d := newFiler(t, s, katatest.DefaultProjectName, sig)
	closed := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	seedRecurrenceSignal(t, d, "2026-09-13", "claude:s1", new(closed.Add(24*time.Hour)), sig)
	seedRecurrenceSignal(t, d, "2026-09-15", "claude:s2", new(closed.Add(72*time.Hour)), sig)
	seedRecurrenceSignal(t, d, "2026-09-17", "claude:s3", new(closed.Add(120*time.Hour)), sig)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), sig.Fingerprint(), is.UID)
	require.NoError(t, err)

	n, err := f.Reopen(t.Context(), "2026-09-14", "2026-09-15", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
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
	n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	s.CloseIssue(is.UID, closed.Add(100*time.Nanosecond))
	n, err = f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, mustIssue(t, s, is.UID).Comments, 2)
}

func TestReopenRetry(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(26*time.Hour)))
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), correction.Fingerprint(), is.UID)
	require.NoError(t, err)
	f.EvaluatedOccurrences = make(map[string]time.Time)
	s.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})
	n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, "closed", mustIssue(t, s, is.UID).Status)
	n, err = f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got := mustIssue(t, s, is.UID)
	assert.Equal(t, "open", got.Status)
	assert.Len(t, got.Comments, 1)
}

type countingGetKata struct {
	filing.Kata
	calls int
}

func (k *countingGetKata) GetIssue(ctx context.Context, ref string) (kata.Issue, error) {
	k.calls++
	return k.Kata.GetIssue(ctx, ref)
}

func TestReopenReadsIssueOnlyForNewOccurrences(t *testing.T) {
	s := katatest.New(t)
	sig := toolError("go: cannot find main module", "claude:s1")
	f, d := newFiler(t, s, katatest.DefaultProjectName, sig)
	occurred := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	seedRecurrenceSignal(t, d, "2026-09-15", "claude:s1", &occurred, sig)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "open"})
	_, err := f.Link(t.Context(), sig.Fingerprint(), is.UID)
	require.NoError(t, err)
	spy := &countingGetKata{Kata: f.Kata}
	f.Kata = spy
	f.EvaluatedOccurrences = make(map[string]time.Time)

	for range 2 {
		n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
		require.NoError(t, err)
		assert.Zero(t, n)
	}
	assert.Equal(t, 1, spy.calls)

	s.CloseIssue(is.UID, occurred.Add(time.Hour))
	seedRecurrenceSignal(t, d, "2026-09-16", "claude:s2", new(occurred.Add(24*time.Hour)), sig)
	n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, spy.calls)
	assert.Equal(t, "open", mustIssue(t, s, is.UID).Status)
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
			seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(time.Hour)))
			seedRecurrenceSignal(t, d, "2026-09-17", "claude:s2", new(closed.Add(25*time.Hour)), other)
			for _, sig := range []friction.Signal{correction, other} {
				is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
				require.NoError(t, d.SaveFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), KataInstanceUID: "instance", KataProjectUID: "project", IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, Source: db.FrictionIssueSourceManual, LinkedAt: closed}))
			}
			spy := &failingGetKata{Kata: f.Kata, err: tt.err}
			f.Kata = spy
			f.EvaluatedOccurrences = make(map[string]time.Time)
			for i := range 2 {
				n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
				require.NoError(t, err)
				assert.Zero(t, n)
				assert.Equal(t, i+1, spy.calls)
			}
		})
	}
}
