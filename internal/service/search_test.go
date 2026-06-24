package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// seedSearchSession creates a non-one-shot session with a user message
// whose content is FTS-indexed, so Search can find it by a term.
func seedSearchSession(t *testing.T, d *db.DB, id, project, content string) {
	t.Helper()
	dbtest.SeedSession(t, d, id, project, func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	msgs := []db.Message{
		dbtest.UserMsg(id, 0, content),
		dbtest.AsstMsg(id, 1, "understood"),
	}
	require.NoError(t, d.InsertMessages(msgs))
}

func TestDirectBackend_Search_Roundtrip(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	if !d.HasFTS() {
		t.Skip("FTS not available in this build")
	}
	seedSearchSession(t, d, "s1", "proj-a", "the quick brown fox jumped")
	seedSearchSession(t, d, "s2", "proj-b", "lazy dogs sleep")
	be := service.NewDirectBackend(d, nil)

	res, err := be.Search(context.Background(), service.SearchRequest{
		Query: "fox",
		Limit: 10,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, res.Results, 1)
	assert.Equal(t, "s1", res.Results[0].SessionID)
	assert.Equal(t, "proj-a", res.Results[0].Project)

	// Project filter restricts results.
	none, err := be.Search(context.Background(), service.SearchRequest{
		Query:   "fox",
		Project: "proj-b",
		Limit:   10,
	})
	require.NoError(t, err)
	assert.Empty(t, none.Results)
}

func TestDirectBackend_Search_EmptyQuery(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	be := service.NewDirectBackend(d, nil)

	_, err := be.Search(context.Background(), service.SearchRequest{Query: "   "})
	require.Error(t, err)
	var inputErr *db.SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"empty query should be a SearchInputError, got %T", err)
}

// A query containing FTS operator characters (a hyphen, a colon) must be
// treated as literal terms rather than 500ing the store, because
// db.PrepareFTSQuery quotes each term.
func TestDirectBackend_Search_PunctuationIsLiteral(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	if !d.HasFTS() {
		t.Skip("FTS not available in this build")
	}
	seedSearchSession(t, d, "s1", "proj", "deploying agentsview-mcp today")
	be := service.NewDirectBackend(d, nil)

	res, err := be.Search(context.Background(), service.SearchRequest{
		Query: "agentsview-mcp",
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, res.Results, 1)
	assert.Equal(t, "s1", res.Results[0].SessionID)
}

func TestHTTPBackend_Search_Roundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	d := env.DB
	if !d.HasFTS() {
		t.Skip("FTS not available in this build")
	}
	seedSearchSession(t, d, "s1", "proj-a", "the quick brown fox jumped")
	svc := env.Backend("", false)

	res, err := svc.Search(context.Background(), service.SearchRequest{
		Query: "fox",
		Limit: 10,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, res.Results, 1)
	assert.Equal(t, "s1", res.Results[0].SessionID)
}

// A daemon without an FTS index responds 501; the HTTP backend maps that
// to the shared ErrSearchUnavailable sentinel.
func TestHTTPBackend_Search_Unavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		}))
	t.Cleanup(srv.Close)
	svc := service.NewHTTPBackend(srv.URL, "", true)

	_, err := svc.Search(context.Background(), service.SearchRequest{Query: "fox"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrSearchUnavailable),
		"501 should map to ErrSearchUnavailable, got %v", err)
}
