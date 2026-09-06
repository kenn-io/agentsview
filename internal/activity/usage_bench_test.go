package activity

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkClaudeSnapshotSelection(b *testing.B) {
	const count = 32_000
	for _, scenario := range []struct {
		name        string
		repetitions int
	}{
		{name: "unique", repetitions: 1},
		{name: "repeated", repetitions: 16},
		{name: "non_claude"},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			rows := make([]UsageRow, count)
			for i := range rows {
				rows[i] = UsageRow{
					SessionID: "session", Timestamp: "2026-01-01T00:00:00Z",
					MessageOrdinal: int64(i), OutputTokens: i + 1,
				}
				if scenario.repetitions != 0 {
					rows[i].ClaudeMessageID = strconv.Itoa(i / scenario.repetitions)
					rows[i].ClaudeRequestID = "request"
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			var mask []bool
			for range b.N {
				mask, _, _ = ClaudeSnapshotSurvivorSelection(rows)
			}
			b.StopTimer()
			survivors := 0
			for _, selected := range mask {
				if selected {
					survivors++
				}
			}
			require.Equal(b, count/max(1, scenario.repetitions), survivors)
		})
	}
}
