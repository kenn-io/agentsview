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
		wantInput                   bool
	}{
		{name: "not configured", mode: "hybrid", optIn: true, reason: "not set up"},
		{name: "building", mode: "hybrid", optIn: true, cause: fmt.Errorf("search unavailable: %w", db.NewSemanticUnavailableError("index is building: 40% complete")), reason: "index is building: 40% complete"},
		{name: "empty", mode: "hybrid", optIn: true, pattern: "absent", empty: true, reason: "not set up"},
		{name: "no opt in", mode: "hybrid", wantErr: db.ErrSemanticUnavailable},
		{name: "semantic", mode: "semantic", optIn: true, wantErr: db.ErrSemanticUnavailable},
		{name: "transient", mode: "hybrid", optIn: true, cause: db.ErrSemanticTransient, wantErr: db.ErrSemanticTransient},
		{name: "invalid", mode: "hybrid", optIn: true, pattern: `"needle`, wantInput: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			if !d.HasFTS(t.Context()) {
				t.Skip("fts5 not available")
			}
			seedSearchSession(t, d, "found", "project", "needle")
			searcher := &fallbackVectorSearcher{err: tc.cause}
			if tc.cause != nil {
				d.SetVectorSearcher(searcher)
			}
			pattern := tc.pattern
			if pattern == "" {
				pattern = "needle"
			}
			result, err := service.NewDirectBackend(d, nil).SearchContent(t.Context(), service.ContentSearchRequest{Pattern: pattern, Mode: tc.mode, KeywordFallback: tc.optIn})
			if tc.wantInput {
				var inputErr *db.SearchInputError
				require.ErrorAs(t, err, &inputErr)
				return
			}
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "hybrid_keyword", result.EffectiveMode)
			assert.Equal(t, "Search by meaning is unavailable: "+tc.reason+". Returned hybrid keyword matches only. Query words must appear. If matches are empty or few, try short substring queries with key terms or synonyms.", result.Note)
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

// fallbackKeywordStore models a backend whose keyword leg accepts malformed quotes.
type fallbackKeywordStore struct {
	fakeContentStore
}

func (s *fallbackKeywordStore) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	if !f.KeywordOnly {
		return db.ContentSearchPage{}, db.ErrSemanticUnavailable
	}
	return s.fakeContentStore.SearchContent(ctx, f)
}

func TestHybridKeywordFallbackRejectsMalformedQueryBeforeRetry(t *testing.T) {
	store := &fallbackKeywordStore{}
	result, err := service.NewReadOnlyBackend(store).SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: `"needle`, Mode: "hybrid", KeywordFallback: true,
	})
	var inputErr *db.SearchInputError
	require.ErrorAs(t, err, &inputErr)
	assert.Nil(t, result)
	assert.Empty(t, store.filters, "malformed input must not reach the keyword leg")
}
