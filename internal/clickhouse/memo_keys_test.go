package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

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
