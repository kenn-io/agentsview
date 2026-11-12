package db

import (
	"context"
	"encoding/json/jsontext"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// rawUsageActivityBackend builds reports from raw archive usage rows, the
// path that predates reading Activity usage from the usage cache.
type rawUsageActivityBackend struct{ activityReportBackend }

func TestActivityReportCachedUsageSignedCosts(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "project", func(s *Session) {
			s.Agent = "copilot"
			s.StartedAt = new("2026-06-16T09:00:00Z")
			s.EndedAt = new("2026-06-16T11:00:00Z")
		})
	}
	negative := money.Money{Microdollars: -1}
	maximum := money.Money{Microdollars: math.MaxInt64}
	one := money.Money{Microdollars: 1}
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "a", []UsageEvent{
		{Source: "request", Model: "model", Cost: &negative, OccurredAt: "2026-06-16T09:00:00Z", DedupKey: "a1"},
		{Source: "request", Model: "model", Cost: &maximum, OccurredAt: "2026-06-16T10:01:00Z", DedupKey: "a2"},
		{Source: "request", Model: "model", Cost: &one, OccurredAt: "2026-06-16T10:02:00Z", DedupKey: "a3"},
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "b", []UsageEvent{
		{Source: "request", Model: "model", Cost: &negative, OccurredAt: "2026-06-16T10:00:00Z", DedupKey: "b1"},
	}))
	q := dayQuery(t, "2026-06-16", "UTC")
	q.Partial = true
	q.EffectiveEnd = time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	f := AnalyticsFilter{Timezone: "UTC", IncludeSubagents: true, IncludeForks: true}
	start, end := activityReportRangeBoundsUTC(q)
	sessions, ids, err := d.activityReportSessions(t.Context(), f, start, end)
	require.NoError(t, err)
	raw := rawUsageActivityBackend{activityReportBackend{
		DB: d, lowerBound: PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14),
		upperBound: PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14), counted: &activityReportCounted{},
	}}
	want, _, err := activity.BuildReportArtifacts(t.Context(), raw, sessions, ids, q, nil, "raw activity report")
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: math.MaxInt64 - 1}, want.Report.Totals.Cost)
	got, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	assert.Equal(t, activity.ReportWithSessions(want), got)
}

func TestSumActivityUsageRowsPreservesSessionTotals(t *testing.T) {
	q := dayQuery(t, "2026-06-16", "UTC")
	reported := money.Money{Microdollars: 100}
	rows := []activity.UsageRow{
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 10, OutputTokens: 3, Cost: money.Money{Microdollars: 2}},
		{SessionID: "reported", Model: "model", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 5},
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T10:01:00Z", InputTokens: 20, OutputTokens: 7, Cost: money.Money{Microdollars: 4}},
		{SessionID: "reported", Model: "model", Timestamp: "2026-06-16T10:01:00Z", InputTokens: 6, SessionCost: &reported},
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T11:00:00Z", InputTokens: 2},
	}
	got, err := sumActivityUsageRows(rows, q)
	require.NoError(t, err)
	assert.Equal(t, []activity.UsageRow{
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 30, OutputTokens: 10, Cost: money.Money{Microdollars: 6}},
		{SessionID: "reported", Model: "model", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 5},
		{SessionID: "reported", Model: "model", Timestamp: "2026-06-16T10:01:00Z", InputTokens: 6, SessionCost: &reported},
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T11:00:00Z", InputTokens: 2},
	}, got)

	negative := money.Money{Microdollars: -1}
	rows = []activity.UsageRow{
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T10:00:00Z", InputTokens: 1},
		{SessionID: "ordinary", Model: "model", Timestamp: "2026-06-16T10:01:00Z", InputTokens: 2},
		{SessionID: "reported", Model: "model", Timestamp: "2026-06-16T10:02:00Z", SessionCost: &negative},
	}
	got, err = sumActivityUsageRows(rows, q)
	require.NoError(t, err)
	assert.Equal(t, rows, got)
}

func (b rawUsageActivityBackend) ActivityReportUsage(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	rows, pricing, _, err := b.activityReportUsage(
		ctx, ids, b.lowerBound, b.upperBound, q, false)
	return rows, pricing, err
}

func seedActivityUsageParity(t *testing.T, d *DB) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "claude-x", InputPerMTok: money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}, {
		ModelPattern: "copilot-model", InputPerMTok: money.MustParseDollars("10"),
	}}))
	session := func(id, project, agent, started string) {
		insertSession(t, d, id, project, func(s *Session) {
			s.Agent = agent
			s.StartedAt = Ptr(started)
			s.EndedAt = Ptr("2026-06-16T12:00:00Z")
		})
	}
	usage := func(input, output int) jsontext.Value {
		return jsontext.Value(`{"input_tokens":` + strconv.Itoa(input) +
			`,"output_tokens":` + strconv.Itoa(output) + `}`)
	}
	message := func(id string, ordinal int, ts, model string, tokens jsontext.Value) Message {
		return Message{
			SessionID: id, Ordinal: ordinal, Role: "assistant", Content: "x",
			Timestamp: ts, Model: model, TokenUsage: tokens,
		}
	}
	claude := func(m Message, messageID, requestID string) Message {
		m.ClaudeMessageID, m.ClaudeRequestID = messageID, requestID
		return m
	}
	session("a", "proj-a", "claude", "2026-06-16T09:00:00Z")
	session("b", "proj-b", "claude", "2026-06-16T10:01:00Z")
	session("c", "proj-b", "claude", "2026-06-15T22:00:00Z")
	session("far", "proj-c", "claude", "2026-06-16T10:30:00Z")
	session("gone", "proj-a", "claude", "2026-06-16T10:00:00Z")
	session("copilot:s", "proj-a", "copilot", "2026-06-16T10:00:00Z")
	// Active in the range, but its session start precedes the padded bounds.
	session("early", "proj-a", "claude", "2026-06-15T05:00:00Z")
	sourceShared := message("a", 2, "", "claude-x", usage(11, 7))
	sourceShared.SourceUUID = "shared-source"
	sourceCopy := message("b", 1, "2026-06-16T10:03:00Z", "claude-x", usage(12, 8))
	sourceCopy.SourceUUID = "shared-source"
	insertMessages(t, d,
		// Snapshots of one request, completed later in another session.
		claude(message("a", 0, "2026-06-16T10:00:00Z", "claude-x", usage(100, 5)), "m1", "r1"),
		claude(message("a", 1, "2026-06-16T10:00:01.5Z", "claude-x", usage(100, 50)), "m1", "r1"),
		claude(message("b", 0, "2026-06-16T10:02:00Z", "claude-x", usage(100, 60)), "m1", "r1"),
		claude(message("gone", 0, "2026-06-16T10:04:00Z", "claude-x", usage(100, 99)), "m1", "r1"),
		// A blank timestamp uses the session start; the copy shares its source.
		sourceShared, sourceCopy,
		message("early", 0, "", "claude-x", usage(1, 21)),
		// An offset timestamp, web searches, 1-hour cache writes, reasoning.
		message("a", 3, "2026-06-16T15:30:00+05:30", "claude-x", jsontext.Value(
			`{"input_tokens":5,"output_tokens":9,"reasoning_tokens":4,`+
				`"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_1h_input_tokens":8},`+
				`"server_tool_use":{"web_search_requests":2}}`)),
		message("a", 4, "2026-06-16T10:05:00Z", "claude-x", usage(1, 3_000_000)),
		message("a", 5, "2026-06-16T10:06:00Z", "<synthetic>", usage(1, 1)),
		// A peer outside the padded bounds.
		claude(message("a", 6, "2026-06-16T10:10:00Z", "claude-x", usage(1, 4)), "m2", "r2"),
		claude(message("b", 2, "2026-06-14T00:00:00Z", "claude-x", usage(1, 40)), "m2", "r2"),
		// A peer before the range but inside the padding.
		claude(message("a", 7, "2026-06-16T10:20:00Z", "claude-x", usage(1, 6)), "m3", "r3"),
		claude(message("c", 0, "2026-06-15T23:00:00Z", "claude-x", usage(1, 30)), "m3", "r3"),
		// A peer whose candidate copy lies outside the bounds.
		claude(message("a", 8, "2026-06-14T00:00:00Z", "claude-x", usage(1, 2)), "m4", "r4"),
		claude(message("far", 0, "2026-06-16T10:30:00Z", "claude-x", usage(1, 70)), "m4", "r4"),
	)
	require.NoError(t, d.SoftDeleteSession(ctx, "gone"))
	reported := money.MustParseDollars("0.03")
	direct := money.MustParseDollars("0.002")
	ordinal := 0
	require.NoError(t, d.ReplaceSessionUsageEvents(ctx, "copilot:s", []UsageEvent{
		{
			Source: "shutdown", Model: "copilot-model", InputTokens: 1_000_000,
			OccurredAt: "2026-06-16T10:05:00Z", DedupKey: "first",
		},
		{
			Source: "shutdown", Model: "copilot-model", InputTokens: 2_000_000,
			Cost: &reported, CostStatus: "exact", CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-06-16T10:10:00Z", DedupKey: "final",
		},
		{Source: "session", Model: "copilot-model", InputTokens: -5, OutputTokens: 3_500_000},
		{
			Source: "request", Model: "claude-x", OutputTokens: 9, Cost: &direct,
			CostStatus: "exact", CostSource: "provider", MessageOrdinal: &ordinal,
			OccurredAt: "2026-06-16T10:07:00Z",
		},
	}))
}

// TestActivityReportCachedUsageMatchesRawUsage holds Activity reports read
// from the usage cache to the raw archive path: same pricing, sessions read
// for invalidation, and whole report, across filters and time zones.
func TestActivityReportCachedUsageMatchesRawUsage(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedActivityUsageParity(t, d)
	query := func(preset, date, tz string, now time.Time) activity.Query {
		q, err := activity.ResolveQuery(
			activity.QueryInput{Preset: preset, Date: date, Timezone: tz}, now)
		require.NoError(t, err)
		return q
	}
	later := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	queries := map[string]activity.Query{
		"utc day":        query("day", "2026-06-16", "UTC", later),
		"new york day":   query("day", "2026-06-16", "America/New_York", later),
		"kolkata day":    query("day", "2026-06-16", "Asia/Kolkata", later),
		"utc week":       query("week", "2026-06-16", "UTC", later),
		"new york month": query("month", "2026-06-16", "America/New_York", later),
		"partial day":    query("day", "2026-06-16", "UTC", time.Date(2026, 6, 16, 10, 4, 0, 0, time.UTC)),
	}
	filters := map[string]AnalyticsFilter{
		"all":       {},
		"project a": {Project: "proj-a"},
		"project b": {Project: "proj-b"},
	}
	compare := func(t *testing.T, filter AnalyticsFilter, q activity.Query) []string {
		t.Helper()
		filter.Timezone = q.Loc.String()
		filter.IncludeSubagents, filter.IncludeForks = true, true
		rangeStart, rangeEnd := activityReportRangeBoundsUTC(q)
		lower := PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
		upper := PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
		sessions, ids, err := d.activityReportSessions(ctx, filter, rangeStart, rangeEnd)
		require.NoError(t, err)

		wantRows, wantPricing, wantRead, err := d.activityReportUsage(ctx, ids, lower, upper, q, true)
		require.NoError(t, err)
		window, err := d.prepareActivityUsageWindow(ctx, lower, upper)
		require.NoError(t, err)
		wantRead = append(wantRead, ids...)
		slices.Sort(wantRead)
		wantRead = slices.Compact(wantRead)
		gotRows, gotPricing, read, _, err := d.activityReportCachedUsage(
			ctx, window, ids, lower, upper, q, true, false)
		require.NoError(t, err)
		assert.Equal(t, wantRows, gotRows)
		assert.Equal(t, wantPricing, gotPricing)
		for _, id := range ids {
			assert.NotContains(t, read, id)
		}
		uniqueRead := slices.Clone(read)
		slices.Sort(uniqueRead)
		assert.Len(t, slices.Compact(uniqueRead), len(read))
		gotRead := append(slices.Clone(ids), read...)
		slices.Sort(gotRead)
		assert.Equal(t, wantRead, slices.Compact(gotRead))

		raw := rawUsageActivityBackend{activityReportBackend{
			DB: d, lowerBound: lower, upperBound: upper,
			counted: &activityReportCounted{},
		}}
		artifacts, _, err := activity.BuildReportArtifacts(
			ctx, raw, sessions, ids, q, nil, "raw activity report")
		require.NoError(t, err)
		got, err := d.GetActivityReport(ctx, filter, q)
		require.NoError(t, err)
		assert.Equal(t, activity.ReportWithSessions(artifacts), got)
		return gotRead
	}
	for queryName, q := range queries {
		for filterName, filter := range filters {
			t.Run(queryName+"/"+filterName, func(t *testing.T) {
				compare(t, filter, q)
			})
		}
	}

	// The fixture must exercise peers: a project-a day reads the snapshot
	// completed in project b's session, and the in-padding peer before the
	// range, but neither the deleted copy nor the out-of-bounds pairs.
	read := compare(t, AnalyticsFilter{Project: "proj-a"}, queries["utc day"])
	assert.Equal(t, []string{"a", "b", "c", "copilot:s", "early"}, read)

	// A session written after the cache filled is refilled before reading.
	insertMessages(t, d, Message{
		SessionID: "a", Ordinal: 9, Role: "assistant", Content: "x",
		Timestamp: "2026-06-16T10:40:00Z", Model: "claude-x",
		TokenUsage:      jsontext.Value(`{"input_tokens":1,"output_tokens":13}`),
		ClaudeMessageID: "m5", ClaudeRequestID: "r5",
	})
	insertMessages(t, d, Message{
		SessionID: "b", Ordinal: 3, Role: "assistant", Content: "x",
		Timestamp: "2026-06-16T10:41:00Z", Model: "claude-x",
		TokenUsage:      jsontext.Value(`{"input_tokens":1,"output_tokens":17}`),
		ClaudeMessageID: "m5", ClaudeRequestID: "r5",
	})
	compare(t, AnalyticsFilter{}, queries["utc day"])
	report, err := d.GetActivityReport(ctx, AnalyticsFilter{Project: "proj-a", Timezone: "UTC"}, queries["utc day"])
	require.NoError(t, err)
	output := make(map[string]int, len(report.BySession))
	for _, row := range report.BySession {
		output[row.SessionID] = row.OutputTokens
	}
	// a: completed snapshots 60 and 17 from b, the session-start row 7, the
	// offset row 9, the clamped row 2,000,000, and the two unpaired rows 4
	// and 6. copilot:s: the floored session total and the request row.
	assert.Equal(t, map[string]int{
		"a": 2_000_103, "copilot:s": 3_500_009, "early": 0,
	}, output)
}

// TestActivityReportWithoutCandidatesLeavesUsageCacheEmpty holds a report
// whose filter selects no sessions to the raw path's early return: it fills
// no usage facts, while a report with candidates does.
func TestActivityReportWithoutCandidatesLeavesUsageCacheEmpty(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedActivityUsageParity(t, d)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-06-16", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	databaseID, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	cache, release, err := d.usageCache.acquireGeneration(ctx, databaseID)
	require.NoError(t, err)
	defer release()
	cachedSessions := func() int {
		var n int
		require.NoError(t, cache.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM usage_cached_sessions`).Scan(&n))
		return n
	}
	require.Zero(t, cachedSessions())

	report, err := d.GetActivityReport(ctx,
		AnalyticsFilter{Project: "no-such-project", Timezone: "UTC"}, q)
	require.NoError(t, err)
	assert.Empty(t, report.BySession)
	assert.Zero(t, cachedSessions())

	_, err = d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err)
	assert.NotZero(t, cachedSessions())
}
