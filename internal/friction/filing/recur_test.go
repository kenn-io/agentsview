package filing_test

import (
	"context"
	"fmt"
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
	type occurrence struct {
		date string
		at   *time.Time
	}
	tests := []struct {
		name, reason, since, latest string
		occurrences                 []occurrence
		closed                      *time.Time
		loc                         *time.Location
		stalePattern                bool
		comment                     string
	}{
		{name: "pre-close next day", reason: "done", occurrences: []occurrence{{"2026-09-16", new(closed.Add(-time.Hour))}}, closed: &closed},
		{name: "same day after close", reason: "done", occurrences: []occurrence{{"2026-09-15", new(closed.Add(time.Hour))}}, closed: &closed, comment: "This pattern recurred on 2026-09-15."},
		{name: "missing time later day", reason: "done", occurrences: []occurrence{{"2026-09-16", nil}}, closed: &closed},
		{name: "wontfix", reason: "wontfix", occurrences: []occurrence{{"2026-09-16", new(closed.Add(25 * time.Hour))}}, closed: &closed},
		{name: "missing close", reason: "done", occurrences: []occurrence{{"2026-09-16", new(closed.Add(25 * time.Hour))}}},
		{name: "rebuilt digest with stale pattern", reason: "done", occurrences: []occurrence{{"2026-09-14", new(closed.Add(25 * time.Hour))}}, closed: &closed, stalePattern: true, comment: "This pattern recurred on 2026-09-16."},
		{name: "local occurrence date", reason: "done", occurrences: []occurrence{{"2026-09-17", new(time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC))}}, closed: &closed, loc: time.FixedZone("UTC-4", -4*60*60), comment: "This pattern recurred on 2026-09-16."},
		{name: "latest occurrence in window", reason: "done", since: "2026-09-16", latest: "2026-09-17", occurrences: []occurrence{{"2026-09-16", new(closed.Add(50 * time.Hour))}, {"2026-09-17", new(closed.Add(25 * time.Hour))}, {"2026-09-18", new(closed.Add(74 * time.Hour))}}, closed: &closed, comment: "This pattern recurred on 2026-09-17."},
		{name: "occurrence before window", reason: "done", since: "2026-09-17", latest: "2026-09-18", occurrences: []occurrence{{"2026-09-16", new(closed.Add(25 * time.Hour))}}, closed: &closed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			sig := toolError("go: cannot find main module", "claude:s1")
			f, d := newFiler(t, s, katatest.DefaultProjectName, sig)
			if tt.stalePattern {
				seedRecurrenceSignal(t, d, "2026-09-14", "claude:s1", nil, sig)
			}
			for i, occurrence := range tt.occurrences {
				id := fmt.Sprintf("claude:s%d", i+1)
				if tt.stalePattern {
					sig.SubjectID = id
					sig.OccurredAt = *occurrence.at
					snapshot, err := review.EncodeSnapshot(review.Snapshot{Date: occurrence.date, Timezone: "UTC", RulesVersion: friction.RulesVersion, Signals: []friction.Signal{sig}})
					require.NoError(t, err)
					digest, err := d.GetFrictionDigest(t.Context(), occurrence.date)
					require.NoError(t, err)
					digest.SnapshotJSON = snapshot
					digest.Revision++
					require.NoError(t, d.SaveFrictionDigest(t.Context(), *digest, nil, nil))
					require.Equal(t, "2026-09-14", patternRow(t, d, sig.Fingerprint()).LastSeenDate)
				} else {
					seedRecurrenceSignal(t, d, occurrence.date, id, occurrence.at, sig)
				}
			}
			is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: tt.reason, ClosedAt: tt.closed})
			_, err := f.Link(t.Context(), sig.Fingerprint(), is.UID)
			require.NoError(t, err)
			since, latest := tt.since, tt.latest
			if since == "" {
				since = "2026-09-01"
			}
			if latest == "" {
				latest = "2026-09-18"
			}
			n, err := f.Reopen(t.Context(), since, latest, tt.loc)
			require.NoError(t, err)
			got := mustIssue(t, s, is.UID)
			if tt.comment != "" {
				assert.Equal(t, 1, n)
				assert.Equal(t, "open", got.Status)
				require.Len(t, got.Comments, 1)
				assert.Equal(t, tt.comment, got.Comments[0])
			} else {
				assert.Zero(t, n)
				assert.Equal(t, "closed", got.Status)
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

func TestReopenClosuresWithinSameSecond(t *testing.T) {
	s := katatest.New(t)
	sig := toolError("go: cannot find main module", "claude:s1")
	f, d := newFiler(t, s, katatest.DefaultProjectName, sig)
	closed := time.Date(2026, 9, 15, 12, 0, 0, 100, time.UTC)
	occurred := closed.Add(50 * time.Nanosecond)
	seedRecurrenceSignal(t, d, "2026-09-15", "claude:s1", &occurred, sig)
	is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
	_, err := f.Link(t.Context(), sig.Fingerprint(), is.UID)
	require.NoError(t, err)
	f.EvaluatedOccurrences = make(map[string]time.Time)
	n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	s.CloseIssue(is.UID, closed.Add(100*time.Nanosecond))
	seedRecurrenceSignal(t, d, "2026-09-15", "claude:s2", new(closed.Add(200*time.Nanosecond)), sig)
	n, err = f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, mustIssue(t, s, is.UID).Comments, 2)
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
		name       string
		err        error
		failReopen bool
	}{{name: "not ready", err: kata.ErrNotReady}, {name: "service unavailable", err: &kata.APIError{Status: 503}}, {name: "reopen unavailable", failReopen: true}} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			other := toolError("go: cannot find main module", "claude:s2")
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction, other)
			closed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			seedRecurrence(t, d, "2026-09-16", "claude:s1", new(closed.Add(time.Hour)))
			seedRecurrenceSignal(t, d, "2026-09-17", "claude:s2", new(closed.Add(25*time.Hour)), other)
			var issues []string
			for _, sig := range []friction.Signal{correction, other} {
				is := s.AddIssue(katatest.Issue{Title: "issue", Status: "closed", ClosedReason: "done", ClosedAt: &closed})
				require.NoError(t, d.SaveFrictionIssueLink(t.Context(), db.FrictionIssueLink{Fingerprint: sig.Fingerprint(), KataInstanceUID: "instance", KataProjectUID: "project", IssueUID: is.UID, QualifiedID: "agentsview#" + is.ShortID, Source: db.FrictionIssueSourceManual, LinkedAt: closed}))
				issues = append(issues, is.UID)
			}
			f.EvaluatedOccurrences = make(map[string]time.Time)
			if tt.failReopen {
				s.Fail(http.MethodPost, "/actions/reopen", katatest.Fault{Status: 503, Code: "unavailable"})
				n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
				require.NoError(t, err)
				assert.Zero(t, n)
				assert.Empty(t, f.EvaluatedOccurrences)
				n, err = f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
				require.NoError(t, err)
				assert.Equal(t, 2, n)
				for _, uid := range issues {
					got := mustIssue(t, s, uid)
					assert.Equal(t, "open", got.Status)
					assert.Len(t, got.Comments, 1)
				}
				return
			}
			spy := &failingGetKata{Kata: f.Kata, err: tt.err}
			f.Kata = spy
			for i := range 2 {
				n, err := f.Reopen(t.Context(), "2026-09-01", "2026-09-18", nil)
				require.NoError(t, err)
				assert.Zero(t, n)
				assert.Equal(t, i+1, spy.calls)
			}
		})
	}
}
