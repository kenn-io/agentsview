package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func keptActivitySession(t *testing.T, d *DB, id, project, date string) {
	t.Helper()
	insertSession(t, d, id, project, func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new(date + "T10:00:00Z")
		s.EndedAt = new(date + "T10:01:00Z")
		s.MessageCount = 2
	})
	insertMessages(t, d,
		Message{SessionID: id, Ordinal: 0, Role: "user", Content: "question", Timestamp: date + "T10:00:00Z"},
		Message{SessionID: id, Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: date + "T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":500}`)},
	)
}

func keptActivityBuild(t *testing.T, d *DB, f AnalyticsFilter, q activity.Query, wantUsage bool) activity.CandidateArtifacts {
	t.Helper()
	var progress []activity.Progress
	artifacts, err := d.BuildActivityReportArtifacts(t.Context(), f, q, func(p activity.Progress) { progress = append(progress, p) })
	require.NoError(t, err)
	require.NotEmpty(t, progress)
	assert.Equal(t, activity.ProgressLoadingSessions, progress[0].Phase)
	assert.Equal(t, activity.ProgressDone, progress[len(progress)-1].Phase)
	loaded := false
	for _, p := range progress {
		loaded = loaded || p.Phase == activity.ProgressLoadingUsage
	}
	assert.Equal(t, wantUsage, loaded)
	return artifacts
}

func requireKeptActivityEqual(t *testing.T, want, got activity.CandidateArtifacts) {
	t.Helper()
	a, err := activity.ArtifactDigest(want)
	require.NoError(t, err)
	b, err := activity.ArtifactDigest(got)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func clearKeptActivity(d *DB) { d.activityKept.DeleteSlots(func(string) bool { return true }) }

func TestActivityReportKeptReuse(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	keptActivitySession(t, d, "current", "project-a", "2026-06-17")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	first := keptActivityBuild(t, d, f, q, true)
	require.Equal(t, 500, first.Report.Totals.OutputTokens)
	requireKeptActivityEqual(t, first, keptActivityBuild(t, d, f, q, false))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{{SessionID: "current", Ordinal: 2, Role: "assistant", Content: "more", Timestamp: "2026-06-17T10:02:00Z"}}))
	requireKeptActivityEqual(t, first, keptActivityBuild(t, d, f, q, false))
	s, err := d.GetSession(t.Context(), "current")
	require.NoError(t, err)
	require.NotNil(t, s)
	s.MessageCount = 4
	result, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{Session: *s, Messages: []Message{{SessionID: s.ID, Ordinal: 3, Role: "assistant", Content: "again", Timestamp: "2026-06-17T10:03:00Z"}}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	requireKeptActivityEqual(t, first, keptActivityBuild(t, d, f, q, false))
}

func TestActivityReportKeptChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *DB)
		write   func(*testing.T, *DB)
		check   func(*testing.T, activity.CandidateArtifacts)
	}{
		{name: "late import", write: func(t *testing.T, d *DB) {
			t.Helper()
			keptActivitySession(t, d, "late", "project-a", "2026-06-14")
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 2)
		}},
		{name: "soft delete", write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SoftDeleteSession(t.Context(), "past"))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "hard delete", write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.DeleteSession(t.Context(), "past"))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "restore", prepare: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.SoftDeleteSession(t.Context(), "past"))
		}, write: func(t *testing.T, d *DB) {
			t.Helper()
			_, err := d.RestoreSession(t.Context(), "past")
			require.NoError(t, err)
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 1)
		}},
		{name: "redate", write: func(t *testing.T, d *DB) {
			t.Helper()
			s, err := d.GetSession(t.Context(), "past")
			require.NoError(t, err)
			require.NotNil(t, s)
			s.StartedAt = new("2026-06-17T10:00:00Z")
			s.EndedAt = new("2026-06-17T10:01:00Z")
			require.NoError(t, d.UpsertSession(t.Context(), *s))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "rename", write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.RenameSession(t.Context(), "past", new("Renamed report session")))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			require.Len(t, a.Sessions, 1)
			assert.Equal(t, "Renamed report session", a.Sessions[0].Title)
		}},
		{name: "catalog pricing", write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.UpsertModelPricing([]ModelPricing{{ModelPattern: "kept-model", OutputPerMTok: money.MustParseDollars("10")}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, money.MustParseDollars("0.005"), a.Report.Totals.Cost)
		}},
		{name: "custom pricing", write: func(t *testing.T, d *DB) {
			t.Helper()
			d.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, money.MustParseDollars("0.01"), a.Report.Totals.Cost)
		}},
		{name: "project identity", write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
				SessionID: "past", Project: "project-a", Machine: defaultMachine,
				RootPath: "/work/project-a", GitRemote: "https://example.com/team/project-a.git",
				ObservedAt: time.Date(2026, 6, 14, 11, 0, 0, 0, time.UTC),
			}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.NotEmpty(t, a.Report.Projects)
		}},
		{name: "project assignment", write: func(t *testing.T, d *DB) {
			t.Helper()
			_, err := d.AssignSessionProject(t.Context(), "past", "project-b")
			require.NoError(t, err)
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "usage replacement", prepare: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "past", []UsageEvent{{Source: "session", Model: "kept-model", OutputTokens: 100, OccurredAt: "2026-06-14T10:01:00Z"}}))
		}, write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "past", []UsageEvent{{Source: "session", Model: "kept-model", OutputTokens: 900, OccurredAt: "2026-06-14T10:01:00Z"}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, 1400, a.Report.Totals.OutputTokens)
		}},
		{name: "excluded Claude peer", prepare: func(t *testing.T, d *DB) {
			t.Helper()
			insertSession(t, d, "peer", "project-b", func(s *Session) {
				s.Agent = "claude"
				s.StartedAt = new("2026-06-14T10:01:00Z")
				s.EndedAt = s.StartedAt
			})
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "past", []Message{{SessionID: "past", Ordinal: 0, Role: "assistant", Content: "partial", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":5}`), ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"}}))
			insertMessages(t, d, Message{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":631}`), ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"})
		}, write: func(t *testing.T, d *DB) {
			t.Helper()
			require.NoError(t, d.DeleteSession(t.Context(), "peer"))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, 5, a.Report.Totals.OutputTokens)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			keptActivitySession(t, d, "past", "project-a", "2026-06-14")
			if tc.prepare != nil {
				tc.prepare(t, d)
			}
			q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
			before := keptActivityBuild(t, d, f, q, true)
			requireKeptActivityEqual(t, before, keptActivityBuild(t, d, f, q, false))
			tc.write(t, d)
			changed := keptActivityBuild(t, d, f, q, true)
			tc.check(t, changed)
			a, err := activity.ArtifactDigest(before)
			require.NoError(t, err)
			b, err := activity.ArtifactDigest(changed)
			require.NoError(t, err)
			assert.NotEqual(t, a, b)
			clearKeptActivity(d)
			requireKeptActivityEqual(t, changed, keptActivityBuild(t, d, f, q, true))
			requireKeptActivityEqual(t, changed, keptActivityBuild(t, d, f, q, false))
		})
	}
}

func TestActivityReportKeptSkipsPartialAndSwappedArchives(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	f := AnalyticsFilter{Timezone: "UTC"}
	partial, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-06-14", Timezone: "UTC"}, time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	for range 2 {
		keptActivityBuild(t, d, f, partial, true)
	}
	q := dayQuery(t, "2026-06-14", "UTC")
	before := keptActivityBuild(t, d, f, q, true)
	requireKeptActivityEqual(t, before, keptActivityBuild(t, d, f, q, false))

	readOnly, err := OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
	for range 2 {
		requireKeptActivityEqual(t, before, keptActivityBuild(t, readOnly, f, q, true))
	}
	require.NoError(t, readOnly.Close())

	next := testDBAtPath(t, filepath.Join(t.TempDir(), "next.db"), "next")
	keptActivitySession(t, next, "replacement", "project-a", "2026-06-14")
	require.NoError(t, next.Close())
	require.NoError(t, d.CloseConnections(t.Context()))
	require.NoError(t, os.Rename(next.Path(), d.Path()))
	require.NoError(t, d.Reopen())
	changed := keptActivityBuild(t, d, f, q, true)
	require.Len(t, changed.Sessions, 1)
	assert.Equal(t, "replacement", changed.Sessions[0].SessionID)
	clearKeptActivity(d)
	requireKeptActivityEqual(t, changed, keptActivityBuild(t, d, f, q, true))
}
