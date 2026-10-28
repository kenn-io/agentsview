package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"

	// Docbank's API package replaces huma.NewError during init. The server
	// binary imports Docbank, so this test binary must too: otherwise the
	// validation response below would not see that side effect.
	_ "go.kenn.io/docbank/docbank"
)

func TestDocbankImportKeepsAgentsViewValidationError(t *testing.T) {
	cfg := config.Config{Host: "127.0.0.1"}
	srv := server.New(cfg, nil, nil)
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "/api/v1/insights?date_from=not-a-date", nil,
	)
	req.Host = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	req.RemoteAddr = "127.0.0.1:1234"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotContains(t, rec.Header().Get("Content-Type"), "application/problem+json")
	assert.JSONEq(t,
		`{"error":"expected string to be RFC 3339 date (query.date_from: not-a-date)"}`,
		rec.Body.String(),
	)
}
