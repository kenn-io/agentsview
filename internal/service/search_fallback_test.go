package service_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

type fallbackVectorSearcher struct {
	conceptVectorSearcher
	err error
}

func (s *fallbackVectorSearcher) PrepareSemanticSearch(context.Context, string) (db.VectorSearchPage, int, error) {
	return nil, 0, s.err
}

func TestDirectBackendHybridKeywordFallback(t *testing.T) {
	for _, tc := range []struct {
		name, mode, pattern, reason string
		cause, wantErr              error
		optIn                       bool
		empty                       bool
	}{
		{name: "not configured", mode: "hybrid", optIn: true, reason: "not set up"},
		{name: "building", mode: "hybrid", optIn: true, cause: fmt.Errorf("search unavailable: %w", db.NewSemanticUnavailableError("index is building: 40% complete")), reason: "index is building: 40% complete"},
		{name: "near not configured", mode: "hybrid", optIn: true, pattern: `"needle" AND NEAR("needle" "haystack", 2)`, reason: "not set up"},
		{name: "near building", mode: "hybrid", optIn: true, pattern: `"needle" AND NEAR("needle" "haystack", 2)`, cause: db.NewSemanticUnavailableError("index is building: 40% complete"), reason: "index is building: 40% complete"},
		{name: "empty", mode: "hybrid", optIn: true, pattern: "absent", empty: true, reason: "not set up"},
		{name: "no opt in", mode: "hybrid", wantErr: db.ErrSemanticUnavailable},
		{name: "semantic", mode: "semantic", optIn: true, wantErr: db.ErrSemanticUnavailable},
		{name: "transient", mode: "hybrid", optIn: true, cause: db.ErrSemanticTransient, wantErr: db.ErrSemanticTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			if !d.HasFTS(t.Context()) {
				t.Skip("fts5 not available")
			}
			seedSearchSession(t, d, "found", "project", "needle haystack")
			searcher := &fallbackVectorSearcher{err: tc.cause}
			if tc.cause != nil {
				d.SetVectorSearcher(searcher)
			}
			pattern := tc.pattern
			if pattern == "" {
				pattern = "needle"
			}
			result, err := service.NewDirectBackend(d, nil).SearchContent(t.Context(), service.ContentSearchRequest{Pattern: pattern, Mode: tc.mode, KeywordFallback: tc.optIn})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "hybrid_keyword", result.EffectiveMode)
			assert.Equal(t, "Search by meaning is unavailable: "+tc.reason+". Returned hybrid keyword matches only. Query words must appear. If matches are empty or few, try short substring queries with key terms or synonyms. To enable search by meaning, use Settings → Embeddings → Turn on or run `agentsview index embeddings build`.", result.Note)
			if tc.empty {
				assert.Empty(t, result.Matches)
			} else {
				require.Len(t, result.Matches, 1)
			}
		})
	}
}

func TestHTTPBackendHybridKeywordFallback(t *testing.T) {
	env := newHTTPBackendEnv(t, withHTTPStore(func(d *db.DB) db.Store { seedSearchSession(t, d, "found", "project", "needle"); return d }))
	result, err := env.Backend("", false).SearchContent(t.Context(), service.ContentSearchRequest{Pattern: "needle", Mode: "hybrid", KeywordFallback: true})
	require.NoError(t, err)
	assert.Equal(t, "hybrid_keyword", result.EffectiveMode)
	assert.NotEmpty(t, result.Note)
}
