package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Arguments that render the same when joined by spaces must still name
// different listings, or one filter's sessions would answer another's.
func TestAnalyticsSessionMemoKeyKeepsArgumentBoundaries(t *testing.T) {
	a := analyticsSessionMemoKey("s.project IN (?, ?)", []any{"a b", "c"})
	b := analyticsSessionMemoKey("s.project IN (?, ?)", []any{"a", "b c"})
	require.NotEqual(t, a, b)
	require.NotEqual(t, a, analyticsSessionMemoKey("s.project IN (?, ?)", []any{"a b", 1}))
}

func TestUsageRowMemoKeyKeepsCustomModelBoundaries(t *testing.T) {
	f := db.UsageFilter{Timezone: "UTC", From: "2026-01-12", To: "2026-01-12"}
	a, err := usageRowMemoKeyFor("daily", f, "", "digest", [][2]string{{"a b", "c"}})
	require.NoError(t, err)
	b, err := usageRowMemoKeyFor("daily", f, "", "digest", [][2]string{{"a", "b c"}})
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	c, err := usageRowMemoKeyFor("daily", db.UsageFilter{Timezone: "UTC", From: "2026-01-12", To: "2026-01-13"}, "", "digest", nil)
	require.NoError(t, err)
	d, err := usageRowMemoKeyFor("daily", f, "", "digest", nil)
	require.NoError(t, err)
	require.NotEqual(t, c, d)
}

// A termination filter that compares session times with the current time
// selects other sessions as time passes without any write, so its rows
// must not be answered from the memo. Other filters are kept.
func TestUsageRowMemoSkipsTimeDependentTermination(t *testing.T) {
	for _, c := range []struct {
		termination string
		kept        bool
	}{
		{"", true},
		{"clean", true},
		{"awaiting_user", true},
		{"active", false},
		{"stale", false},
		{"unclean", false},
		{"clean,active", false},
	} {
		f := db.UsageFilter{Timezone: "UTC", From: "2026-01-12", To: "2026-01-12", Termination: c.termination}
		slot, err := usageRowMemoKeyFor("daily", f, "", "digest", nil)
		require.NoError(t, err)
		var memo usageRowMemo[int]
		memo.put(slot, "parts", []int{1})
		_, ok := memo.get(slot, "parts")
		require.Equal(t, c.kept, ok, c.termination)
	}
}

// A memo with a size keeps no more than its byte cap: the oldest written
// slots go first, and rows larger than the cap are not kept at all.
func TestUsageRowMemoKeepsWithinItsByteCap(t *testing.T) {
	memo := usageRowMemo[int]{size: func(n int) int64 { return int64(n) }, maxBytes: 10}
	memo.put("a", "v", []int{4})
	memo.put("b", "v", []int{4})
	memo.put("a", "v", []int{4})
	memo.put("c", "v", []int{4})
	_, ok := memo.get("b", "v")
	require.False(t, ok, "b was written longest ago")
	for _, slot := range []string{"a", "c"} {
		_, ok := memo.get(slot, "v")
		require.True(t, ok, slot)
	}
	memo.put("a", "v", []int{11})
	_, ok = memo.get("a", "v")
	require.False(t, ok, "rows over the cap replace the slot's older rows with nothing")
	_, ok = memo.get("c", "v")
	require.True(t, ok)

	memo.deleteSlots(func(slot string) bool { return slot == "c" })
	_, ok = memo.get("c", "v")
	require.False(t, ok)
	require.Zero(t, memo.bytes)
}
