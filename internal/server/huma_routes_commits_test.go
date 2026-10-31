package server

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestCommitLookupHTTPContract(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	require.NoError(t, d.StoreCommitLinks(t.Context(), []db.CommitLink{{SessionID: "unknown", CommitSHA: "abcdef0123456789012345678901234567890123", Resolved: true, Source: "trailer", Subject: "Subject"}}))
	s := newRoutedTestServerWithStore(t, d)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/commits/sessions", strings.NewReader(`{"shas":["ABCDEF0","0000000"]}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, request)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response service.CommitSessionsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Commits["ABCDEF0"], 1)
	assert.NotNil(t, response.Commits["0000000"])
	link := response.Commits["ABCDEF0"][0]
	assert.True(t, link.Gone)
	assert.Nil(t, link.Session)
	assert.Nil(t, link.MessageOrdinal)
	assert.Nil(t, link.Turn)
	bad := httptest.NewRequest(http.MethodPost, "/api/v1/commits/sessions", strings.NewReader(`{"shas":["bad"]}`))
	bad.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, bad)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
