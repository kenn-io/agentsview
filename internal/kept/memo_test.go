package kept

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A memo with a size keeps no more than its byte cap: the oldest written
// slots go first, and rows larger than the cap are not kept at all.
func TestKeptMemoKeepsWithinItsByteCap(t *testing.T) {
	memo := Memo[int]{Size: func(n int) int64 { return int64(n) }, MaxBytes: 10}
	memo.Put("a", "v", []int{4})
	memo.Put("b", "v", []int{4})
	memo.Put("a", "v", []int{4})
	memo.Put("c", "v", []int{4})
	_, ok := memo.Get("b", "v")
	require.False(t, ok, "b was written longest ago")
	for _, slot := range []string{"a", "c"} {
		_, ok := memo.Get(slot, "v")
		require.True(t, ok, slot)
	}
	memo.Put("a", "v", []int{11})
	_, ok = memo.Get("a", "v")
	require.False(t, ok, "rows over the cap replace the slot's older rows with nothing")
	_, ok = memo.Get("c", "v")
	require.True(t, ok)

	memo.DeleteSlots(func(slot string) bool { return slot == "c" })
	_, ok = memo.Get("c", "v")
	require.False(t, ok)
	require.Zero(t, memo.bytes)
}

func TestKeptMemoSlotsAndVersions(t *testing.T) {
	memo := Memo[int]{Limit: 2}
	memo.Put("", "v1", []int{0})
	_, ok := memo.Get("", "v1")
	require.False(t, ok)
	memo.Put("a", "v1", []int{1})
	memo.Put("a", "v2", []int{2})
	_, ok = memo.Get("a", "v1")
	require.False(t, ok)
	rows, ok := memo.Get("a", "v2")
	require.True(t, ok)
	require.Equal(t, []int{2}, rows)
	memo.Put("b", "v1", []int{3})
	memo.Put("c", "v1", []int{4})
	_, ok = memo.Get("a", "v2")
	require.False(t, ok)
	rows, ok = memo.Get("b", "v1")
	require.True(t, ok)
	require.Equal(t, []int{3}, rows)
}
