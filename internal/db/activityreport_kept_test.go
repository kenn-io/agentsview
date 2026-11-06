package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
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

func TestActivityReportKeptJournalReadsChangesPastSnapshot(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	f.IncludeForks = true
	f.IncludeSubagents = true
	keptActivityBuild(t, d, f, q, true)

	selection := kept.Selection(f, q)
	cacheVersion, _, err := d.activityKeptVersion(t.Context())
	require.NoError(t, err)
	entries, ok := d.activityKept.Get(selection, cacheVersion)
	require.True(t, ok)
	cachedRevision := entries[0].revision
	before, err := d.UsageChangeRevision(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.RenameSession(t.Context(), "past", new("first rename")))
	version, snapshot, err := d.activityKeptVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, cacheVersion, version)
	require.Greater(t, snapshot, cachedRevision)
	assert.Greater(t, snapshot, before)
	require.NoError(t, d.RenameSession(t.Context(), "past", new("second rename")))
	var latestJournalRevision int64
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT revision FROM usage_session_changes WHERE session_id = 'past'`).Scan(&latestJournalRevision))
	require.Greater(t, latestJournalRevision, snapshot)
	boundedChanges, err := d.LoadUsageChanges(t.Context(), cachedRevision, snapshot)
	require.NoError(t, err)
	require.Empty(t, boundedChanges)

	_, hit, err := d.keptActivityReport(t.Context(), selection, version, snapshot)
	require.NoError(t, err)
	assert.False(t, hit, "the second write to one session must invalidate the kept report")
}

func TestActivityReportKeptAdvancesPastJournalSnapshot(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	keptActivitySession(t, d, "today", "project-a", "2026-06-17")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{
		Timezone: "UTC", IncludeForks: true, IncludeSubagents: true,
	}
	keptActivityBuild(t, d, f, q, true)
	selection := kept.Selection(f, q)
	cacheVersion, _, err := d.activityKeptVersion(t.Context())
	require.NoError(t, err)
	entries, ok := d.activityKept.Get(selection, cacheVersion)
	require.True(t, ok)
	cachedRevision := entries[0].revision

	require.NoError(t, d.RenameSession(t.Context(), "today", new("first rename")))
	version, snapshot, err := d.activityKeptVersion(t.Context())
	require.NoError(t, err)
	require.Equal(t, cacheVersion, version)
	require.NoError(t, d.RenameSession(t.Context(), "today", new("second rename")))
	var latestJournalRevision int64
	require.NoError(t, d.getReader().QueryRowContext(t.Context(),
		`SELECT revision FROM usage_session_changes WHERE session_id = 'today'`).Scan(&latestJournalRevision))
	require.Greater(t, latestJournalRevision, snapshot)

	_, hit, err := d.keptActivityReport(t.Context(), selection, version, snapshot)
	require.NoError(t, err)
	require.True(t, hit)
	entries, ok = d.activityKept.Get(selection, version)
	require.True(t, ok)
	assert.Equal(t, latestJournalRevision, entries[0].revision)
	assert.Greater(t, entries[0].revision, cachedRevision)
}

func TestActivityReportKeptInvalidatesForTerminalEventAfterRange(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "old", "project-a", "2026-06-12")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	first := keptActivityBuild(t, d, f, q, true)
	assert.Empty(t, first.Sessions)

	timingInsertToolResultEvent(t, d, "old", 0, 0, "call", "completed", "2026-06-16T10:00:00Z", 0)
	require.NoError(t, d.RenameSession(t.Context(), "old", new("terminal session")))
	after := keptActivityBuild(t, d, f, q, true)
	require.Len(t, after.Sessions, 1)
	assert.Equal(t, "old", after.Sessions[0].SessionID)
}

func TestActivityReportKeptIgnoresSessionOutsideExactRange(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	keptActivitySession(t, d, "today", "project-a", "2026-06-15")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	first := keptActivityBuild(t, d, f, q, true)
	require.Len(t, first.Sessions, 1)
	require.NoError(t, d.RenameSession(t.Context(), "today", new("today changed")))
	requireKeptActivityEqual(t, first, keptActivityBuild(t, d, f, q, false))
}

func TestActivityReportKeptIgnoresSameIdentityObservationFromToday(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	identity := export.ProjectIdentityObservation{
		Project: "project-a", Machine: defaultMachine, RootPath: "/work/project-a",
		GitRemote: "https://example.com/team/project-a.git",
	}
	identity.SessionID = "past"
	identity.ObservedAt = time.Date(2026, 6, 14, 11, 0, 0, 0, time.UTC)
	require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), identity))
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	first := keptActivityBuild(t, d, f, q, true)
	beforeIdentityRevision, err := d.ProjectIdentityPublicationRevision(t.Context())
	require.NoError(t, err)

	today := Session{
		ID: "today", Project: "project-a", Machine: defaultMachine, Agent: "claude",
		StartedAt: new("2026-06-15T10:00:00Z"), EndedAt: new("2026-06-15T10:01:00Z"),
		MessageCount: 1,
	}
	identity.SessionID = today.ID
	identity.ObservedAt = time.Date(2026, 6, 15, 10, 2, 0, 0, time.UTC)
	snapshotProject := today.Project
	result, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{
		Session:             today,
		Messages:            []Message{{SessionID: today.ID, Ordinal: 0, Role: "user", Content: "today", Timestamp: "2026-06-15T10:00:00Z"}},
		IdentityObservation: identity, IdentitySnapshotProject: &snapshotProject,
	}})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	afterIdentityRevision, err := d.ProjectIdentityPublicationRevision(t.Context())
	require.NoError(t, err)
	assert.Greater(t, afterIdentityRevision, beforeIdentityRevision)
	projects, err := d.BuildProjectIdentityMap(t.Context(), []string{"project-a"})
	require.NoError(t, err)
	assert.Equal(t, first.Report.Projects, export.ProjectMapForWire(projects))
	snapshots, err := d.ListSessionProjectIdentitySnapshotsByID(t.Context(), []string{today.ID})
	require.NoError(t, err)
	require.Contains(t, snapshots, today.ID)
	assert.Equal(t, today.ID, snapshots[today.ID].SessionID)
	after := keptActivityBuild(t, d, f, q, false)
	requireKeptActivityEqual(t, first, after)
	selection := kept.Selection(AnalyticsFilter{Timezone: "UTC", IncludeForks: true, IncludeSubagents: true}, q)
	version, revision, err := d.activityKeptVersion(t.Context())
	require.NoError(t, err)
	entries, ok := d.activityKept.Get(selection, version)
	require.True(t, ok)
	assert.GreaterOrEqual(t, entries[0].revision, revision)
}

func TestActivityReportKeptValidationWaitsForBulkReadPermit(t *testing.T) {
	d := testDB(t)
	keptActivitySession(t, d, "past", "project-a", "2026-06-14")
	q, f := dayQuery(t, "2026-06-14", "UTC"), AnalyticsFilter{Timezone: "UTC"}
	keptActivityBuild(t, d, f, q, true)

	releases := make([]func(), 0, bulkReadSlots)
	for range bulkReadSlots {
		release, err := d.acquireBulkRead(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := d.BuildActivityReportArtifacts(ctx, f, q, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
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
