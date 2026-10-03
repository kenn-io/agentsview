package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestHTTPNativeThinkingSearch(t *testing.T) {
	te := setup(t)
	dbtest.SeedNativeSearch(t, te.db)
	w := te.get(t, "/api/v1/search/content?pattern=reasonneedle&in=thinking&context=1")
	assertStatus(t, w, http.StatusOK)
	result := decode[service.ContentSearchResult](t, w)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "thinking", result.Matches[0].Location)
	assert.NotContains(t, result.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM")
	require.Len(t, result.Matches[0].ContextBefore, 1)
	assert.Equal(t, "questionneedle", result.Matches[0].ContextBefore[0].Content)
	for _, mode := range []string{"fts", "terms", "semantic", "hybrid"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search/content?pattern=reasonneedle&in=thinking&mode="+mode, nil)
		req.Header.Set(service.SemanticSearchIntentHeader, service.SemanticSearchIntentValue)
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		assertStatus(t, w, http.StatusBadRequest)
	}
}
