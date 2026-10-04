package clickhouse

import (
	"database/sql"
	"slices"
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

func keptTestRow(session, source, ts string, ordinal int64, message, request string, output int) clickSessionUsageOrderedRow {
	parsed, ok := parseAnalyticsTime(ts)
	return clickSessionUsageOrderedRow{
		scan: clickActivityReportUsageRow{
			sessionID: session, source: source, model: "model-a", ts: ts, pricingTS: ts,
			messageOrdinal:  sql.NullInt64{Int64: ordinal, Valid: ordinal >= 0},
			claudeMessageID: message, claudeRequestID: request, outputTok: output,
		},
		ts: parsed, validTS: ok, ordinal: ordinal,
	}
}

func keptTestRows(k *activityUsageKept) []clickActivityReportUsageRow {
	var got []clickActivityReportUsageRow
	for r := range k.all() {
		got = append(got, *r)
	}
	return got
}

// A push's changed sessions merged into kept rows must give the rows a
// fresh read of the whole range gives, in the same order, without the
// replaced sessions' old rows: otherwise a report after a push counts old
// usage or drops new usage.
func TestMergedActivityUsageMatchesFreshRead(t *testing.T) {
	unchanged := []clickSessionUsageOrderedRow{
		keptTestRow("session-a", "message", "2026-09-01T10:00:00Z", 1, "", "", 1),
		keptTestRow("session-a", "message", "2026-09-01T10:00:02.5Z", 2, "", "", 2),
		keptTestRow("session-c", "message", "2026-09-01T10:00:01Z", 1, "", "", 3),
		keptTestRow("session-c", "event", "2026-09-01T10:00:03Z", -1, "", "", 4),
	}
	before := slices.Concat(unchanged, []clickSessionUsageOrderedRow{
		keptTestRow("session-b", "message", "2026-09-01T10:00:00Z", 1, "", "", 5),
		keptTestRow("session-gone", "message", "2026-09-01T10:00:02Z", 1, "", "", 6),
	})
	pushed := []clickSessionUsageOrderedRow{
		keptTestRow("session-b", "message", "2026-09-01T10:00:00Z", 1, "", "", 7),
		keptTestRow("session-b", "message", "2026-09-01T10:00:02.5Z", 2, "", "", 8),
		keptTestRow("session-b", "message", "2026-09-01T10:00:04Z", 3, "", "", 9),
		keptTestRow("session-new", "message", "2026-09-01T09:59:59Z", 1, "", "", 10),
	}
	prev := keepActivityUsage(before, sortActivityUsage(before))
	replaced := map[string]bool{"session-b": true, "session-gone": true, "session-new": true}
	merged := mergeActivityUsage(prev, replaced, pushed, sortActivityUsage(pushed))

	after := slices.Concat(unchanged, pushed)
	want := keepActivityUsage(after, sortActivityUsage(after))
	require.Equal(t, keptTestRows(want), keptTestRows(merged))
	require.Equal(t, want.count, merged.count)

	// A second merge starts from the first one's dictionary.
	again := []clickSessionUsageOrderedRow{keptTestRow("session-a", "message", "2026-09-01T10:00:05Z", 3, "", "", 11)}
	merged = mergeActivityUsage(merged, map[string]bool{}, again, sortActivityUsage(again))
	after = slices.Concat(after, again)
	require.Equal(t, keptTestRows(keepActivityUsage(after, sortActivityUsage(after))), keptTestRows(merged))
}

// Selecting candidates from a range's kept rows must return what the
// candidate read returns: every candidate row, plus the message rows of
// other sessions that carry a candidate row's Claude message and request
// IDs, and nothing else. Survivor selection compares the candidates with
// exactly those peers.
func TestActivityUsageSelectionKeepsClaudePeers(t *testing.T) {
	rows := []clickSessionUsageOrderedRow{
		keptTestRow("candidate", "message", "2026-09-01T10:00:00Z", 1, "m1", "r1", 1),
		keptTestRow("candidate", "message", "2026-09-01T10:00:01Z", 2, "m2", "r2", 2),
		keptTestRow("candidate", "message", "2026-09-01T10:00:02Z", 3, "", "", 3),
		keptTestRow("peer", "message", "2026-09-01T10:00:03Z", 1, "m1", "r1", 4),
		keptTestRow("peer", "message", "2026-09-01T10:00:04Z", 2, "m3", "r3", 5),
		keptTestRow("peer", "event", "2026-09-01T10:00:05Z", -1, "m2", "r2", 6),
		keptTestRow("peer", "message", "2026-09-01T10:00:06Z", 3, "m2", "", 7),
		keptTestRow("other", "message", "2026-09-01T10:00:07Z", 1, "m2", "r2", 8),
	}
	kept := keepActivityUsage(rows, sortActivityUsage(rows))
	selection, count := activityUsageSelection(kept, []string{"candidate"})
	var outputs []int
	for r := range selection {
		outputs = append(outputs, r.outputTok)
	}
	require.Equal(t, []int{1, 2, 3, 4, 8}, outputs)
	require.Equal(t, len(outputs), count)

	selection, count = activityUsageSelection(kept, []string{"candidate", "peer", "other", "absent"})
	outputs = outputs[:0]
	for r := range selection {
		outputs = append(outputs, r.outputTok)
	}
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 8}, outputs)
	require.Equal(t, len(outputs), count)
}
