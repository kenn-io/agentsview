package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestContentScopeExactSessionAndBranch(t *testing.T) {
	where, args := contentScope(db.ContentSearchFilter{
		SessionID: "target-session", GitBranchExact: "feature/memory",
	})

	assert.Contains(t, where, "id = ?")
	assert.Contains(t, where, "git_branch = ?")
	assert.NotContains(t, where, "relationship_type NOT IN",
		"an exact session ID must bypass root-session hiding")
	require.Equal(t, []any{"target-session", "feature/memory"}, args)
}
