package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryChunkedSizeSplitsAtMirrorLimit(t *testing.T) {
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
