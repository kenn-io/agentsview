package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

type readinessVectorSearcher struct {
	status db.SemanticReadiness
	calls  int
}

func (s *readinessVectorSearcher) SemanticSearch(
	context.Context, string, int,
) ([]db.VectorHit, error) {
	return nil, nil
}

func (s *readinessVectorSearcher) ResolveMessageUnits(
	context.Context, []db.MessageRef,
) ([]db.UnitRef, error) {
	return nil, nil
}

func (s *readinessVectorSearcher) SemanticReadiness(
	context.Context,
) (db.SemanticReadiness, error) {
	s.calls++
	return s.status, nil
}

func TestDirectMemoryStatusAndSearchCoverageShareSemantics(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	searcher := &readinessVectorSearcher{status: db.SemanticReadiness{
		State: "partial", Generation: "generation-a", Embedded: 9, Missing: 2,
		Reason: "index_incomplete",
	}}
	database.SetVectorSearcher(searcher)
	backend := service.NewDirectBackend(database, nil)

	status, err := service.GetMemoryStatus(t.Context(), backend)
	require.NoError(t, err)
	assert.Equal(t, service.MemoryPartial, status.Status)
	assert.Equal(t, "sqlite", status.Archive.Backend)
	assert.False(t, status.Archive.ReadOnly)
	assert.Equal(t, service.MemoryReady, status.Lexical.Status)
	assert.Equal(t, service.MemoryPartial, status.Semantic.Status)
	assert.EqualValues(t, 9, status.Semantic.Embedded)
	assert.EqualValues(t, 2, status.Semantic.Missing)
	assert.Equal(t, service.MemoryUnknown, status.Sources.Status)
	assert.Equal(t, "source_telemetry_unavailable", status.Sources.Reason)

	search, err := backend.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "absent", Mode: "substring",
	})
	require.NoError(t, err)
	assert.Equal(t, status.Coverage(), search.Coverage)
	_, err = backend.SearchContent(t.Context(), service.ContentSearchRequest{
		Pattern: "still absent", Mode: "substring",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, searcher.calls, "search responses should reuse the recent status snapshot")
}

func TestMemoryStatusUnsupportedServiceIsExplicit(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	legacy := struct{ service.SessionService }{
		SessionService: service.NewDirectBackend(database, nil),
	}

	status, err := service.GetMemoryStatus(t.Context(), legacy)
	require.NoError(t, err)
	assert.Equal(t, service.MemoryUnknown, status.Status)
	assert.Equal(t, "unsupported", status.Lexical.Reason)
	assert.Equal(t, "unsupported", status.Semantic.Reason)
}
