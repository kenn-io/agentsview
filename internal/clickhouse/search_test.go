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

func TestSearchContentThinkingRequiresSQLite(t *testing.T) {
	_, err := (&Store{}).SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"thinking"}})
	var input *db.SearchInputError
	require.ErrorAs(t, err, &input)
	assert.Equal(t, "search: thinking requires SQLite substring or regex mode", input.Msg)
}
