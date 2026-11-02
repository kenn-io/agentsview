package db

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkGetAnalyticsToolsYearRange(b *testing.B) {
	d := testDB(b)
	start := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	for day := range 1096 {
		id := fmt.Sprintf("day-%d", day)
		ts := start.AddDate(0, 0, day)
		require.NoError(b, d.UpsertSession(b.Context(), Session{ID: id, Project: "bench", Machine: "local", Agent: "claude", StartedAt: new(ts.Format(time.RFC3339)), MessageCount: 30}))
		msgs := make([]Message, 30)
		for n := range msgs {
			msgs[n] = asstMsgAt(id, n, "read", ts.Add(time.Duration(n)*time.Second).Format(time.RFC3339))
			msgs[n].ToolCalls = []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}}
		}
		require.NoError(b, d.InsertMessages(b.Context(), msgs))
	}
	f := AnalyticsFilter{From: "2025-01-01", To: "2025-12-31", Timezone: "UTC"}
	b.ResetTimer()
	for b.Loop() {
		resp, err := d.GetAnalyticsTools(b.Context(), f)
		require.NoError(b, err)
		require.Equal(b, 10950, resp.TotalCalls)
	}
}

// Cold dashboard iterations repeat the prompt scan paid by every request before
// memoization. The two axes vary session count and total user-message bytes.
func BenchmarkSignalsFrustration(b *testing.B) {
	for _, size := range []struct {
		name     string
		sessions int
		bytes    int
	}{
		{"sessions/1000", 1000, 1000 * 10 * 500},
		{"sessions/4000", 4000, 4000 * 10 * 500},
		{"bytes/2MB", 500, 2 << 20},
		{"bytes/16MB", 500, 16 << 20},
	} {
		b.Run(size.name, func(b *testing.B) {
			d := testDB(b)
			ctx := b.Context()
			text := "this is broken " + strings.Repeat("x", size.bytes/(size.sessions*10)-len("this is broken "))
			for i := range size.sessions {
				id := fmt.Sprintf("session-%d", i)
				msgs := make([]Message, 10)
				for j := range msgs {
					msgs[j] = Message{SessionID: id, Ordinal: j, Role: "user", Content: text}
				}
				result, err := d.WriteSessionBatchContext(ctx, []SessionBatchWrite{{
					Session:  Session{ID: id, Project: "bench", Machine: "local", Agent: "claude", MessageCount: 10},
					Messages: msgs,
					Signals:  SessionSignalUpdate{ToolRetryCount: 1, QualitySignals: QualitySignals{Version: CurrentQualitySignalVersion}},
				}})
				require.NoError(b, err)
				require.Empty(b, result.Errors)
			}
			for _, mode := range []string{"cold", "warm", "tool_retries"} {
				b.Run(mode, func(b *testing.B) {
					_, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
					require.NoError(b, err)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if mode == "cold" || mode == "tool_retries" {
							d.frustrationMemo.swap(func() {})
						}
						if mode == "tool_retries" {
							resp, err := d.GetAnalyticsSignalSessions(ctx, AnalyticsFilter{}, mode, 10)
							require.NoError(b, err)
							require.Len(b, resp.Sessions, 10)
						} else {
							resp, err := d.GetAnalyticsSignals(ctx, AnalyticsFilter{})
							require.NoError(b, err)
							require.Equal(b, size.sessions*10, resp.QualityHealth.Totals.FrustrationMarkerCount)
						}
					}
				})
			}
		})
	}
}
