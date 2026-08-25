package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func reportSessionIDs(sessions []activity.SessionRow) map[string]struct{} {
	out := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		out[s.SessionID] = struct{}{}
	}
	return out
}

// dayQuery resolves a single-day "day" Query for date/tz against a fixed
// far-future now, so the candidate range is the full local day and the
// report is never partial regardless of the wall clock.
func dayQuery(t *testing.T, date, tz string) activity.Query {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "day", Date: date, Timezone: tz}, now)
	require.NoError(t, err)
	return q
}

func seedMessage(
	t *testing.T, d *DB, sid string, ordinal int, role, ts, model string,
) {
	t.Helper()
	insertMessages(t, d, Message{
		SessionID: sid,
		Ordinal:   ordinal,
		Role:      role,
		Content:   "x",
		Timestamp: ts,
		Model:     model,
	})
}

func TestSQLiteActivityReportCandidatesMatchGoPairingAtScanBounds(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "edge", "p", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-15T23:54:59Z")
		s.EndedAt = Ptr("2026-06-17T00:20:00Z")
	})
	events := []activity.ActivityEvent{
		{SessionID: "edge", Ordinal: 1, Timestamp: "2026-06-15T23:54:59Z", Role: "user"},
		{SessionID: "edge", Ordinal: 2, Timestamp: "2026-06-15T23:59:30Z", Role: "assistant", Model: "old"},
		{SessionID: "edge", Ordinal: 3, Timestamp: "2026-06-15T23:59:20Z", Role: "assistant", Model: "ignored"},
		{SessionID: "edge", Ordinal: 4, Timestamp: "", Role: "user"},
		{SessionID: "edge", Ordinal: 5, Timestamp: "not-a-timestamp", Role: "assistant", Model: "malformed"},
		{SessionID: "edge", Ordinal: 6, Timestamp: "2026-06-16T23:59:00Z", Role: "user"},
		{SessionID: "edge", Ordinal: 7, Timestamp: "2026-06-17T00:20:00Z", Role: "assistant", Model: "new"},
	}
	for _, event := range events {
		seedMessage(t, d, event.SessionID, event.Ordinal, event.Role, event.Timestamp, event.Model)
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	want := activity.PairActivityEvents(
		events, q.RangeStart, q.EffectiveEnd,
		time.Duration(q.GapCapSeconds)*time.Second,
	)

	got, err := d.activityReportCandidates(ctx, []string{"edge"}, q)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSQLiteActivityReportCandidateQueryUsesExistingSessionIndex(t *testing.T) {
	d := testDB(t)
	q := dayQuery(t, "2026-06-16", "UTC")
	rows, err := d.getReader().QueryContext(
		context.Background(), "EXPLAIN QUERY PLAN "+activityReportCandidatesSQL,
		`["session"]`, "2026-06-15T09:00:00Z", "2026-06-17T14:00:00Z",
		q.RangeStart.Add(-5*time.Minute).UnixMicro(), q.EffectiveEnd.UnixMicro(),
	)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "idx_messages_velocity")
	assert.NotContains(t, plan, "idx_messages_activity_timestamp")
	assert.NotContains(t, plan, "SCAN m")
}

func TestSQLiteActivityReportCandidateSourceStopsOnCancellation(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "cancel", "p", func(s *Session) {
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:03:00Z")
	})
	for ordinal, timestamp := range []string{
		"2026-06-16T10:00:00Z",
		"2026-06-16T10:01:00Z",
		"2026-06-16T10:02:00Z",
		"2026-06-16T10:03:00Z",
	} {
		seedMessage(t, d, "cancel", ordinal, "user", timestamp, "")
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	err := d.activityReportCandidateSource(
		[]string{"cancel"}, dayQuery(t, "2026-06-16", "UTC"),
	)(ctx, func(activity.IntervalCandidate) error {
		seen++
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, seen)
}

func TestSQLiteActivityReportCandidateSourceDoesNotRequireGlobalTimestampIndex(
	t *testing.T,
) {
	d := testDB(t)
	_, err := d.getWriter().Exec(`DROP INDEX IF EXISTS idx_messages_activity_timestamp`)
	require.NoError(t, err)
	insertSession(t, d, "legacy-index", "p", func(s *Session) {
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:01:00Z")
	})
	seedMessage(t, d, "legacy-index", 0, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "legacy-index", 1, "assistant", "2026-06-16T10:01:00Z", "model")

	candidates, err := d.activityReportCandidates(
		context.Background(), []string{"legacy-index"},
		dayQuery(t, "2026-06-16", "UTC"),
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
}

func TestGetActivityReport_InlineToolCompletionResetsGapCap(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "inline-completion", "tools", func(s *Session) {
		s.Agent = "grok"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:11:00Z")
	})
	seedMessage(t, d, "inline-completion", 0, "user",
		"2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "inline-completion", 1, "assistant",
		"2026-06-16T10:01:00Z", "model-x")
	seedMessage(t, d, "inline-completion", 2, "assistant",
		"2026-06-16T10:11:00Z", "model-x")
	timingInsertToolResultEvent(t, d, "inline-completion", 1, 0,
		"sample-call", "completed", "2026-06-16T10:10:00Z", 1)

	report, err := d.GetActivityReport(
		context.Background(), AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"),
	)
	require.NoError(t, err)
	require.Len(t, report.BySession, 1)
	require.NotNil(t, report.BySession[0].AgentMinutes)
	assert.InDelta(t, 7.0, *report.BySession[0].AgentMinutes, 1e-9)
}

func TestGetActivityReport_ToolCompletionAfterRangeClosesFinalMessage(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "completion-after-range", "tools", func(s *Session) {
		s.Agent = "grok"
		s.StartedAt = Ptr("2026-06-16T23:59:00Z")
		s.EndedAt = Ptr("2026-06-17T00:01:00Z")
	})
	seedMessage(t, d, "completion-after-range", 0, "assistant",
		"2026-06-16T23:59:00Z", "model-x")
	timingInsertToolResultEvent(t, d, "completion-after-range", 0, 0,
		"sample-call", "completed", "2026-06-17T00:01:00Z", 1)

	report, err := d.GetActivityReport(
		context.Background(), AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"),
	)
	require.NoError(t, err)
	require.Len(t, report.BySession, 1)
	require.NotNil(t, report.BySession[0].AgentMinutes)
	assert.InDelta(t, 1.0, *report.BySession[0].AgentMinutes, 1e-9)
}

func BenchmarkSQLiteActivityReportCandidateSource100K(b *testing.B) {
	d, ids, q := seedSQLiteActivityReportBenchmark(b)
	source := d.activityReportCandidateSource(ids, q)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		count := 0
		err := source(context.Background(), func(activity.IntervalCandidate) error {
			count++
			return nil
		})
		require.NoError(b, err)
		require.Equal(b, 90_000, count)
	}
}

// BenchmarkSQLiteActivityReportCandidateSourceLongSession covers the opposite
// archive shape from the many-session benchmark: one transcript with a large
// ordinal index range. It guards against accidentally turning the successor or
// prior-model lookup into a per-candidate linear scan.
func BenchmarkSQLiteActivityReportCandidateSourceLongSession(b *testing.B) {
	d := testDB(b)
	const messageCount = 100_001
	_, err := d.getWriter().Exec(`
		INSERT INTO sessions(
			id, project, started_at, ended_at, message_count
		) VALUES (
			'bench-long', 'bench', '2026-07-01T00:00:00Z',
			'2026-07-02T03:46:40Z', ?
		)`, messageCount)
	require.NoError(b, err)
	_, err = d.getWriter().Exec(`
		WITH RECURSIVE n(i) AS (
			VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < ?
		)
		INSERT INTO messages(
			session_id, ordinal, role, content, timestamp, model
		)
		SELECT 'bench-long', i,
			CASE WHEN i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			'x',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:00:00Z',
				printf('+%d seconds', i)),
			CASE WHEN i % 2 = 0 THEN '' ELSE 'model' END
		FROM n`, messageCount-1)
	require.NoError(b, err)
	full, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "month", Date: "2026-07-01", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	narrow, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-07-02T00:00:00Z", To: "2026-07-02T01:00:00Z",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	for _, benchmark := range []struct {
		name string
		q    activity.Query
		want int
	}{
		{name: "full-100k", q: full, want: messageCount - 1},
		{name: "narrow-3900", q: narrow, want: 3900},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			source := d.activityReportCandidateSource([]string{"bench-long"}, benchmark.q)
			b.ReportAllocs()
			for range b.N {
				count := 0
				err := source(context.Background(), func(activity.IntervalCandidate) error {
					count++
					return nil
				})
				require.NoError(b, err)
				require.Equal(b, benchmark.want, count)
			}
		})
	}
}

func BenchmarkSQLiteActivityReportArtifacts100K(b *testing.B) {
	d, _, q := seedSQLiteActivityReportBenchmark(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		artifacts, err := d.BuildActivityReportArtifacts(
			context.Background(), AnalyticsFilter{Timezone: "UTC"}, q, nil,
		)
		require.NoError(b, err)
		require.Len(b, artifacts.Sessions, 100_000)
		page, err := activity.PageSessions(
			artifacts.Sessions, artifacts.Membership, activity.SessionPageOptions{},
		)
		require.NoError(b, err)
		report := artifacts.Report
		report.BySession = page.Sessions
		encoded, err := json.Marshal(report)
		require.NoError(b, err)
		b.ReportMetric(float64(len(encoded)), "response_bytes")
	}
}

func seedSQLiteActivityReportBenchmark(
	b *testing.B,
) (*DB, []string, activity.Query) {
	b.Helper()
	d := testDB(b)
	const sessionCount = 100_000
	_, err := d.getWriter().Exec(`
		WITH RECURSIVE n(i) AS (
			VALUES(1) UNION ALL SELECT i + 1 FROM n WHERE i < ?
		)
		INSERT INTO sessions(id, project, started_at, ended_at, message_count)
		SELECT printf('bench-%06d', i), 'bench',
			'2026-07-01T10:00:00Z', '2026-07-28T10:02:00Z', 2
		FROM n`, sessionCount)
	require.NoError(b, err)
	_, err = d.getWriter().Exec(`
		WITH RECURSIVE n(i) AS (
			VALUES(1) UNION ALL SELECT i + 1 FROM n WHERE i < ?
		)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model)
		SELECT printf('bench-%06d', i), 1, 'user', 'x',
			printf('2026-07-%02dT10:00:00Z', 1 + (i % 28)), ''
		FROM n
		UNION ALL
		SELECT printf('bench-%06d', i), 2, 'assistant', 'x',
			printf('2026-07-%02dT10:02:00Z', 1 + (i % 28)), 'model'
		FROM n WHERE i % 10 != 0`, sessionCount)
	require.NoError(b, err)
	ids := make([]string, sessionCount)
	for i := range ids {
		ids[i] = fmt.Sprintf("bench-%06d", i+1)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "month", Date: "2026-07-01", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	return d, ids, q
}

func TestGetActivityReport_BasicConcurrency(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// Two overlapping sessions on 2026-06-16 (UTC), each two messages.
	// started_at/ended_at are set explicitly so the candidate-session
	// window anchors on the target day regardless of the wall clock; a
	// created_at fallback would drift to the prior/next day when the
	// suite runs near UTC midnight.
	insertSession(t, d, "a", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "a", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "a", 2, "assistant", "2026-06-16T10:02:00Z", "opus")
	insertSession(t, d, "b", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-16T10:01:00Z")
		s.EndedAt = Ptr("2026-06-16T10:03:00Z")
	})
	seedMessage(t, d, "b", 1, "user", "2026-06-16T10:01:00Z", "")
	seedMessage(t, d, "b", 2, "assistant", "2026-06-16T10:03:00Z", "gpt5")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 2, r.Peak.Agents)
	assert.Equal(t, 2, r.Totals.Sessions)
	assert.Equal(t, 2, r.SessionsTotal)
	assert.GreaterOrEqual(t, len(r.ByModel), 2)
}

func TestActivityReportEmptyProjectsMapExcludesUnrelatedObservations(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedProjectIdentityObservation(t, d, "unrelated-project")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Empty(t, r.BySession)
	assert.Empty(t, r.Projects)
}

func TestGetActivityReport_UsageCostAndTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "x",
		Timestamp:  "2026-06-16T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.Equal(t, money.MustParseDollars("0.0105"), r.Totals.Cost)
	require.NotNil(t, r.Pricing)
	provenance := r.Pricing.Models["claude-sonnet-4-20250514"]
	require.Len(t, provenance.Resolutions, 1)
	assert.Equal(t, 1,
		provenance.Resolutions[0].Application.BaseRequestCount)
}

func TestGetActivityReportFiltersAfterCrossSessionSnapshotSelection(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-parent", "parent-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:00:00Z")
	})
	insertSession(t, d, "activity-child", "child-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:01:00Z")
		s.EndedAt = Ptr("2026-06-16T10:01:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-parent", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:00:00Z", Model: "partial-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":10,"output_tokens":5}`),
			ClaudeMessageID: "activity-message",
			ClaudeRequestID: "activity-request",
		},
		Message{
			SessionID: "activity-child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:01:00Z", Model: "complete-model",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":631}`),
			ClaudeMessageID: "activity-message",
			ClaudeRequestID: "activity-request",
		},
	)

	parentReport, err := d.GetActivityReport(ctx, AnalyticsFilter{
		Project: "parent-project", Timezone: "UTC",
	}, dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, parentReport.Totals.Sessions)
	assert.Equal(t, 631, parentReport.Totals.OutputTokens,
		"the parent filter must retain the complete child snapshot")

	childReport, err := d.GetActivityReport(ctx, AnalyticsFilter{
		Project: "child-project", Timezone: "UTC",
	}, dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, childReport.Totals.Sessions)
	assert.Zero(t, childReport.Totals.OutputTokens,
		"the child source must not claim usage attributed to the parent")
}

func TestGetActivityReportDeduplicatesAfterProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "excluded-earlier", "excluded-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:00:00Z")
	})
	insertSession(t, d, "included-later", "included-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:01:00Z")
		s.EndedAt = Ptr("2026-06-16T10:01:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "excluded-earlier", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:00:00Z", Model: "model-x",
			TokenUsage: jsontext.Value(`{"input_tokens":10,"output_tokens":5}`),
			SourceUUID: "shared-source",
		},
		Message{
			SessionID: "included-later", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:01:00Z", Model: "model-x",
			TokenUsage: jsontext.Value(`{"input_tokens":20,"output_tokens":631}`),
			SourceUUID: "shared-source",
		},
	)

	report, err := d.GetActivityReport(ctx, AnalyticsFilter{
		Project: "included-project", Timezone: "UTC",
	}, dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 631, report.Totals.OutputTokens,
		"an excluded duplicate must not suppress included usage")
}

func TestLoadActivityReportUsageCandidatesBoundsFilteredWorkingSet(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "candidate", "included-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:00:00Z")
	})
	insertSession(t, d, "snapshot-peer", "excluded-project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:01:00Z")
		s.EndedAt = Ptr("2026-06-16T10:01:00Z")
	})
	insertMessages(t, d,
		Message{
			SessionID: "candidate", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:00:00Z", Model: "partial-model",
			TokenUsage:      jsontext.Value(`{"input_tokens":10,"output_tokens":5}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
		Message{
			SessionID: "snapshot-peer", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:01:00Z", Model: "complete-model",
			TokenUsage:      jsontext.Value(`{"input_tokens":20,"output_tokens":631}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		},
	)
	for i := range 64 {
		id := fmt.Sprintf("unrelated-%02d", i)
		insertSession(t, d, id, "excluded-project", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = Ptr("2026-06-16T11:00:00Z")
			s.EndedAt = Ptr("2026-06-16T11:00:00Z")
		})
		insertMessages(t, d, Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T11:00:00Z", Model: "unrelated-model",
			TokenUsage: jsontext.Value(`{"input_tokens":1,"output_tokens":1}`),
		})
	}

	candidates, _, err := d.loadActivityReportUsageCandidatesFrom(
		ctx, d.getReader(), []string{"candidate"},
		"2026-06-15T10:00:00Z", "2026-06-17T10:00:00Z", false)
	require.NoError(t, err)
	require.Len(t, candidates, 2,
		"the working set contains the candidate and its Claude peer only")
	assert.ElementsMatch(t, []string{"candidate", "snapshot-peer"}, []string{
		candidates[0].row.SessionID,
		candidates[1].row.SessionID,
	})
}

func TestGetSessionUsageRowsPrefersCompleteClaudeSnapshotAcrossSessions(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-test",
		OutputPerMTok: money.MustParseDollars("1"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "root", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
	})
	insertSession(t, d, "child", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:02Z")
		s.ParentSessionID = Ptr("root")
		s.RelationshipType = "subagent"
	})
	insertMessages(t, d,
		Message{
			SessionID: "root", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:00:00Z",
			Model:     "claude-sonnet-test",
			TokenUsage: jsontext.Value(
				`{"input_tokens":2,"output_tokens":5}`),
			ClaudeMessageID: "msg-stream",
			ClaudeRequestID: "req-stream",
		},
		Message{
			SessionID: "child", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-06-16T10:00:01Z",
			Model:     "claude-sonnet-test",
			TokenUsage: jsontext.Value(
				`{"input_tokens":2,"output_tokens":631}`),
			ClaudeMessageID: "msg-stream",
			ClaudeRequestID: "req-stream",
		},
	)

	rowSet, err := d.GetSessionUsageRows(ctx, []string{"root", "child"})
	require.NoError(t, err)
	require.Len(t, rowSet.Rows, 1)
	assert.Equal(t, "root", rowSet.Rows[0].SessionID)
	assert.Equal(t, "child", rowSet.Rows[0].SourceSessionID)
	assert.Equal(t, 631, rowSet.Rows[0].OutputTokens)
	assert.Equal(t, map[string]int{"root": 5, "child": 631},
		rowSet.RawOutputTokensBySession)
	assert.Equal(t, map[string]int{"root": 5, "child": 631},
		rowSet.DeduplicatedOutputTokens)
	assert.Equal(t, map[string]struct{}{"root": {}, "child": {}},
		rowSet.DiscardedContributingSessions)
	assert.Equal(t, map[string]activity.SessionTokenCoverage{
		"root":  {OutputTokens: 631, PeakContextTokens: 2},
		"child": {OutputTokens: 631, PeakContextTokens: 2},
	}, rowSet.CanonicalTokenCoverageBySession)
}

func TestSQLiteSessionUsageRowLessEquivalentInstantUsesSessionOrder(t *testing.T) {
	instant := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	parent := sqliteSessionUsageOrderedRow{
		scan: usageScanRow{
			sessionID: "a-parent",
			ts:        "2026-06-16T10:00:00Z",
		},
		ts:      instant,
		validTS: true,
	}
	child := sqliteSessionUsageOrderedRow{
		scan: usageScanRow{
			sessionID: "z-child",
			ts:        "2026-06-16T05:00:00-05:00",
		},
		ts:      instant,
		validTS: true,
	}
	sessionOrder := map[string]int{"a-parent": 0, "z-child": 1}

	assert.True(t, sqliteSessionUsageRowLess(parent, child, sessionOrder))
	assert.False(t, sqliteSessionUsageRowLess(child, parent, sessionOrder))
}

func TestSQLiteActivityReportRowStatusCanonicalizesKimiAliasByTimestamp(t *testing.T) {
	tests := []struct {
		name         string
		timestamp    string
		canonical    string
		expectedCost money.Money
	}{
		{
			name:         "before cutoff",
			timestamp:    "2026-07-18T23:59:59Z",
			canonical:    pricingpkg.KimiK26Canonical,
			expectedCost: money.MustParseDollars("1"),
		},
		{
			name:         "at cutoff",
			timestamp:    "2026-07-19T00:00:00Z",
			canonical:    pricingpkg.KimiK3Canonical,
			expectedCost: money.MustParseDollars("2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := export.NewPricingResolver([]export.EffectivePricingRow{
				{
					ModelPattern: pricingpkg.KimiK26Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("1"),
					},
				},
				{
					ModelPattern: pricingpkg.KimiK3Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("2"),
					},
				},
			})

			cost, priced, contributes, err := sqliteActivityReportRowStatus(
				dailyUsageScanRow{
					usageSource: "provider",
					model:       "daimon-kimi-code",
					ts:          tt.timestamp,
					pricingTS:   tt.timestamp,
					inputTokens: 1_000_000,
				},
				resolver,
			)

			require.NoError(t, err)
			assert.True(t, priced)
			assert.True(t, contributes)
			assert.Equal(t, tt.expectedCost, cost)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)
			require.Contains(t, block.Models, "daimon-kimi-code")
			resolutions := block.Models["daimon-kimi-code"].Resolutions
			require.Len(t, resolutions, 1)
			assert.Equal(t, tt.canonical, resolutions[0].PricedModel)
			assert.NotContains(t, block.Models, tt.canonical)
		})
	}
}

func TestSQLiteActivityReportRowStatusPrefersExactCustomKimiAlias(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "daimon-kimi-code",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	cost, priced, contributes, err := sqliteActivityReportRowStatus(
		dailyUsageScanRow{
			usageSource: "provider",
			model:       "daimon-kimi-code",
			ts:          "2026-07-19T00:00:00Z",
			pricingTS:   "2026-07-19T00:00:00Z",
			inputTokens: 1_000_000,
		},
		resolver,
	)

	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "daimon-kimi-code")
	resolutions := block.Models["daimon-kimi-code"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "daimon-kimi-code", resolutions[0].PricedModel)
}

func TestGetActivityReport_CopilotReportedCostReplacesSessionEstimates(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "copilot-model-a", InputPerMTok: money.MustParseDollars("10")},
		{ModelPattern: "copilot-model-b", InputPerMTok: money.MustParseDollars("20")},
	}))
	insertSession(t, d, "copilot:activity-authoritative", "proj1", func(s *Session) {
		s.Agent = "copilot"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:10:00Z")
	})
	reportedCost := money.MustParseDollars("0.03")
	require.NoError(t, d.ReplaceSessionUsageEvents(
		"copilot:activity-authoritative",
		[]UsageEvent{
			{
				Source: "shutdown", Model: "copilot-model-a",
				InputTokens: 1_000_000,
				OccurredAt:  "2026-06-16T10:05:00Z", DedupKey: "first",
			},
			{
				Source: "shutdown", Model: "copilot-model-b",
				InputTokens: 1_000_000,
				Cost:        &reportedCost, CostStatus: "exact",
				CostSource: CopilotReportedCostSource,
				OccurredAt: "2026-06-16T10:10:00Z", DedupKey: "final",
			},
		},
	))

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, reportedCost, r.Totals.Cost)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, reportedCost, r.BySession[0].Cost)
	modelCosts := make(map[string]money.Money, len(r.ByModel))
	for _, model := range r.ByModel {
		modelCosts[model.Key] = model.Cost
	}
	assert.Equal(t, money.MustParseDollars("0.01"), modelCosts["copilot-model-a"])
	assert.Equal(t, money.MustParseDollars("0.02"), modelCosts["copilot-model-b"])
	assert.Equal(t, r.Totals.Cost,
		money.MustAdd(modelCosts["copilot-model-a"], modelCosts["copilot-model-b"]))
}

func TestGetActivityReport_PricingModelsOnlyIncludeDedupSurvivors(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "partial-model",
			InputPerMTok:  money.MustParseDollars("3.0"),
			OutputPerMTok: money.MustParseDollars("15.0"),
		},
		{
			ModelPattern:  "complete-model",
			InputPerMTok:  money.MustParseDollars("3.0"),
			OutputPerMTok: money.MustParseDollars("15.0"),
		},
	}), "UpsertModelPricing")

	insertSession(t, d, "earlier", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "earlier", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "partial-model",
		ClaudeMessageID: "m-dup", ClaudeRequestID: "r-dup",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "later", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:31:00Z")
		s.EndedAt = Ptr("2026-06-16T10:31:00Z")
		s.IsAutomated = true
	})
	insertMessages(t, d, Message{
		SessionID: "later", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:31:00Z",
		Model:           "complete-model",
		ClaudeMessageID: "m-dup", ClaudeRequestID: "r-dup",
		TokenUsage: jsontext.Value(`{"input_tokens":2000,"output_tokens":900}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 900, r.Totals.OutputTokens)
	assert.Equal(t, r.Totals.Cost, r.Totals.InteractiveCost)
	assert.Zero(t, r.Totals.AutomatedCost.Microdollars)
	bySession := make(map[string]activity.SessionRow, len(r.BySession))
	for _, session := range r.BySession {
		bySession[session.SessionID] = session
	}
	require.Contains(t, bySession, "earlier")
	require.Contains(t, bySession, "later")
	assert.Equal(t, 900, bySession["earlier"].OutputTokens)
	assert.Zero(t, bySession["later"].OutputTokens)
	require.NotNil(t, r.Pricing)
	assert.Contains(t, r.Pricing.Models, "complete-model")
	assert.NotContains(t, r.Pricing.Models, "partial-model")
}

// TestGetActivityReport_IncludesSubagentUsage confirms subagent and fork
// sessions are candidates so their usage lands in the totals, keeping the
// activity cost consistent with GetDailyUsage (which never filters by
// relationship_type). A fork whose only usage row replays the root's
// Claude ids contributes a session row but no extra cost: the aggregator's
// first-seen dedup collapses the duplicate, the same guarantee
// GetDailyUsage relies on.
func TestGetActivityReport_IncludesSubagentUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "root-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
		{ModelPattern: "sub-model", InputPerMTok: money.MustParseDollars("3.0"), OutputPerMTok: money.MustParseDollars("15.0")},
	}), "UpsertModelPricing")

	insertSession(t, d, "root", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:10:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "root", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: "2026-06-16T10:00:00Z", Model: "root-model",
		ClaudeMessageID: "m-root", ClaudeRequestID: "r-root",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "agent-sub", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.ParentSessionID = Ptr("root")
		s.RelationshipType = "subagent"
		s.StartedAt = Ptr("2026-06-16T10:02:00Z")
		s.EndedAt = Ptr("2026-06-16T10:04:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "agent-sub", Ordinal: 0, Role: "assistant", Content: "y",
		Timestamp: "2026-06-16T10:03:00Z", Model: "sub-model",
		ClaudeMessageID: "m-sub", ClaudeRequestID: "r-sub",
		TokenUsage: jsontext.Value(`{"input_tokens":2000,"output_tokens":700}`),
	})
	// Fork replaying the root's message: same Claude ids, so the dedup
	// must drop its usage row while the session itself still appears.
	insertSession(t, d, "fork", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.ParentSessionID = Ptr("root")
		s.RelationshipType = "fork"
		s.StartedAt = Ptr("2026-06-16T10:05:00Z")
		s.EndedAt = Ptr("2026-06-16T10:06:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "fork", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp: "2026-06-16T10:05:00Z", Model: "root-model",
		ClaudeMessageID: "m-root", ClaudeRequestID: "r-root",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "root")
	assert.Contains(t, ids, "agent-sub",
		"subagent session must be a candidate")
	assert.Contains(t, ids, "fork", "fork session must be a candidate")
	assert.Equal(t, 1200, r.Totals.OutputTokens,
		"totals include subagent usage; the fork's replayed row dedups away")
	// Cost = root (1000*3+500*15)/1e6 + subagent (2000*3+700*15)/1e6; the
	// fork's duplicate row contributes nothing.
	assert.Equal(t, money.MustParseDollars("0.027"), r.Totals.Cost)
}

// TestGetActivityReport_ExcludesOtherDays confirms the candidate-session
// window and the usage ts-bounds keep a session whose only activity
// falls outside the target day from contributing to that day.
func TestGetActivityReport_ExcludesOtherDays(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "today", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "today", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "today", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	insertSession(t, d, "yesterday", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-10T10:00:00Z")
		s.EndedAt = Ptr("2026-06-10T10:02:00Z")
	})
	seedMessage(t, d, "yesterday", 1, "user", "2026-06-10T10:00:00Z", "")
	seedMessage(t, d, "yesterday", 2, "assistant", "2026-06-10T10:02:00Z", "gpt5")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	// Only the in-day session has timed intervals on 2026-06-16.
	assert.Equal(t, 1, r.Peak.Agents)
	require.Len(t, r.ByAgent, 1)
	assert.Equal(t, "claude", r.ByAgent[0].Key)
}

// TestGetActivityReport_PriorDayWithinPadExcluded confirms the candidate
// window uses the EXACT local day, not the +/-14h padded bounds: a
// session that began and ended on the prior day but lands inside the
// pad must NOT appear as an untimed session in the target day's report.
func TestGetActivityReport_PriorDayWithinPadExcluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "today", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "today", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "today", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	// Prior-day session at 2026-06-15T12:00Z: within the old -14h pad
	// (2026-06-15T10:00Z) but outside the exact 2026-06-16 UTC window.
	insertSession(t, d, "prior", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-15T12:00:00Z")
		s.EndedAt = Ptr("2026-06-15T12:05:00Z")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "today")
	assert.NotContains(t, ids, "prior", "prior-day session must not leak in")
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 0, r.Totals.UntimedSessions)
}

// TestGetActivityReport_UntimedSessionOnDayIncluded confirms a session
// that started on the target day but has no timestamped messages still
// appears in the report as an untimed candidate.
func TestGetActivityReport_UntimedSessionOnDayIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "untimed", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T09:00:00Z")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "untimed")
	assert.Equal(t, 1, r.Totals.UntimedSessions)
}

// TestGetActivityReport_EmptyStringEndedAtIncluded confirms the overlap
// predicate uses NULLIF so a session with an empty-string ended_at but a
// valid started_at on the target day is not excluded by COALESCE
// treating an empty string as a real upper bound.
func TestGetActivityReport_EmptyStringEndedAtIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "empty-end", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T09:00:00Z")
		s.EndedAt = Ptr("")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "empty-end", "empty ended_at must fall back to started_at")
}

// TestGetActivityReport_SubSecondDayStartIncluded confirms a session whose
// only activity lands in the first sub-second of the day is not dropped by
// SQLite's lexicographic TEXT comparison. A stored RFC3339Nano value like
// "2026-06-14T00:00:00.123Z" sorts before a Z-suffixed day-start bound
// ("2026-06-14T00:00:00Z") because '.' < 'Z', so a Z-suffixed bound would
// wrongly exclude it. The zone-less day bound fixes that.
func TestGetActivityReport_SubSecondDayStartIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "subsec", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-14T00:00:00.123Z")
		s.EndedAt = Ptr("2026-06-14T00:00:00.123Z")
	})
	seedMessage(t, d, "subsec", 0, "user", "2026-06-14T00:00:00.123Z", "")
	seedMessage(t, d, "subsec", 1, "assistant", "2026-06-14T00:00:00.456Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "subsec",
		"first-sub-second session must not be dropped by the day-start bound")
	assert.GreaterOrEqual(t, r.Totals.Sessions, 1)
}

// TestGetActivityReport_ExcludesIneligibleUsage confirms the usage union
// applies the same eligibility filters as GetDailyUsage: a message with
// an empty model and empty token_usage must not inflate the day totals.
func TestGetActivityReport_ExcludesIneligibleUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:31:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "x",
		Timestamp:  "2026-06-16T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	// Ineligible: a synthetic-model message carrying real token_usage.
	// usageMessageEligibility drops m.model == '<synthetic>', so these
	// tokens must NOT leak into the day totals even though the blob is
	// non-empty.
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    1,
		Role:       "assistant",
		Content:    "y",
		Timestamp:  "2026-06-16T10:31:00Z",
		Model:      "<synthetic>",
		TokenUsage: jsontext.Value(`{"input_tokens":9000,"output_tokens":7000}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens, "synthetic message excluded")
	assert.Equal(t, money.MustParseDollars("0.0105"), r.Totals.Cost)
}

// TestGetActivityReport_HourlyRange exercises a multi-day custom range so
// the bucket auto-policy selects hourly buckets, and confirms the fetch
// window spans the whole range: a session whose only activity falls on the
// middle day populates the hourly bucket that contains it.
func TestGetActivityReport_HourlyRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "mid", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-17T10:00:00Z")
		s.EndedAt = Ptr("2026-06-17T10:30:00Z")
	})
	seedMessage(t, d, "mid", 1, "user", "2026-06-17T10:00:00Z", "")
	seedMessage(t, d, "mid", 2, "assistant", "2026-06-17T10:30:00Z", "opus")

	// 3-day span -> hourly buckets per the auto policy.
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-16T00:00:00Z", To: "2026-06-19T00:00:00Z",
	}, now)
	require.NoError(t, err)

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err)
	assert.Equal(t, "hour", r.BucketUnit)
	assert.Equal(t, 72, r.BucketCount, "3 days of hourly buckets")
	// The 30-min gap caps to 5 min and lands in the 2026-06-17T10:00 bucket.
	var found bool
	for _, b := range r.Buckets {
		if b.Start == "2026-06-17T10:00:00Z" {
			found = true
			assert.Equal(t, "2026-06-17T11:00:00Z", b.End)
			assert.InDelta(t, 5.0, b.AgentMinutes, 1e-9,
				"mid-range hourly bucket is populated")
		}
	}
	assert.True(t, found, "the 2026-06-17T10:00 hourly bucket must be present")
}

// TestGetActivityReport_UsageDedupSubSecondOrder confirms the SQLite usage
// stream is ordered by the PARSED instant, not the RFC3339 text. A
// resumed/forked pair shares one source UUID fallback dedup
// key in the same second: one whole-second instant ("...00Z", 500 output
// tokens) and one fractional ("...00.123Z", 9000). Lexically "...00.123Z"
// sorts before "...00Z" ('.' < 'Z'), so a TEXT sort would keep the 9000 row;
// chronologically the whole-second row is first. First-seen-wins dedup must
// keep the 500 row, matching PostgreSQL/DuckDB which order on the parsed time.
func TestGetActivityReport_UsageDedupSubSecondOrder(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "earlier", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "earlier", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup", SourceUUID: "src-dup",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "later", "proj2", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "later", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00.123Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup", SourceUUID: "src-dup",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":9000}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"first-seen dedup keeps the chronologically earlier whole-second row")
}

func TestGetActivityReport_UsageDedupEqualInstantUsesSessionOrder(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "a-session", "project a", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "a-session", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-equal", SourceUUID: "src-equal",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "z-session", "project z", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "z-session", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T05:30:00-05:00",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-equal", SourceUUID: "src-equal",
		TokenUsage: jsontext.Value(`{"input_tokens":1000,"output_tokens":9000}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"equal parsed instants fall through to session ID ordering")
}

func TestGetActivityReport_UsageDedupFallsBackToSourceUUID(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  money.MustParseDollars("3.0"),
		OutputPerMTok: money.MustParseDollars("15.0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "earlier", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:       "earlier",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup",
		ClaudeRequestID: "",
		SourceUUID:      "src-dup",
		TokenUsage:      jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "later", "proj2", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:01Z")
		s.EndedAt = Ptr("2026-06-16T10:30:01Z")
	})
	insertMessages(t, d, Message{
		SessionID:       "later",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "x",
		Timestamp:       "2026-06-16T10:30:01Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup",
		ClaudeRequestID: "",
		SourceUUID:      "src-dup",
		TokenUsage:      jsontext.Value(`{"input_tokens":1000,"output_tokens":900}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"incomplete Claude pairs fall back to source_uuid dedup in activity reports")
}

// TestGetActivityReport_TitleSkipsEmptyDisplayName confirms the Title fallback
// null-checks each candidate independently: an empty (non-NULL) display_name
// must not mask a real session_name. A nested COALESCE(display_name,
// session_name) would return an empty string and be NULLIF'd away, wrongly
// skipping to first_message. RenameSession stores a literal empty string (only
// nil clears to NULL), so this reproduces a session renamed to "" that still
// has a session_name.
func TestGetActivityReport_TitleSkipsEmptyDisplayName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s", "proj", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = Ptr("real-session-name")
		s.FirstMessage = Ptr("first message text")
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	require.NoError(t, d.RenameSession("s", Ptr("")))
	seedMessage(t, d, "s", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "s", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, "real-session-name", r.BySession[0].Title,
		"empty display_name must not mask the real session_name")
}

func TestGetActivityReport_TitleNeverFallsBackToFirstMessage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "private-title", "safe-project", func(s *Session) {
		s.Agent = "claude"
		s.FirstMessage = Ptr("distinctive private prompt sentinel")
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "private-title", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "private-title", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, "safe-project", r.BySession[0].Title)
	assert.NotContains(t, r.BySession[0].Title, "private prompt sentinel")
}

// TestGetActivityReport_OpenSessionWithInRangeMessageIncluded confirms a
// still-open session (no ended_at) that started before the range but has a
// message inside it is not dropped. The effective-end fallback uses the
// session's latest message timestamp, not started_at, so the overlap
// predicate sees the in-range activity. Without the fix, ended_at falls back
// to the pre-range started_at and the session vanishes from the report.
func TestGetActivityReport_OpenSessionWithInRangeMessageIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Started the day before and never closed (no ended_at), active in-range.
	insertSession(t, d, "open", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-15T23:00:00Z")
	})
	seedMessage(t, d, "open", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "open", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "open",
		"open session active in-range must not be dropped by the started_at fallback")
	assert.Equal(t, 1, r.Totals.Sessions)
}

// TestGetActivityReport_AutomationFilterAndSessionSplit confirms the
// AnalyticsFilter automation class selects the right sessions and that the
// Totals carry the automated/interactive session-count split. "all" keeps
// both classes; ExcludeAutomated keeps only interactive sessions;
// ExcludeInteractive (the mirror predicate) keeps only automated ones.
func TestGetActivityReport_AutomationFilterAndSessionSplit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two automated (roborev-style) sessions and one interactive, all timed
	// on 2026-06-16 so each is a candidate for that day's report. Automated
	// sessions are classified the way the sync path does it: a single-turn
	// session whose first message matches a known automated prompt prefix.
	for _, id := range []string{"auto1", "auto2"} {
		start := "2026-06-16T10:00:00Z"
		end := "2026-06-16T10:02:00Z"
		insertSession(t, d, id, "proj1", func(s *Session) {
			s.Agent = "claude"
			s.FirstMessage = Ptr("You are a code reviewer.")
			s.UserMessageCount = 1
			s.StartedAt = Ptr(start)
			s.EndedAt = Ptr(end)
		})
		seedMessage(t, d, id, 1, "user", start, "")
		seedMessage(t, d, id, 2, "assistant", end, "opus")
	}
	insertSession(t, d, "human", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-16T12:00:00Z")
		s.EndedAt = Ptr("2026-06-16T12:02:00Z")
	})
	seedMessage(t, d, "human", 1, "user", "2026-06-16T12:00:00Z", "")
	seedMessage(t, d, "human", 2, "assistant", "2026-06-16T12:02:00Z", "gpt5")

	tests := []struct {
		name            string
		filter          AnalyticsFilter
		wantAutomated   int
		wantInteractive int
		wantIDs         []string
	}{
		{
			name:            "all keeps both classes",
			filter:          AnalyticsFilter{Timezone: "UTC"},
			wantAutomated:   2,
			wantInteractive: 1,
			wantIDs:         []string{"auto1", "auto2", "human"},
		},
		{
			name:            "exclude automated keeps interactive only",
			filter:          AnalyticsFilter{Timezone: "UTC", ExcludeAutomated: true},
			wantAutomated:   0,
			wantInteractive: 1,
			wantIDs:         []string{"human"},
		},
		{
			name:            "exclude interactive keeps automated only",
			filter:          AnalyticsFilter{Timezone: "UTC", ExcludeInteractive: true},
			wantAutomated:   2,
			wantInteractive: 0,
			wantIDs:         []string{"auto1", "auto2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := d.GetActivityReport(ctx, tc.filter,
				dayQuery(t, "2026-06-16", "UTC"))
			require.NoError(t, err)
			assert.Equal(t, len(tc.wantIDs), r.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, r.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, r.Totals.InteractiveSessions)
			ids := reportSessionIDs(r.BySession)
			require.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}

// forceReaderVarLimit pins the reader pool to a single connection and lowers
// its SQLITE_LIMIT_VARIABLE_NUMBER to mimic older SQLite builds, whose limit is
// the documented 999 rather than the modern default (32766). Every read through
// d.getReader() then reuses this one constrained connection, so a query that
// binds too many variables fails exactly as it would on those builds.
func forceReaderVarLimit(t *testing.T, d *DB, limit int) {
	t.Helper()
	reader := d.rawReader()
	reader.SetMaxOpenConns(1)
	reader.SetMaxIdleConns(1)
	conn, err := reader.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.Raw(func(dc any) error {
		sc, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("reader conn is %T, want *sqlite3.SQLiteConn", dc)
		}
		sc.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, limit)
		return nil
	}))
}

// TestGetActivityReport_ManySessionsWithinSQLiteVarLimit reproduces the older
// SQLite 999-variable limit on the reader pool, then builds a report whose
// candidate set exceeds it. The usage fetch binds each id chunk twice (the
// message-where and usage-event-where subqueries) plus two time bounds, so a
// generic maxSQLVars chunk would emit 2*maxSQLVars+2 = 1002 variables and fail
// on such builds. The fetch must instead chunk small enough to stay within the
// limit while still aggregating usage across every chunk.
func TestGetActivityReport_ManySessionsWithinSQLiteVarLimit(t *testing.T) {
	d := openChunkedAnalyticsFixtureDB(t)
	ctx := context.Background()

	forceReaderVarLimit(t, d, 999)

	// Guard: prove the lowered limit is live on the pool, so a setup that
	// failed to constrain it cannot mask the regression checked below.
	overLimitPh, overLimitArgs := inPlaceholders(make([]string, 1001))
	_, probeErr := d.getReader().QueryContext(
		ctx, "SELECT 1 WHERE '' IN "+overLimitPh, overLimitArgs...)
	require.Error(t, probeErr, "reader variable limit was not constrained")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2024-06-01", "UTC"))
	require.NoError(t, err)
	assert.Len(t, reportSessionIDs(r.BySession),
		chunkedAnalyticsFixtureSessionCount,
		"every candidate session survives id chunking")
	assert.Positive(t, r.Totals.OutputTokens,
		"usage aggregated across all id chunks")
}
