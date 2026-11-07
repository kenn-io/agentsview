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
		wantSetup                   bool
	}{
		{name: "not configured", mode: "hybrid", optIn: true, reason: "not set up", wantSetup: true},
		{name: "building", mode: "hybrid", optIn: true, cause: fmt.Errorf("search unavailable: %w", db.NewSemanticUnavailableError("index is building: 40% complete")), reason: "index is building: 40% complete"},
		{name: "stale", mode: "hybrid", optIn: true, cause: db.NewSemanticUnavailableError("index is stale (embedding config changed): run 'agentsview index embeddings build --full-rebuild'"), reason: "index is stale (embedding config changed): run 'agentsview index embeddings build --full-rebuild'"},
		{name: "mirror mismatch", mode: "hybrid", optIn: true, cause: db.NewSemanticUnavailableError("vector index was built by an incompatible version: run `agentsview index embeddings build`"), reason: "vector index was built by an incompatible version: run `agentsview index embeddings build`"},
		{name: "empty", mode: "hybrid", optIn: true, pattern: "absent", empty: true, reason: "not set up", wantSetup: true},
		{name: "no opt in", mode: "hybrid", wantErr: db.ErrSemanticUnavailable},
		{name: "semantic", mode: "semantic", optIn: true, wantErr: db.ErrSemanticUnavailable},
		{name: "transient", mode: "hybrid", optIn: true, cause: db.ErrSemanticTransient, wantErr: db.ErrSemanticTransient},
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
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "hybrid_keyword", result.EffectiveMode)
			wantNote := "Search by meaning is unavailable: " + tc.reason + ". Returned hybrid keyword matches only. Query words must appear. If matches are empty or few, try short substring queries with key terms or synonyms."
			if tc.wantSetup {
				wantNote += " To enable search by meaning, set `enabled = true` in the `[vector]` section of `~/.agentsview/config.toml`, restart the daemon, then run `agentsview index embeddings build`. If `[vector]` is already enabled, run the build now."
			}
			assert.Equal(t, wantNote, result.Note)
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
