package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestDirectNativeThinkingSearchContext(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	dbtest.SeedNativeSearch(t, archive)
	backend := service.NewDirectBackend(archive, nil)
	result, err := backend.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "reasonneedle", Sources: []string{"thinking"}, Context: 1,
	})
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	match := result.Matches[0]
	assert.Equal(t, "thinking", match.Location)
	assert.NotContains(t, match.Snippet, fakeAWSKey)
	require.Len(t, match.ContextBefore, 1)
	assert.Equal(t, "questionneedle", match.ContextBefore[0].Content)
	require.Len(t, match.ContextAfter, 1)
	assert.Equal(t, "systemdialogue", match.ContextAfter[0].Content)
}
