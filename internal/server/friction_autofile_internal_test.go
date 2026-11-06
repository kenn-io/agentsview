package server

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

// An idle daemon that stops while automatic filing waits on Kata would
// leave a new issue without its link, so the pass holds a work lease and
// skips once the daemon is shutting down.
func TestAutomaticFilingHoldsAWorkLease(t *testing.T) {
	for _, tt := range []struct {
		name     string
		draining bool
	}{
		{name: "running"},
		{name: "shutting_down", draining: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ks := katatest.New(t)
			target, err := url.Parse(ks.Endpoint())
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			tracker := NewIdleTracker(time.Hour, func() {})
			var mu sync.Mutex
			var leasesDuringCreate []int
			watch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
					tracker.mu.Lock()
					active := tracker.activeWork
					tracker.mu.Unlock()
					mu.Lock()
					leasesDuringCreate = append(leasesDuringCreate, active)
					mu.Unlock()
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(watch.Close)

			fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
			runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
			s := testServer(t, 0, WithIdleTracker(tracker), WithFriction(runner, nil),
				WithKataConn(kata.NewConn(kata.Config{
					Enabled: true, Hub: true, Endpoint: watch.URL, Project: katatest.DefaultProjectName,
				})))
			s.mu.Lock()
			s.cfg.Friction.Kata.AutoFile = true
			s.cfg.Friction.BackfillDays = 7
			s.mu.Unlock()
			d, ok := s.db.(*db.DB)
			require.True(t, ok)
			runner.Store = d
			seedAutoFileDigest(t, d)
			if tt.draining {
				tracker.mu.Lock()
				tracker.draining = true
				tracker.mu.Unlock()
			}

			require.NoError(t, s.autoFileFriction(t.Context()))
			mu.Lock()
			defer mu.Unlock()
			if tt.draining {
				assert.Empty(t, leasesDuringCreate, "a shutting-down daemon files nothing")
				return
			}
			require.NotEmpty(t, leasesDuringCreate)
			for _, n := range leasesDuringCreate {
				assert.Equal(t, 1, n, "the pass holds a work lease while Kata creates the issue")
			}
			tracker.mu.Lock()
			assert.Zero(t, tracker.activeWork, "the lease ends with the pass")
			tracker.mu.Unlock()
		})
	}
}

func seedAutoFileDigest(t *testing.T, d *db.DB) {
	t.Helper()
	const date = "2026-09-14"
	sig := friction.Signal{
		Kind: friction.KindCorrection, SubjectID: "claude:s1", SubjectKind: friction.SubjectSession,
		Detector: "correction.coding", Text: "no, use the other flag", Ordinal: new(4),
	}
	snap := review.Snapshot{
		Date: date, Signals: []friction.Signal{sig}, SessionsScanned: 1,
		Timezone: "UTC", RulesVersion: friction.RulesVersion,
	}
	snapJSON, err := review.EncodeSnapshot(snap)
	require.NoError(t, err)
	require.NoError(t, d.SaveFrictionDigest(t.Context(), db.FrictionDigest{
		Date: date, Timezone: "UTC", RulesVersion: friction.RulesVersion,
		BuiltAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC), Revision: 1, SessionsScanned: 1,
		SnapshotJSON: snapJSON, SummaryJSON: friction.RenderSummaryJSON(snap.DigestSnapshot),
		Markdown: friction.RenderMarkdown(snap.DigestSnapshot), MarkdownSHA256: "s", RunID: "r",
	}, nil, []db.FrictionPatternUpdate{{
		Fingerprint: sig.Fingerprint(), Kind: string(sig.Kind), Title: sig.Title(),
		Date: date, SubjectID: sig.SubjectID, Ordinal: sig.Ordinal, Occurrences: 1,
	}}))
}
