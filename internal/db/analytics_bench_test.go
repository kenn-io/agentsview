package db

import (
	"fmt"
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
