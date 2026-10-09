package duckdb

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

func TestAnalyticsQueryChunkedSplitsAtLimit(t *testing.T) {
	ids := make([]string, readbase.AnalyticsMaxSQLVars+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("session-%04d", i)
	}

	var sizes []int
	var firstIDs []string
	err := db.QueryChunkedSize(ids, readbase.AnalyticsMaxSQLVars, func(chunk []string) error {
		sizes = append(sizes, len(chunk))
		firstIDs = append(firstIDs, chunk[0])
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int{readbase.AnalyticsMaxSQLVars, 1}, sizes)
	assert.Equal(t, []string{"session-0000", "session-0900"}, firstIDs)
}
