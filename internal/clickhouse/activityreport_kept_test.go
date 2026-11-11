package clickhouse

import (
	"database/sql"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"
	"weak"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A kept activity usage read must give back every field of every row, in
// report order, or a reopened report silently changes its totals.
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
	for i := range rows {
		rows[i].ts, rows[i].validTS = readbase.ParseAnalyticsTime(rows[i].scan.ts)
		rows[i].ordinal = db.UsageRowMessageOrdinal(rows[i].scan.messageOrdinal)
	}
	kept := keepTestUsage(rows)
	// Session A's rows in time order; session B's row has no timestamp, so
	// it sorts by session after them.
	want := []clickActivityReportUsageRow{rows[2].scan, rows[0].scan, rows[1].scan}
	var got []clickActivityReportUsageRow
	for r := range kept.all() {
		got = append(got, *r)
	}
	require.Equal(t, want, got)
}

func keptTestRow(session, source, ts string, ordinal int64, message, request string, output int) clickSessionUsageOrderedRow {
	parsed, ok := readbase.ParseAnalyticsTime(ts)
	return clickSessionUsageOrderedRow{
		scan: clickActivityReportUsageRow{
			sessionID: session, source: source, model: "model-a", ts: ts, pricingTS: ts,
			messageOrdinal:  sql.NullInt64{Int64: ordinal, Valid: ordinal >= 0},
			claudeMessageID: message, claudeRequestID: request, outputTok: output,
		},
		ts: parsed, validTS: ok, ordinal: ordinal,
	}
}

// keepTestUsage keeps rows the way a read keeps the rows it scans.
func keepTestUsage(rows []clickSessionUsageOrderedRow) *activityUsageKept {
	sorter := newKeptUsageSorter()
	for i := range rows {
		sorter.add(&rows[i])
	}
	return sorter.finish()
}

func keptTestRows(k *activityUsageKept) []clickActivityReportUsageRow {
	var got []clickActivityReportUsageRow
	for r := range k.all() {
		got = append(got, *r)
	}
	return got
}

// A push's changed sessions patched into a kept range must leave each
// session with the rows a fresh read of the whole range gives it, in the
// same order, and drop the sessions that are gone: otherwise a report after
// a push counts old usage or drops new usage.
func TestPatchedActivityUsageMatchesFreshRead(t *testing.T) {
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
	var r activityUsageRange
	r.reset(nil, keepTestUsage(before))
	r.patch(nil, []string{"session-b", "session-gone", "session-new"}, keepTestUsage(pushed))

	sessionRows := func(r *activityUsageRange) map[string][]int {
		out := map[string][]int{}
		for _, session := range r.sessions {
			if session.rows == nil {
				continue
			}
			for row := range session.rows.all() {
				out[session.id] = append(out[session.id], row.outputTok)
			}
		}
		return out
	}
	want := map[string][]int{
		"session-a": {1, 2}, "session-b": {7, 8, 9}, "session-c": {3, 4}, "session-new": {10},
	}
	require.Equal(t, want, sessionRows(&r))
	require.Equal(t, 8, r.rows)

	var fresh activityUsageRange
	fresh.reset(nil, keepTestUsage(slices.Concat(unchanged, pushed)))
	require.Equal(t, sessionRows(&fresh), sessionRows(&r))
}

// A push that replaces a session must let its previous rows be freed while
// the sessions it left unchanged stay kept. The range's byte estimate counts
// only the rows it holds, so replaced rows that stayed alive beside the kept
// ones would let the range outgrow its memo's byte cap uncounted.
func TestPatchedActivityUsageFreesReplacedRows(t *testing.T) {
	var before []clickSessionUsageOrderedRow
	for _, session := range []string{"session-a", "session-b", "session-c"} {
		for i := range 10 {
			before = append(before, keptTestRow(session, "message",
				time.Date(2026, 9, 1, 10, 0, i, 0, time.UTC).Format(time.RFC3339), int64(i+1), "msg", "req", i))
		}
	}
	var r activityUsageRange
	r.reset(nil, keepTestUsage(before))
	data, arena := func() (weak.Pointer[byte], weak.Pointer[byte]) {
		replaced := r.sessions[r.number["session-b"]].rows
		return weak.Make(unsafe.SliceData(replaced.data)), weak.Make(unsafe.StringData(replaced.arena))
	}()
	r.patch(nil, []string{"session-b"}, keepTestUsage([]clickSessionUsageOrderedRow{
		keptTestRow("session-b", "message", "2026-09-01T10:00:20Z", 1, "msg", "req", 20),
	}))

	runtime.GC()
	assert.Nil(t, data.Value(), "the replaced session's data is still alive")
	assert.Nil(t, arena.Value(), "the replaced session's arena is still alive")
	for _, session := range []string{"session-a", "session-c"} {
		assert.Len(t, keptTestRows(r.sessions[r.number[session]].rows), 10, session)
	}
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
	kept := keepTestUsage(rows)
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

// Slices of a range must cover it exactly once, so a sliced read neither
// drops nor repeats a row: each slice ends a microsecond before the next
// begins, and cuts that would leave a slice empty are skipped.
func TestActivityUsageSlicesCoverRange(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, s)
		require.NoError(t, err)
		return v
	}
	const lower, upper = "2026-04-15T04:00:00Z", "2026-11-10T05:00:00Z"
	cuts := []time.Time{
		at("2026-05-01T00:00:00Z"), at("2026-06-01T00:00:00.5Z"), at("2026-06-01T00:00:00.5Z"),
		at("2026-07-01T00:00:00Z"), at("2026-08-01T00:00:00Z"), at("2027-01-01T00:00:00Z"),
		at("2027-02-01T00:00:00Z"),
	}
	got, err := activityUsageSlices(lower, upper, 8, cuts)
	require.NoError(t, err)
	require.Equal(t, []activityUsageSlice{
		{lower, "2026-04-30T23:59:59.999999Z"},
		{"2026-05-01T00:00:00.000000Z", "2026-06-01T00:00:00.499999Z"},
		{"2026-06-01T00:00:00.500000Z", "2026-06-30T23:59:59.999999Z"},
		{"2026-07-01T00:00:00.000000Z", "2026-07-31T23:59:59.999999Z"},
		{"2026-08-01T00:00:00.000000Z", upper},
	}, got)

	got, err = activityUsageSlices(lower, upper, 2, cuts)
	require.NoError(t, err)
	require.Equal(t, []activityUsageSlice{
		{lower, "2026-06-30T23:59:59.999999Z"},
		{"2026-07-01T00:00:00.000000Z", upper},
	}, got, "two slices cut at the median")

	got, err = activityUsageSlices(lower, upper, 1, cuts)
	require.NoError(t, err)
	require.Equal(t, []activityUsageSlice{{lower, upper}}, got)
}

// Consecutive slices joined together must keep what one read of all their
// rows keeps, dictionary references and timestamps included.
func TestJoinedActivityUsageSlicesMatchOneRead(t *testing.T) {
	early := []clickSessionUsageOrderedRow{
		keptTestRow("session-b", "message", "2026-09-01T10:00:00Z", 1, "m1", "r1", 1),
		keptTestRow("session-a", "message", "2026-09-01T10:00:00Z", 2, "", "", 2),
	}
	late := []clickSessionUsageOrderedRow{
		keptTestRow("session-c", "event", "2026-09-01T10:00:01Z", -1, "", "", 3),
		keptTestRow("session-a", "message", "2026-09-01T10:00:02Z", 3, "m2", "r2", 4),
	}
	slice := func(rows []clickSessionUsageOrderedRow) *keptUsageSorter {
		sorter := newKeptUsageSorter()
		for i := range rows {
			sorter.add(&rows[i])
		}
		sorter.sort()
		return sorter
	}
	joined := joinActivityUsageSlices([]*keptUsageSorter{slice(early), slice(late)})
	want := keepTestUsage(slices.Concat(early, late))
	require.Equal(t, keptTestRows(want), keptTestRows(joined))
	require.Equal(t, want.count, joined.count)
	micros := func(k *activityUsageKept) []int64 {
		var out []int64
		for r := range k.rows() {
			out = append(out, r.us)
		}
		return out
	}
	require.Equal(t, micros(want), micros(joined), "timestamps chain across the slice boundary")
	selection, _ := activityUsageSelection(joined, []string{"session-a"})
	var outputs []int
	for r := range selection {
		outputs = append(outputs, r.outputTok)
	}
	require.Equal(t, []int{2, 4}, outputs, "joined sessions keep their dictionary entries")
}
