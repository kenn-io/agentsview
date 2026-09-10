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
		for _, version := range []int{3, 4} {
			b.Run(fmt.Sprintf("projects-%d/v%d", projects, version), func(b *testing.B) {
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
					require.NoError(b, d.UpsertSession(Session{ID: id,
						Project: fmt.Sprintf("project-%d", i%projects), Agent: fmt.Sprintf("agent-%d", i%3),
						Machine: "synthetic", MessageCount: 3, IsAutomated: i%2 == 0,
						StartedAt: Ptr(at.Format(time.RFC3339)), EndedAt: Ptr(at.Add(4 * time.Minute).Format(time.RFC3339)),
					}))
					require.NoError(b, d.InsertMessages([]Message{
						{SessionID: id, Ordinal: 0, Role: "user", Timestamp: at.Format(time.RFC3339)},
						{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: at.Add(2 * time.Minute).Format(time.RFC3339),
							Model: fmt.Sprintf("model-%d", i%8), TokenUsage: jsontext.Value(`{"output_tokens":100}`)},
						{SessionID: id, Ordinal: 2, Role: "assistant", Timestamp: at.Add(4 * time.Minute).Format(time.RFC3339),
							Model: fmt.Sprintf("model-%d", (i+1)%8), TokenUsage: jsontext.Value(`{"output_tokens":200}`)},
					}))
				}
				opts := ReportingExportOptions{Date: start.Truncate(24 * time.Hour), Now: start.Add(24 * time.Hour), SchemaVersion: version}
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
				if version == 4 {
					require.NotEmpty(b, day.Hours[12].Joint.Cells)
					b.ReportMetric(float64(len(day.Hours[12].Joint.Cells)), "cells/op")
				}
			})
		}
	}
}
