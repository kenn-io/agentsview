//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
)

type pausedFrictionSelection struct {
	review.Store
	selected, resume chan struct{}
	once             sync.Once
}

func (s *pausedFrictionSelection) FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error) {
	subjects, err := s.Store.FrictionSubjectsForDate(ctx, date, loc, includeDigested)
	if err != nil {
		return nil, err
	}
	s.once.Do(func() {
		close(s.selected)
		select {
		case <-s.resume:
		case <-ctx.Done():
		}
	})
	return subjects, ctx.Err()
}

func TestFrictionConcurrentBuildsAfterActivityMoves(t *testing.T) {
	for _, tc := range []struct {
		name           string
		competingBuild bool
	}{
		{"postgres builders", true},
		{"postgres push without another builder", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, hub := openFrictionParityStores(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			other, err := NewStore(testPGURL(t), "agentsview_friction_digest_test", true)
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })
			paused := &pausedFrictionSelection{Store: hub, selected: make(chan struct{}), resume: make(chan struct{})}
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(paused.resume) }) }
			defer resume()
			now := func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
			first := &review.Runner{Store: paused, Now: now}
			second := &review.Runner{Store: other, Now: now}
			done := make(chan struct{})
			var firstReport review.Report
			var buildErr error
			go func() {
				defer close(done)
				firstReport, buildErr = first.BuildDate(ctx, "2026-09-15", review.BuildOptions{})
			}()
			defer func() {
				cancel()
				resume()
				<-done
			}()
			select {
			case <-paused.selected:
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			}

			// The first builder has selected a and b. A sync moves a to the
			// next day and replaces its findings before the build reads them.
			session, err := local.GetSession(ctx, "a")
			require.NoError(t, err)
			require.NotNil(t, session)
			session.EndedAt = new("2026-09-16T01:00:00Z")
			require.NoError(t, local.UpsertSession(ctx, *session))
			require.NoError(t, local.UpdateSessionSignals(ctx, "a", db.SessionSignalUpdate{
				Friction: &db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Hash: "resumed",
					Findings: []db.FrictionFinding{{SessionID: "a", Kind: "correction", Detector: "correction.coding",
						Text: "changed after selection", Title: "updated", Fingerprint: "updated", RulesVersion: friction.RulesVersion}},
				},
			}))
			syncer := &Sync{pg: hub.DB(), local: local, machine: "machine", schema: "agentsview_friction_digest_test", schemaDone: true}
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			buildSecond := func() {
				secondReport, err := second.BuildDate(ctx, "2026-09-16", review.BuildOptions{})
				require.NoError(t, err)
				require.True(t, secondReport.Written)
			}
			if tc.competingBuild {
				buildSecond()
			}
			resume()
			select {
			case <-done:
				require.NoError(t, buildErr)
			case <-ctx.Done():
				require.NoError(t, ctx.Err())
			}
			require.True(t, firstReport.Written)
			storedFirst, err := hub.GetFrictionDigest(ctx, "2026-09-15")
			require.NoError(t, err)
			require.NotNil(t, storedFirst)
			assert.Equal(t, 1, storedFirst.SessionsScanned)
			snapshot, err := review.DecodeSnapshot(storedFirst.SnapshotJSON)
			require.NoError(t, err)
			require.Len(t, snapshot.Signals, 1)
			assert.Equal(t, "b", snapshot.Signals[0].SubjectID)
			if !tc.competingBuild {
				buildSecond()
			}
			storedSecond, err := other.GetFrictionDigest(ctx, "2026-09-16")
			require.NoError(t, err)
			require.NotNil(t, storedSecond)
			assert.Equal(t, 2, storedSecond.SessionsScanned)
			assert.Contains(t, string(storedSecond.Markdown), "changed after selection")
			var sessions, occurrences int
			require.NoError(t, hub.DB().QueryRowContext(ctx, `SELECT SUM(session_count), SUM(occurrence_count) FROM friction_patterns`).Scan(&sessions, &occurrences))
			assert.Equal(t, 3, sessions, "each of the three reviewed sessions contributes once")
			assert.Equal(t, 3, occurrences)
		})
	}
}

// The same archive digested on SQLite and on its PostgreSQL hub produces
// identical Markdown and summary JSON.
func TestFrictionRunnerParitySQLiteAndPG(t *testing.T) {
	local, pgStore := openFrictionParityStores(t)
	now := func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }
	build := func(s review.Store) ([]byte, []byte) {
		r := &review.Runner{Store: s, Loc: time.UTC, Now: now, BackfillDays: 7}
		reps, err := r.CatchUp(t.Context())
		require.NoError(t, err)
		require.Equal(t, []string{"2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18", "2026-09-19", "2026-09-20", "2026-09-21"},
			func() []string {
				out := []string{}
				for _, rep := range reps {
					out = append(out, rep.Date)
				}
				return out
			}())
		d, err := s.GetFrictionDigest(t.Context(), "2026-09-15")
		require.NoError(t, err)
		require.NotNil(t, d)
		return d.Markdown, d.SummaryJSON
	}
	sqliteMD, sqliteSummary := build(local)
	pgMD, pgSummary := build(pgStore)
	assert.Equal(t, string(sqliteMD), string(pgMD))
	assert.Equal(t, string(sqliteSummary), string(pgSummary))
	assert.Contains(t, string(pgMD), "`agent:claude`")
}

func TestFrictionHubCatchesUpAfterLatePush(t *testing.T) {
	local, hub := openFrictionParityStores(t)
	r := &review.Runner{Store: hub, Loc: time.UTC, Now: func() time.Time {
		return time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	}, BackfillDays: 7}
	_, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	for _, s := range []struct{ id, version string }{
		{"late-old", "friction-v0"}, {"late-new", "friction-v2"}, {"disabled", ""},
		{"skipped-old", "friction-v0:skipped"}, {"skipped-new", "friction-v2:skipped"},
	} {
		ended := "2026-09-15T12:00:00Z"
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: s.id, Project: "p", Machine: "machine", Agent: "claude", EndedAt: &ended,
		}))
		var findings []db.FrictionFinding
		if !strings.HasSuffix(s.version, ":skipped") {
			findings = []db.FrictionFinding{{SessionID: s.id, Kind: "correction", Detector: "correction.coding",
				Text: s.id, Title: s.id, Fingerprint: s.id, RulesVersion: s.version}}
		}
		require.NoError(t, local.UpdateSessionSignals(t.Context(), s.id, db.SessionSignalUpdate{
			Friction: &db.SessionFrictionUpdate{RulesVersion: s.version, Hash: s.id,
				Findings: findings,
			},
		}))
	}
	syncer := &Sync{pg: hub.DB(), local: local, machine: "machine", schema: "agentsview_friction_digest_test", schemaDone: true}
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	require.Len(t, reps, 1)
	assert.Equal(t, "2026-09-15", reps[0].Date)
	got, err := hub.GetFrictionDigest(t.Context(), "2026-09-15")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 4, got.SessionsScanned)
	assert.Contains(t, string(got.Markdown), "late-old")
	assert.Contains(t, string(got.Markdown), "late-new")
	assert.NotContains(t, string(got.Markdown), "disabled")
	var count int
	require.NoError(t, hub.pg.QueryRowContext(t.Context(), `SELECT session_count FROM friction_patterns WHERE last_subject_id = 'a'`).Scan(&count))
	assert.Equal(t, 1, count, "rebuilding a date must not recount previous members")
	again, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Empty(t, again)
}

func TestFrictionMidnightDSTParity(t *testing.T) {
	local, hub := openFrictionParityStores(t)
	loc, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	for _, s := range []struct{ id, ended string }{
		{"before", "2026-09-06T03:59:59Z"}, {"first", "2026-09-06T04:00:00Z"},
		{"late", "2026-09-07T02:59:59Z"}, {"after", "2026-09-07T03:00:00Z"},
	} {
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: s.id, Project: "p", Machine: "machine", Agent: "claude", EndedAt: &s.ended,
		}))
		require.NoError(t, local.UpdateSessionSignals(t.Context(), s.id, db.SessionSignalUpdate{
			Friction: &db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Hash: s.id},
		}))
	}
	syncer := &Sync{pg: hub.DB(), local: local, machine: "machine", schema: "agentsview_friction_digest_test", schemaDone: true}
	_, err = syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	for name, store := range map[string]review.Store{"sqlite": local, "postgres": hub} {
		t.Run(name, func(t *testing.T) {
			subjects, err := store.FrictionSubjectsForDate(t.Context(), "2026-09-06", loc, false)
			require.NoError(t, err)
			ids := []string{}
			for _, s := range subjects {
				ids = append(ids, s.SubjectID)
			}
			assert.ElementsMatch(t, []string{"first", "late"}, ids)
			dates, err := store.FrictionPendingDates(t.Context(), "2026-09-06", "2026-09-06", loc)
			require.NoError(t, err)
			assert.Equal(t, []string{"2026-09-06"}, dates)
		})
	}
}
