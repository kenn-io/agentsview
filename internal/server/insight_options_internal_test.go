package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/insight"
)

func TestDefaultInsightGenerateStreamUsesServerConfig(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"snapshot-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer endpoint.Close()

	srv := &Server{}
	srv.cfg.Insights.Endpoint = endpoint.URL
	srv.cfg.Insights.Model = "snapshot-model"
	srv.cfg.Insights.AllowHTTP = true
	result, err := srv.defaultInsightGenerateStream(
		context.Background(), "claude", "prompt", nil,
	)
	require.NoError(t, err)
	require.Equal(t, "openai", result.Agent)
	require.Equal(t, "snapshot-model", result.Model)
	require.Equal(t, "ok", result.Content)
}

func TestCannedGenerationPassesSnapshotToGenerator(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"snapshot-model","choices":[{"message":{"role":"assistant","content":"{\"schema_version\":\"llm_insight.v1\",\"kind\":\"prompt_maturity_review\",\"summary\":\"ok\",\"confidence\":\"low\",\"recommendations\":[{\"title\":\"ok\",\"rationale\":\"ok\",\"actions\":[\"ok\"],\"evidence_refs\":[\"aggregate:empty\"],\"impact\":\"low\",\"effort\":\"low\"}],\"risks\":[],\"evidence_refs\":[\"aggregate:empty\"]}"}}]}`))
	}))
	defer endpoint.Close()
	mutatedEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"mutated-model","choices":[{"message":{"role":"assistant","content":"wrong"}}]}`))
	}))
	defer mutatedEndpoint.Close()

	var srv *Server
	srv = testServer(t, 30*time.Second, WithGenerateStreamFunc(
		func(ctx context.Context, agent, prompt string, onLog insight.LogFunc) (insight.Result, error) {
			srv.mu.Lock()
			srv.cfg.Insights.Endpoint = mutatedEndpoint.URL
			srv.cfg.Insights.Model = "mutated-model"
			srv.mu.Unlock()
			return srv.defaultInsightGenerateStream(ctx, agent, prompt, onLog)
		},
	))
	srv.cfg.Insights.Endpoint = endpoint.URL
	srv.cfg.Insights.Model = "snapshot-model"
	srv.cfg.Insights.AllowHTTP = true

	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/insights/generate", strings.NewReader(
			`{"type":"llm_canned","kind":"prompt_maturity_review","date_from":"2025-01-15","date_to":"2025-01-15","agent":"claude","llm_opt_in":true}`,
		),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.Host = "127.0.0.1:0"
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "snapshot-model")
	require.Contains(t, recorder.Body.String(), "ok")
	require.NotContains(t, recorder.Body.String(), "mutated-model")
}
