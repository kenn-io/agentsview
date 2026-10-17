package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSearchContentThinkingRequiresSQLite(t *testing.T) {
	_, err := (&Store{}).SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "substring", Sources: []string{"thinking"}})
	var input *db.SearchInputError
	require.ErrorAs(t, err, &input)
	assert.Equal(t, "search: thinking requires SQLite substring or regex mode", input.Msg)
}
