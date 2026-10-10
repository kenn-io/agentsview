package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUniqueAnalyticsIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want []string
	}{
		{name: "nil", want: []string{}},
		{name: "empty", ids: []string{}, want: []string{}},
		{name: "first-seen order", ids: []string{"b", "a", "b", "", "a", ""}, want: []string{"b", "a", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UniqueAnalyticsIDs(tc.ids))
		})
	}
}

func TestQueryChunkedSizeSplitsAtSize(t *testing.T) {
	const size = 900
	ids := make([]string, size+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("session-%04d", i)
	}

	var sizes []int
	var firstIDs []string
	err := QueryChunkedSize(ids, size, func(chunk []string) error {
		sizes = append(sizes, len(chunk))
		firstIDs = append(firstIDs, chunk[0])
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{size, 1}, sizes)
	assert.Equal(t, []string{"session-0000", "session-0900"}, firstIDs)
}
