package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPGFindSessionIDsByRawSuffixUsesExactFirstSuffixQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	state := &usageProbeState{}
	store := &Store{pg: newUsageProbeDB(t, state)}

	ids, err := store.FindSessionIDsByRawSuffix(
		t.Context(), "project-hash:session-uuid", 2,
	)
	require.NoError(err)
	assert.Equal([]string{
		"kimi:project-hash:session-uuid",
		"openclaw:project-hash:session-uuid",
	}, ids)

	state.mu.Lock()
	require.NotEmpty(state.queries)
	query := strings.ToLower(state.queries[len(state.queries)-1])
	state.mu.Unlock()

	assert.Contains(query, "right(id, length($1) + 1) in (':' || $1, '~' || $1)")
	assert.Contains(query, "deleted_at is null")
	assert.Contains(query, "order by (id = $1) desc")
	assert.Contains(query, "coalesce(ended_at, started_at, created_at) desc")
}
