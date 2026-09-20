package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestReportingDigestMatchesPerDateExport(t *testing.T) {
	d := testDB(t)
	seedReportingSourceFixture(t, d)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	dates := []time.Time{
		time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
	}
	scopedDay, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date: dates[1], Now: now, SchemaVersion: export.ReportingJointSchemaVersion,
		Bucket: "5m",
	})
	require.NoError(t, err)
	var projectKey string
	for key := range scopedDay.Hours[23].Joint.Projects {
		projectKey = key
		break
	}
	require.NotEmpty(t, projectKey)
	variants := []struct {
		name        string
		schema      int
		bucket      string
		projectKeys []string
	}{
		{name: "v3", schema: export.ReportingSchemaVersion},
		{name: "v4-5m", schema: export.ReportingJointSchemaVersion, bucket: "5m"},
		{name: "v4-1m", schema: export.ReportingJointSchemaVersion, bucket: "1m"},
		{name: "v4-scoped", schema: export.ReportingJointSchemaVersion, bucket: "5m", projectKeys: []string{projectKey}},
	}

	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			got, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
				From: dates[0], To: dates[len(dates)-1], Now: now,
				SchemaVersion: variant.schema, Bucket: variant.bucket,
				ProjectKeys: variant.projectKeys,
			})
			require.NoError(t, err)
			require.Len(t, got, len(dates))
			for i, date := range dates {
				day, dayErr := d.ExportReportingDay(t.Context(), ReportingExportOptions{
					Date: date, Now: now, SchemaVersion: variant.schema,
					Bucket: variant.bucket, ProjectKeys: variant.projectKeys,
				})
				require.NoError(t, dayErr)
				assert.Equal(t, reportingDigestDayFromDay(day), got[i])
			}
		})
	}
}

func TestReportingDigestUsesOneReadSnapshot(t *testing.T) {
	d := testDB(t)
	from := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	snapshotCalls := 0
	loadCalls := 0
	var stats reportingSourceLoadStats
	days, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
		From: from, To: to, Now: now,
		afterSnapshot: func() {
			snapshotCalls++
			insertSession(t, d, "after-range-snapshot", "after-project", func(s *Session) {
				s.Agent = "claude"
				s.StartedAt = Ptr("2026-07-29T10:00:00Z")
				s.EndedAt = Ptr("2026-07-29T10:02:00Z")
			})
			seedMessage(t, d, "after-range-snapshot", 1, "user", "2026-07-29T10:00:00Z", "")
			seedMessage(t, d, "after-range-snapshot", 2, "assistant", "2026-07-29T10:02:00Z", "opus")
		},
		afterSourceLoad: func(got reportingSourceLoadStats) {
			loadCalls++
			stats = got
		},
	})
	require.NoError(t, err)
	require.Len(t, days, 5)
	assert.Equal(t, 1, snapshotCalls)
	assert.Equal(t, 1, loadCalls)
	assert.Zero(t, stats.ActivityHistoryRows)
	for _, day := range days {
		assert.False(t, day.HasData)
	}

	day, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), Now: now,
	})
	require.NoError(t, err)
	assert.True(t, day.HasData)
}

func TestReportingDigestSourceLoadObserver(t *testing.T) {
	d := testDB(t)
	seedReportingLongRangeFixture(t, d)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{1, 2, 30, 31} {
		t.Run(testReportingDateCountName(days), func(t *testing.T) {
			legacyCalls := 0
			legacyHistoryRows := 0
			legacyUsageRows := 0
			for date := from; !date.After(from.AddDate(0, 0, days-1)); date = date.Add(24 * time.Hour) {
				_, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
					Date: date, Now: now,
					afterSourceLoad: func(load reportingSourceLoadStats) {
						legacyCalls++
						legacyHistoryRows += load.ActivityHistoryRows
						legacyUsageRows += load.PaddedUsageRows
					},
				})
				require.NoError(t, err)
			}
			rangeCalls := 0
			var rangeStats reportingSourceLoadStats
			got, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
				From: from, To: from.AddDate(0, 0, days-1), Now: now,
				afterSourceLoad: func(load reportingSourceLoadStats) {
					rangeCalls++
					rangeStats = load
				},
			})
			require.NoError(t, err)
			require.Len(t, got, days)
			assert.Equal(t, days, legacyCalls)
			assert.Equal(t, 1, rangeCalls)
			assert.Equal(t, 1, rangeStats.ActivitySessions)
			assert.Equal(t, 2, rangeStats.ActivityHistoryRows)
			assert.GreaterOrEqual(t, legacyHistoryRows, rangeStats.ActivityHistoryRows)
			if days > 1 {
				assert.Greater(t, legacyHistoryRows, rangeStats.ActivityHistoryRows)
			}
			assert.GreaterOrEqual(t, legacyUsageRows, rangeStats.PaddedUsageRows)
		})
	}
}

func TestReportingDigestPreservesTerminalEventEligibility(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "terminal-only", "terminal-project", func(s *Session) {
		s.Agent = "agent-a"
		s.StartedAt = Ptr("2026-07-26T09:00:00Z")
		s.EndedAt = Ptr("2026-07-26T09:01:00Z")
	})
	seedMessage(t, d, "terminal-only", 1, "user", "2026-07-26T09:00:00Z", "")
	timingInsertToolResultEvent(
		t, d, "terminal-only", 1, 0, "tool-1", "completed",
		"2026-07-28T10:00:00Z", 0,
	)

	tx, err := d.getReader().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	source, err := d.loadReportingExportSource(
		t.Context(), tx,
		time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		export.ReportingSchemaVersion,
	)
	require.NoError(t, err)
	day := source.forDate(
		time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
	)
	require.Len(t, day.activitySessions, 1)
	assert.Equal(t, "terminal-only", day.activitySessions[0].SessionID)
}

func TestReportingDigestValidatesBeforeSourceLoad(t *testing.T) {
	d := testDB(t)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	snapshotCalls := 0
	_, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
		From:          now.Add(24 * time.Hour).Truncate(24 * time.Hour),
		To:            now.Add(24 * time.Hour).Truncate(24 * time.Hour),
		Now:           now,
		afterSnapshot: func() { snapshotCalls++ },
	})
	require.EqualError(t, err, "reporting date is in the future")
	assert.Zero(t, snapshotCalls)
	_, err = d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
		From:          time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
		To:            time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
		Now:           now,
		afterSnapshot: func() { snapshotCalls++ },
	})
	require.EqualError(t, err, "reporting date must be UTC midnight")
	assert.Zero(t, snapshotCalls)

	zeroHourDate := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	loadCalls := 0
	var stats reportingSourceLoadStats
	days, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
		From: zeroHourDate, To: zeroHourDate, Now: zeroHourDate,
		afterSourceLoad: func(load reportingSourceLoadStats) {
			loadCalls++
			stats = load
		},
	})
	require.NoError(t, err)
	require.Len(t, days, 1)
	assert.False(t, days[0].Complete)
	assert.Empty(t, days[0].HourDigests)
	assert.Empty(t, days[0].DayDigest)
	assert.Equal(t, 1, loadCalls)
	assert.Zero(t, stats.ActivityHistoryRows)
}

func TestReportingDigestMatchesCapturedBaseline(t *testing.T) {
	d := testDB(t)
	seedReportingSourceFixture(t, d)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	var stats reportingSourceLoadStats
	day, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
		Date: date, Now: now,
		afterSourceLoad: func(load reportingSourceLoadStats) { stats = load },
	})
	require.NoError(t, err)
	canonical, err := export.MarshalCanonical(day)
	require.NoError(t, err)
	assert.Len(t, canonical, reportingBaselineByteCount)
	assert.Equal(t, reportingBaselineSHA256, reportingBaselineDigest(canonical))
	assert.Len(t, day.Hours, 24)
	assert.Equal(t, 2, stats.ActivityHistoryRows)
	assert.Equal(t, 3, stats.PaddedUsageRows)

	digest, err := d.ExportReportingDigest(t.Context(), ReportingDigestExportOptions{
		From: date, To: date, Now: now,
	})
	require.NoError(t, err)
	require.Len(t, digest, 1)
	assert.Equal(t, reportingDigestDayFromDay(day), digest[0])
}

// Captured from the pre-range-loader v3 export of seedReportingSourceFixture
// for 2026-07-28. Keep this baseline: day and digest exports now share a loader,
// so comparing them alone cannot catch a regression in that shared code.
// After reviewing an intentional output or fixture change, run:
//
//	CGO_ENABLED=1 go test -tags fts5 ./internal/db -run '^TestReportingDigestMatchesCapturedBaseline$' -count=1
//
// Update the constants from the actual values in the failed byte-count and
// SHA-256 assertions, then rerun the test.
const (
	reportingBaselineByteCount = 98677
	reportingBaselineSHA256    = "3a6a869c18380c2fdcab1165022968c761385737a3416dc26f98efdf40694f2e"
)

func reportingBaselineDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func testReportingDateCountName(days int) string {
	return fmt.Sprintf("dates-%d", days)
}

func seedReportingSourceFixture(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.SetArchiveIdentityForTest(
		t.Context(), "reporting-source-archive",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	))
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "range-model",
		InputPerMTok:  money.MustParseDollars("2"),
		OutputPerMTok: money.MustParseDollars("4"),
	}}))
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "range-boundary", Project: "range-project", Machine: "machine-a",
		Agent: "agent-a", StartedAt: Ptr("2026-07-28T23:50:00Z"),
		EndedAt: Ptr("2026-07-29T00:10:00Z"), MessageCount: 2,
		UserMessageCount: 1,
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{SessionID: "range-boundary", Ordinal: 1, Role: "user", Timestamp: "2026-07-28T23:50:00Z"},
		{SessionID: "range-boundary", Ordinal: 2, Role: "assistant", Timestamp: "2026-07-29T00:10:00Z", Model: "range-model", TokenUsage: jsontext.Value(`{"input_tokens":10,"output_tokens":20}`)},
	}))
	reportedCost := money.MustParseDollars("0.003")
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "range-usage-only", Project: "usage-project", Machine: "machine-a",
		Agent: "agent-usage", StartedAt: Ptr("2026-07-27T08:00:00Z"),
		EndedAt: Ptr("2026-07-27T08:01:00Z"),
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "range-usage-only", []UsageEvent{{
		Source: "usage-source", Model: "usage-model", InputTokens: 7,
		OutputTokens: 3, Cost: &reportedCost, CostStatus: "exact",
		CostSource: "reported", OccurredAt: "2026-07-28T09:05:00Z",
		DedupKey: "usage-only-row",
	}}))
	require.NoError(t, d.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{
		OccurredAt: "2026-07-28T09:06:00Z", Model: "cursor-model", Kind: "usage",
		InputTokens: 5, OutputTokens: 2, Charged: money.MustParseDollars("0.001"),
		DedupKey: "cursor-row",
	}}))
	require.NoError(t, d.UpsertProjectIdentityObservation(
		t.Context(), export.ProjectIdentityObservation{
			SessionID: "range-boundary", Project: "range-project", Machine: "machine-a",
			RootPath: "/work/range-project", ObservedAt: time.Date(2026, 7, 28, 23, 50, 0, 0, time.UTC),
		},
	))
}

func seedReportingLongRangeFixture(t *testing.T, d *DB) {
	t.Helper()
	require.NoError(t, d.UpsertSession(t.Context(), Session{
		ID: "long-range", Project: "long-project", Machine: "machine-a", Agent: "agent-a",
		StartedAt: Ptr("2026-07-01T00:00:00Z"), EndedAt: Ptr("2026-07-30T23:00:00Z"),
		MessageCount: 2, UserMessageCount: 1,
	}))
	require.NoError(t, d.InsertMessages(t.Context(), []Message{
		{SessionID: "long-range", Ordinal: 1, Role: "user", Timestamp: "2026-07-01T00:00:00Z"},
		{SessionID: "long-range", Ordinal: 2, Role: "assistant", Timestamp: "2026-07-30T23:00:00Z", Model: "long-model"},
	}))
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "long-range", []UsageEvent{
		{Source: "long-source", Model: "long-model", OccurredAt: "2026-07-01T12:00:00Z", DedupKey: "first"},
		{Source: "long-source", Model: "long-model", OccurredAt: "2026-07-30T12:00:00Z", DedupKey: "second"},
	}))
}
