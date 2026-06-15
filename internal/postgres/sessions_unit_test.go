package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGFindSessionIDsByRawSuffixUsesExactFirstSuffixQuery(t *testing.T) {
	state := &usageProbeState{}
	store := &Store{pg: newUsageProbeDB(t, state)}

	ids, err := store.FindSessionIDsByRawSuffix(
		context.Background(), "project-hash:session-uuid", 2,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"kimi:project-hash:session-uuid",
		"openclaw:project-hash:session-uuid",
	}, ids)

	state.mu.Lock()
	require.NotEmpty(t, state.queries)
	query := strings.ToLower(state.queries[len(state.queries)-1])
	state.mu.Unlock()

	assert.Contains(t, query, "right(id, length($1) + 1) = ':' || $1")
	assert.Contains(t, query, "deleted_at is null")
	assert.Contains(t, query, "order by (id = $1) desc")
	assert.Contains(t, query, "coalesce(ended_at, started_at, created_at) desc")
}
