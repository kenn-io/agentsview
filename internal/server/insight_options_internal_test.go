package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/insight"
)

func TestDefaultInsightGenerateStreamUsesContextSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"snapshot-model","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	options := insight.GenerateOptions{
		Endpoint: &insight.EndpointConfig{
			Endpoint:  server.URL,
			Model:     "snapshot-model",
			AllowHTTP: true,
		},
	}
	ctx := context.WithValue(
		context.Background(), insightGenerationOptionsContextKey{}, options,
	)

	result, err := (&Server{}).defaultInsightGenerateStream(
		ctx, "claude", "prompt", nil,
	)
	require.NoError(t, err)
	require.Equal(t, "openai", result.Agent)
	require.Equal(t, "snapshot-model", result.Model)
	require.Equal(t, "ok", result.Content)
}
