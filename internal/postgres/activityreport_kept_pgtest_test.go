//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func pgKeptSession(t *testing.T, d *db.DB, id, project, date string) {
	t.Helper()
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: id, Project: project, Machine: "test-machine", Agent: "claude",
		CreatedAt: date + "T10:00:00Z", StartedAt: new(date + "T10:00:00Z"), EndedAt: new(date + "T10:01:00Z"), MessageCount: 2,
	}))
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "question", Timestamp: date + "T10:00:00Z"},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: date + "T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":500}`)},
	}))
}

func pgKeptIdentity(session string, day int) export.ProjectIdentityObservation {
	return export.ProjectIdentityObservation{
		SessionID: session, Project: "project-a", Machine: "test-machine", RootPath: "/work/project-a",
		GitRemote: "https://example.com/team/project-a.git", ObservedAt: time.Date(2026, 6, day, 11, 0, 0, 0, time.UTC),
	}
}

func pgKeptPush(t *testing.T, syncer *Sync, full bool) {
	t.Helper()
	result, err := syncer.Push(t.Context(), full, nil)
	require.NoError(t, err)
	require.Empty(t, result.Errors)
}

func pgKeptBuild(t *testing.T, store *Store, f db.AnalyticsFilter, q activity.Query, wantUsage bool) activity.CandidateArtifacts {
	t.Helper()
	var progress []activity.Progress
	artifacts, err := store.BuildActivityReportArtifacts(t.Context(), f, q, func(p activity.Progress) { progress = append(progress, p) })
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

func requirePGKeptEqual(t *testing.T, want, got activity.CandidateArtifacts) {
	t.Helper()
	a, err := activity.ArtifactDigest(want)
	require.NoError(t, err)
	b, err := activity.ArtifactDigest(got)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestPGActivityReportKeptChanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timezone string
		keep     bool
		prepare  func(*testing.T, *db.DB)
		write    func(*testing.T, *db.DB)
		check    func(*testing.T, activity.CandidateArtifacts)
	}{
		{name: "next day", keep: true, prepare: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("past", 14)))
		}, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "next", "project-a", "2026-06-15")
			session, err := d.GetSession(t.Context(), "next")
			require.NoError(t, err)
			require.NotNil(t, session)
			_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
				Session: *session, IdentityObservation: pgKeptIdentity("next", 15), IdentitySnapshotProject: &session.Project,
			}})
			require.NoError(t, err)
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 1)
		}},
		{name: "excluded project", keep: true, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "excluded", "project-b", "2026-06-14")
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 1)
		}},
		{name: "late import in New York", timezone: "America/New_York", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.UpsertSession(t.Context(), db.Session{
				ID: "late", Project: "project-a", Machine: "test-machine", Agent: "claude",
				CreatedAt: "2026-06-15T02:00:00Z", StartedAt: new("2026-06-15T02:00:00Z"), EndedAt: new("2026-06-15T02:01:00Z"), MessageCount: 2,
			}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "late", []db.Message{
				{SessionID: "late", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-15T02:00:00Z"},
				{SessionID: "late", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-15T02:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":500}`)},
			}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 2)
			assert.Equal(t, 1000, a.Report.Totals.OutputTokens)
		}},
		{name: "hard delete", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.DeleteSession(t.Context(), "past"))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "restore", prepare: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.SoftDeleteSession(t.Context(), "past"))
		}, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			_, err := d.RestoreSession(t.Context(), "past")
			require.NoError(t, err)
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 1)
		}},
		{name: "redate", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "past", "project-a", "2026-06-17")
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Empty(t, a.Sessions)
		}},
		{name: "rename", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.RenameSession(t.Context(), "past", new("Renamed report session")))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			require.Len(t, a.Sessions, 1)
			assert.Equal(t, "Renamed report session", a.Sessions[0].Title)
		}},
		{name: "catalog pricing", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "kept-model", OutputPerMTok: money.MustParseDollars("10")}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, money.MustParseDollars("0.005"), a.Report.Totals.Cost)
		}},
		{name: "custom pricing", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			d.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, money.MustParseDollars("0.01"), a.Report.Totals.Cost)
		}},
		{name: "project identity", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), pgKeptIdentity("past", 14)))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.NotEmpty(t, a.Report.Projects)
		}},
		{name: "terminal after range", write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "old", "project-a", "2026-06-12")
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "old", []db.Message{{
				SessionID: "old", Ordinal: 0, Role: "assistant", Content: "tool", Timestamp: "2026-06-12T10:00:00Z",
				ToolCalls: []db.ToolCall{{ToolUseID: "call", ToolName: "shell", ResultEvents: []db.ToolResultEvent{{Source: "tool_execution", Status: "completed", Timestamp: "2026-06-16T10:00:00Z"}}}},
			}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Len(t, a.Sessions, 2)
		}},
		{name: "ineligible Claude peer", keep: true, prepare: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "past", []db.Message{{SessionID: "past", Ordinal: 0, Role: "assistant", Content: "answer", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":500}`), ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"}}))
		}, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "peer", "project-b", "2026-06-15")
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "peer", []db.Message{{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "no usage", Timestamp: "2026-06-15T10:01:00Z", Model: "kept-model", ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, 500, a.Report.Totals.OutputTokens)
		}},
		{name: "excluded Claude peer", prepare: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "peer", "project-b", "2026-06-14")
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "past", []db.Message{{SessionID: "past", Ordinal: 0, Role: "assistant", Content: "partial", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":5}`), ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"}}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "peer", []db.Message{{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":631}`), ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request"}}))
		}, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			require.NoError(t, d.DeleteSession(t.Context(), "peer"))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, 5, a.Report.Totals.OutputTokens)
		}},
		{name: "Claude peer of peer", keep: true, prepare: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "peer", "project-b", "2026-06-14")
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "past", []db.Message{{SessionID: "past", Ordinal: 0, Role: "assistant", Content: "partial", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":5}`), ClaudeMessageID: "message-a", ClaudeRequestID: "request-a"}}))
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "peer", []db.Message{
				{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":631}`), ClaudeMessageID: "message-a", ClaudeRequestID: "request-a"},
				{SessionID: "peer", Ordinal: 1, Role: "assistant", Content: "other", Timestamp: "2026-06-14T10:02:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":900}`), ClaudeMessageID: "message-b", ClaudeRequestID: "request-b"},
			}))
		}, write: func(t *testing.T, d *db.DB) {
			t.Helper()
			pgKeptSession(t, d, "other", "project-b", "2026-06-15")
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "other", []db.Message{{SessionID: "other", Ordinal: 0, Role: "assistant", Content: "other", Timestamp: "2026-06-15T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":1000}`), ClaudeMessageID: "message-b", ClaudeRequestID: "request-b"}}))
		}, check: func(t *testing.T, a activity.CandidateArtifacts) {
			t.Helper()
			assert.Equal(t, 631, a.Report.Totals.OutputTokens)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, syncer, store := newUsageJournalFixture(t)
			pgKeptSession(t, d, "past", "project-a", "2026-06-14")
			if tc.prepare != nil {
				tc.prepare(t, d)
			}
			tz := tc.timezone
			if tz == "" {
				tz = "UTC"
			}
			q, f := pgDayQuery(t, "2026-06-14", tz), db.AnalyticsFilter{Timezone: tz, Project: "project-a"}
			pgKeptPush(t, syncer, false)
			before := pgKeptBuild(t, store, f, q, true)
			requirePGKeptEqual(t, before, pgKeptBuild(t, store, f, q, false))
			tc.write(t, d)
			if tc.name == "custom pricing" {
				store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
			}
			pgKeptPush(t, syncer, tc.name == "hard delete" || tc.name == "excluded Claude peer")
			changed := pgKeptBuild(t, store, f, q, !tc.keep)
			tc.check(t, changed)
			a, err := activity.ArtifactDigest(before)
			require.NoError(t, err)
			b, err := activity.ArtifactDigest(changed)
			require.NoError(t, err)
			if tc.keep {
				assert.Equal(t, a, b)
			} else {
				assert.NotEqual(t, a, b)
			}
			store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
			requirePGKeptEqual(t, changed, pgKeptBuild(t, store, f, q, true))
		})
	}
}

func TestPGActivityReportKeptPushDuringBuild(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "past", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	pushed := false
	_, err := store.BuildActivityReportArtifacts(t.Context(), f, q, func(p activity.Progress) {
		if p.Phase == activity.ProgressLoadingUsage {
			require.NoError(t, local.RenameSession(t.Context(), "past", new("Renamed during build")))
			pgKeptPush(t, syncer, false)
			pushed = true
		}
	})
	require.NoError(t, err)
	require.True(t, pushed)
	changed := pgKeptBuild(t, store, f, q, true)
	require.Len(t, changed.Sessions, 1)
	assert.Equal(t, "Renamed during build", changed.Sessions[0].Title)
	store.runtimeState().activityKept.DeleteSlots(func(string) bool { return true })
	requirePGKeptEqual(t, changed, pgKeptBuild(t, store, f, q, true))
}

func TestPGActivityReportKeptBypasses(t *testing.T) {
	for _, mode := range []string{"partial", "hosted", "missing journal", "unreadable journal", "active", "stale", "unclean", "clean, active"} {
		t.Run(mode, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			pgKeptSession(t, local, "past", "project-a", "2026-06-14")
			pgKeptPush(t, syncer, false)
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			wantTokens := 500
			switch mode {
			case "active", "stale", "unclean", "clean, active":
				f.Termination = mode
				wantTokens = 0
			case "partial":
				var err error
				q, err = activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-06-14", Timezone: "UTC"}, time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC))
				require.NoError(t, err)
			case "hosted":
				store.hostedRelations = true
			case "missing journal":
				_, err := syncer.pg.ExecContext(t.Context(), dropUsageChangeJournalSQL)
				require.NoError(t, err)
			case "unreadable journal":
				reader, err := NewStore(t.Context(), newUsageJournalRole(t, syncer, `GRANT SELECT ON ALL TABLES IN SCHEMA `+syncer.schema+` TO ROLE; REVOKE SELECT ON usage_session_changes FROM ROLE`), syncer.schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				store = reader
			}
			for range 2 {
				assert.Equal(t, wantTokens, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
			}
		})
	}
}

func TestPGActivityReportKeptJournalRecreated(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "past", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	require.Len(t, pgKeptBuild(t, store, f, q, true).Sessions, 1)
	_, err := syncer.pg.ExecContext(t.Context(), dropUsageChangeJournalSQL)
	require.NoError(t, err)
	require.NoError(t, local.DeleteSession(t.Context(), "past"))
	// Publish while the journal is absent, then reinstall it as a schema reset does.
	pgKeptPush(t, syncer, true)
	ensureUsageChangeJournalPG(t.Context(), syncer.pg)
	require.Empty(t, pgKeptBuild(t, store, f, q, true).Sessions)
}
