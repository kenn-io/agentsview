package filing_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/filing"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

var allKinds = filing.Policy{Kinds: friction.FileableKinds, Since: digestDate}

// clock lets a test move the filer's time forward between passes.
type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func withClock(f *filing.Filer) *clock {
	c := &clock{now: f.Now()}
	f.Now = func() time.Time { return c.now }
	return c
}

// issueRequests counts issue lookups and creates, leaving out readiness probes.
func issueRequests(s *katatest.Server) int {
	return len(s.RequestsMatching(http.MethodGet, "/issues")) + len(s.RequestsMatching(http.MethodPost, "/issues"))
}

func TestReconcileFilesEachRecentPatternOfAnEnabledKind(t *testing.T) {
	tests := []struct {
		name   string
		policy filing.Policy
		filed  []friction.Signal
	}{
		{name: "all_fileable_kinds", policy: allKinds, filed: []friction.Signal{correction, diagnostic}},
		{
			name:   "chosen_kinds_only",
			policy: filing.Policy{Kinds: []friction.Kind{friction.KindCorrection}, Since: digestDate},
			filed:  []friction.Signal{correction},
		},
		{name: "older_than_the_window", policy: filing.Policy{Kinds: friction.FileableKinds, Since: "2026-09-15"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction, diagnostic, frustration)

			rep, err := f.Reconcile(t.Context(), tt.policy)
			require.NoError(t, err)
			assert.Equal(t, filing.ReconcileReport{Created: len(tt.filed)}, rep)
			for _, sig := range tt.filed {
				assert.NotNil(t, patternRow(t, d, sig.Fingerprint()).Issue, "%s is linked", sig.Kind)
			}
			assert.Nil(t, patternRow(t, d, frustration.Fingerprint()).Issue, "frustration is never filed")

			rep, err = f.Reconcile(t.Context(), tt.policy)
			require.NoError(t, err)
			assert.Zero(t, rep, "a linked pattern is not filed again")
			assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), len(tt.filed))
		})
	}
}

func TestReconcileRetriesWithGrowingDelays(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	c := withClock(f)
	fp := correction.Fingerprint()
	start := c.now
	for range 2 {
		s.Fail(http.MethodPost, "/issues", katatest.Fault{Status: 503, Code: "unavailable"})
	}

	rep, err := f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Equal(t, filing.ReconcileReport{Retry: 1}, rep)
	st := patternRow(t, d, fp).Filing
	require.NotNil(t, st)
	assert.Equal(t, db.FrictionFilingRetry, st.State)
	assert.Equal(t, 1, st.Attempts)
	assert.Contains(t, st.LastError, "503")
	assert.Equal(t, start.Add(time.Hour), st.NextAttemptAt.UTC())

	c.advance(30 * time.Minute)
	rep, err = f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Zero(t, rep, "a pattern waits until its next attempt")
	assert.Len(t, s.RequestsMatching(http.MethodPost, "/issues"), 1)

	c.advance(30 * time.Minute)
	_, err = f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	st = patternRow(t, d, fp).Filing
	require.NotNil(t, st)
	assert.Equal(t, 2, st.Attempts)
	assert.Equal(t, start, st.FirstFailedAt.UTC())
	assert.Equal(t, c.now.Add(2*time.Hour), st.NextAttemptAt.UTC(), "the delay doubles")

	c.advance(2 * time.Hour)
	rep, err = f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Equal(t, filing.ReconcileReport{Created: 1}, rep)
	row := patternRow(t, d, fp)
	assert.NotNil(t, row.Issue)
	assert.Nil(t, row.Filing, "linking clears the filing state")
}

func TestReconcileStopsRetryingAfterTwoWeeks(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	now := f.Now()
	first, next := now.Add(-15*24*time.Hour), now.Add(-time.Minute)
	require.NoError(t, d.SaveFrictionFilingState(t.Context(), db.FrictionFilingState{
		Fingerprint: fp, State: db.FrictionFilingRetry, Attempts: 20,
		FirstFailedAt: &first, NextAttemptAt: &next, UpdatedAt: next,
	}))
	s.Fail(http.MethodPost, "/issues", katatest.Fault{Status: 503, Code: "unavailable", Message: "still down"})

	rep, err := f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Equal(t, filing.ReconcileReport{NeedsPerson: 1}, rep)
	st := patternRow(t, d, fp).Filing
	require.NotNil(t, st)
	assert.Equal(t, db.FrictionFilingNeedsPerson, st.State)
	assert.Equal(t, 21, st.Attempts)
	assert.Contains(t, st.LastError, "stopped retrying after 14 days")
	assert.Nil(t, st.NextAttemptAt)
}

func TestReconcileLeavesUnfixableFailuresToAPerson(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *katatest.Server)
	}{
		{
			name: "several_candidate_issues",
			setup: func(s *katatest.Server) {
				for range 2 {
					s.AddIssue(katatest.Issue{Title: "dup", Status: "open", Metadata: map[string]any{
						filing.MetadataFingerprint: correction.Fingerprint(),
					}})
				}
			},
		},
		{
			name: "kata_rejects_the_request",
			setup: func(s *katatest.Server) {
				s.Fail(http.MethodPost, "/issues", katatest.Fault{Status: 422, Code: "validation", Message: "title too long"})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			c := withClock(f)
			fp := correction.Fingerprint()
			tt.setup(s)

			rep, err := f.Reconcile(t.Context(), allKinds)
			require.NoError(t, err)
			assert.Equal(t, filing.ReconcileReport{NeedsPerson: 1}, rep)
			st := patternRow(t, d, fp).Filing
			require.NotNil(t, st)
			assert.Equal(t, db.FrictionFilingNeedsPerson, st.State)
			assert.NotEmpty(t, st.LastError)

			before := issueRequests(s)
			c.advance(48 * time.Hour)
			rep, err = f.Reconcile(t.Context(), allKinds)
			require.NoError(t, err)
			assert.Zero(t, rep)
			assert.Equal(t, before, issueRequests(s), "the pattern waits for a person")

			mine := s.AddIssue(katatest.Issue{Title: "picked by hand", Status: "open"})
			_, err = f.Link(t.Context(), fp, mine.UID)
			require.NoError(t, err)
			assert.Nil(t, patternRow(t, d, fp).Filing, "a manual link settles the pattern")
		})
	}
}

func TestReconcileSkipsAPatternAPersonUnlinked(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	_, err := f.File(t.Context(), fp)
	require.NoError(t, err)
	require.NoError(t, f.Unlink(t.Context(), fp))
	st := patternRow(t, d, fp).Filing
	require.NotNil(t, st)
	assert.Equal(t, db.FrictionFilingPaused, st.State)

	rep, err := f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Zero(t, rep)
	assert.Nil(t, patternRow(t, d, fp).Issue, "automatic filing does not undo an unlink")

	_, err = f.File(t.Context(), fp)
	require.NoError(t, err)
	row := patternRow(t, d, fp)
	assert.NotNil(t, row.Issue)
	assert.Nil(t, row.Filing, "filing by hand resumes the pattern")
}

func TestReconcileWaitsWhileKataIsNotReady(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, "no-such-project", correction)

	rep, err := f.Reconcile(t.Context(), allKinds)
	require.NoError(t, err)
	assert.Zero(t, rep)
	assert.Nil(t, patternRow(t, d, correction.Fingerprint()).Filing, "an outage costs no attempts")
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
}

func TestReconcileRetriesPatternsOlderThanTheWindow(t *testing.T) {
	s := katatest.New(t)
	f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
	fp := correction.Fingerprint()
	now := f.Now()
	first, next := now.Add(-3*24*time.Hour), now.Add(-time.Minute)
	require.NoError(t, d.SaveFrictionFilingState(t.Context(), db.FrictionFilingState{
		Fingerprint: fp, State: db.FrictionFilingRetry, Attempts: 3,
		FirstFailedAt: &first, NextAttemptAt: &next, UpdatedAt: next,
	}))
	// The pattern was last seen before the window that finds new patterns.
	policy := filing.Policy{Kinds: friction.FileableKinds, Since: "2026-09-15"}

	rep, err := f.Reconcile(t.Context(), policy)
	require.NoError(t, err)
	assert.Equal(t, filing.ReconcileReport{Created: 1}, rep)
	assert.NotNil(t, patternRow(t, d, fp).Issue)
}

// unlinkDuringCreate is Kata that, while the create request is in flight,
// lets a person link the pattern and unlink it again, then answers fail.
type unlinkDuringCreate struct {
	filing.Kata
	store       *db.DB
	fingerprint string
	fail        error
}

func (k unlinkDuringCreate) CreateIssue(ctx context.Context, key string, req kata.CreateIssue) (kata.CreateResult, error) {
	manual := db.FrictionIssueLink{
		Fingerprint: k.fingerprint, KataInstanceUID: "i", KataProjectUID: "p", IssueUID: "manual",
		QualifiedID: "agentsview#m001", Source: db.FrictionIssueSourceManual, LinkedAt: time.Now(),
	}
	if err := k.store.SaveFrictionIssueLink(ctx, manual); err != nil {
		return kata.CreateResult{}, err
	}
	if _, err := k.store.DeleteFrictionIssueLink(ctx, k.fingerprint); err != nil {
		return kata.CreateResult{}, err
	}
	if k.fail != nil {
		return kata.CreateResult{}, k.fail
	}
	return k.Kata.CreateIssue(ctx, key, req)
}

func TestReconcileKeepsAPauseAPersonSetMeanwhile(t *testing.T) {
	for _, tt := range []struct {
		name string
		fail error
	}{
		{name: "filing_succeeds"},
		{name: "filing_fails", fail: errors.New("kata: HTTP 503")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			f, d := newFiler(t, s, katatest.DefaultProjectName, correction)
			fp := correction.Fingerprint()
			f.Kata = unlinkDuringCreate{Kata: f.Kata, store: d, fingerprint: fp, fail: tt.fail}

			_, err := f.Reconcile(t.Context(), allKinds)
			require.NoError(t, err)
			row := patternRow(t, d, fp)
			assert.Nil(t, row.Issue, "the automatic result does not undo the unlink")
			require.NotNil(t, row.Filing)
			assert.Equal(t, db.FrictionFilingPaused, row.Filing.State)
		})
	}
}
