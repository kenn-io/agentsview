package clickhouse

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// A kept activity usage read must give back every field of every row, in
// the order it was kept, or a reopened report silently changes its totals.
func TestKeptActivityUsageRoundTrip(t *testing.T) {
	rows := []clickSessionUsageOrderedRow{
		{scan: clickActivityReportUsageRow{
			sessionID: "session-a", source: "message", model: "model-a", providerID: "anthropic",
			ts: "2026-09-01T10:00:00.123456Z", pricingTS: "2026-09-01T10:00:00.123456Z",
			messageOrdinal: sql.NullInt64{Int64: 7, Valid: true}, agent: "claude",
			claudeMessageID: "msg-1", claudeRequestID: "req-1", sourceUUID: "uuid-1",
			inputTok: 10, outputTok: 20, cacheCr: 30, cacheCr1h: 40, cacheRd: 50,
			reasoningTok: 60, webSearchRequests: 2,
			cost: sql.NullInt64{Int64: 12345, Valid: true}, costSource: "computed",
		}},
		{scan: clickActivityReportUsageRow{
			sessionID: "session-b", source: "event", model: "model-é", providerID: "",
			ts: "", pricingTS: "2026-09-01T11:00:00Z", agent: "codex",
			usageDedupKey: "dedup-1", inputTok: -1, outputTok: 1 << 40,
		}},
		{scan: clickActivityReportUsageRow{
			sessionID: "session-a", source: "message", model: "model-a", providerID: "anthropic",
			ts: "2026-09-01T09:00:00Z", pricingTS: "2026-09-01T09:00:00Z",
			messageOrdinal: sql.NullInt64{Int64: 0, Valid: true}, agent: "claude",
			cost: sql.NullInt64{Int64: -5, Valid: true},
		}},
	}
	order := []int{2, 0, 1}
	kept := keepActivityUsage(rows, order)
	var want []clickActivityReportUsageRow
	for _, index := range order {
		want = append(want, rows[index].scan)
	}
	var got []clickActivityReportUsageRow
	for r := range kept.all() {
		got = append(got, *r)
	}
	require.Equal(t, want, got)
}
