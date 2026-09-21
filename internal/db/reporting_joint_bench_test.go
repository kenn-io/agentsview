package db

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// BenchmarkReportingJointDay measures the real SQLite snapshot, aggregation,
// canonical digest and serialization, not just encoding pre-built cells. The
// archive is fixed across iterations. It does not include process startup.
func BenchmarkReportingJointDay(b *testing.B) {
	for _, projects := range []int{4, 100} {
		for _, variant := range []struct {
			version int
			bucket  string
		}{{3, ""}, {4, "5m"}, {4, "1m"}} {
			b.Run(fmt.Sprintf("projects-%d/v%d/%s", projects, variant.version, variant.bucket), func(b *testing.B) {
				d := testDB(b)
				const sessions = 200
				start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
				for model := range 8 {
					require.NoError(b, d.UpsertModelPricing([]ModelPricing{{
						ModelPattern: fmt.Sprintf("model-%d", model), OutputPerMTok: money.MustParseDollars("10"),
					}}))
				}
				for i := range sessions {
					id := fmt.Sprintf("synthetic-%d", i)
					at := start.Add(time.Duration(i%55) * time.Minute)
					require.NoError(b, d.UpsertSession(b.Context(), Session{
						ID:      id,
						Project: fmt.Sprintf("project-%d", i%projects), Agent: fmt.Sprintf("agent-%d", i%3),
						Machine: "synthetic", MessageCount: 3, IsAutomated: i%2 == 0,
						StartedAt: Ptr(at.Format(time.RFC3339)), EndedAt: Ptr(at.Add(4 * time.Minute).Format(time.RFC3339)),
					}))
					require.NoError(b, d.UpsertProjectIdentityObservation(b.Context(), export.ProjectIdentityObservation{
						SessionID: id, Project: fmt.Sprintf("project-%d", i%projects), Machine: "synthetic",
						GitRemote: fmt.Sprintf("https://example.com/team/project-%d.git", i%projects), ObservedAt: at,
					}))
					require.NoError(b, d.InsertMessages(b.Context(), []Message{
						{SessionID: id, Ordinal: 0, Role: "user", Timestamp: at.Format(time.RFC3339)},
						{
							SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: at.Add(2 * time.Minute).Format(time.RFC3339),
							Model: fmt.Sprintf("model-%d", i%8), TokenUsage: jsontext.Value(`{"output_tokens":100}`),
						},
						{
							SessionID: id, Ordinal: 2, Role: "assistant", Timestamp: at.Add(4 * time.Minute).Format(time.RFC3339),
							Model: fmt.Sprintf("model-%d", (i+1)%8), TokenUsage: jsontext.Value(`{"output_tokens":200}`),
						},
					}))
				}
				opts := ReportingExportOptions{Date: start.Truncate(24 * time.Hour), Now: start.Add(24 * time.Hour), SchemaVersion: variant.version, Bucket: variant.bucket}
				var day export.ReportingDay
				var payload []byte
				var err error
				b.ReportAllocs()
				for b.Loop() {
					day, err = d.ExportReportingDay(b.Context(), opts)
					require.NoError(b, err)
					payload, err = export.MarshalCanonical(day)
					require.NoError(b, err)
				}
				require.Equal(b, int64(60_000), day.Hours[12].Usage.Totals.OutputTokens)
				b.ReportMetric(float64(len(payload)), "payload-bytes/op")
				if variant.version == 4 {
					require.NotEmpty(b, day.Hours[12].Joint.Cells)
					require.Len(b, day.Hours[12].Joint.Projects, projects)
					b.ReportMetric(float64(len(day.Hours[12].Joint.Cells)), "cells/op")
				}
			})
		}
	}
}

func BenchmarkReportingDigestRange(b *testing.B) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 29)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for _, variant := range []struct {
		name   string
		schema int
		bucket string
	}{
		{name: "v3", schema: export.ReportingSchemaVersion},
		{name: "v4-5m", schema: export.ReportingJointSchemaVersion, bucket: "5m"},
	} {
		b.Run(variant.name, func(b *testing.B) {
			d := testDB(b)
			seedReportingDigestBenchmarkArchive(b, d)
			opts := ReportingDigestExportOptions{
				From: start, To: end, Now: now,
				SchemaVersion: variant.schema, Bucket: variant.bucket,
			}
			legacy := func() ([]export.ReportingDigestDay, error) {
				days := make([]export.ReportingDigestDay, 0, 30)
				for date := start; !date.After(end); date = date.Add(24 * time.Hour) {
					day, err := d.ExportReportingDay(b.Context(), ReportingExportOptions{
						Date: date, Now: now, SchemaVersion: variant.schema,
						Bucket: variant.bucket,
					})
					if err != nil {
						return nil, err
					}
					days = append(days, reportingDigestDayFromDay(day))
				}
				return days, nil
			}
			rangeExport := func() ([]export.ReportingDigestDay, error) {
				return d.ExportReportingDigest(b.Context(), opts)
			}

			legacyDays, err := legacy()
			require.NoError(b, err)
			rangeDays, err := rangeExport()
			require.NoError(b, err)
			legacyBytes, err := export.MarshalCanonical(legacyDays)
			require.NoError(b, err)
			rangeBytes, err := export.MarshalCanonical(rangeDays)
			require.NoError(b, err)
			require.Equal(b, legacyBytes, rangeBytes)

			b.Run("legacy", func(b *testing.B) {
				b.ReportAllocs()
				var err error
				for b.Loop() {
					_, err = legacy()
				}
				require.NoError(b, err)
			})
			b.Run("range", func(b *testing.B) {
				b.ReportAllocs()
				var err error
				for b.Loop() {
					_, err = rangeExport()
				}
				require.NoError(b, err)
			})
		})
	}
}

func seedReportingDigestBenchmarkArchive(b *testing.B, d *DB) {
	b.Helper()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(b, d.UpsertSession(b.Context(), Session{
		ID: "benchmark-long-session", Project: "benchmark-project",
		Machine: "benchmark-machine", Agent: "benchmark-agent",
		StartedAt: Ptr(start.Format(time.RFC3339)),
		EndedAt:   Ptr("2026-07-30T23:55:00Z"), MessageCount: 2,
		UserMessageCount: 1,
	}))
	require.NoError(b, d.InsertMessages(b.Context(), []Message{
		{SessionID: "benchmark-long-session", Ordinal: 1, Role: "user", Timestamp: start.Format(time.RFC3339)},
		{SessionID: "benchmark-long-session", Ordinal: 2, Role: "assistant", Timestamp: "2026-07-30T23:55:00Z", Model: "benchmark-model"},
	}))
	usage := make([]UsageEvent, 0, 60)
	for i := range 30 {
		date := start.AddDate(0, 0, i)
		for duplicate := range 2 {
			usage = append(usage, UsageEvent{
				Source: "benchmark-usage", Model: "benchmark-model",
				InputTokens: 100 + duplicate, OutputTokens: 50 + duplicate,
				OccurredAt: date.Add(12 * time.Hour).Format(time.RFC3339),
				DedupKey:   fmt.Sprintf("day-%02d-row-%d", i, duplicate),
			})
		}
	}
	require.NoError(b, d.ReplaceSessionUsageEvents(b.Context(), "benchmark-long-session", usage))
}
