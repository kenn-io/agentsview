package service_test

import (
	"context"
	"encoding/json/v2"
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

func (s *readinessVectorSearcher) PrepareSemanticSearch(
	context.Context, string,
) (db.VectorSearchPage, int, error) {
	return func(context.Context, int) ([]db.VectorHit, bool, error) {
		return nil, true, nil
	}, 0, nil
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

func TestMemoryStatusSemanticCountPresence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status db.SemanticReadiness
		want   string
	}{
		{
			name:   "unmeasured counts",
			status: db.SemanticReadiness{State: "ready", Reason: "counts_unavailable"},
			want:   `{"status":"ready","reason":"counts_unavailable"}`,
		},
		{
			name:   "measured counts",
			status: db.SemanticReadiness{State: "partial", Generation: "gen-1", Embedded: 9, Missing: 2, Reason: "index_incomplete"},
			want:   `{"status":"partial","reason":"index_incomplete","generation":"gen-1","embedded":9,"missing":2}`,
		},
		{
			name:   "measured zero counts",
			status: db.SemanticReadiness{State: "ready", Generation: "gen-1"},
			want:   `{"status":"ready","generation":"gen-1","embedded":0,"missing":0}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			database.SetVectorSearcher(&readinessVectorSearcher{status: tc.status})
			backend := service.NewDirectBackend(database, nil)
			status, err := service.GetMemoryStatus(t.Context(), backend)
			require.NoError(t, err)
			encoded, err := json.Marshal(status.Semantic)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(encoded))
		})
	}
}
