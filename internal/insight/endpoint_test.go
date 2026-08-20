package insight

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if marker := os.Getenv("AGENTSVIEW_TEST_CLI_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("invoked"), 0o600)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestGenerateStreamWithOptions_OpenAIEndpoint(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "tenant=local", r.URL.RawQuery)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		require.NoError(t, json.UnmarshalRead(r.Body, &got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"served-model","choices":[{"message":{"role":"assistant","content":"answer"}}]}`))
	}))
	defer server.Close()

	result, err := GenerateStreamWithOptions(context.Background(), "claude", "exact prompt", nil, GenerateOptions{
		Endpoint: &EndpointConfig{Endpoint: server.URL + "/v1?tenant=local", Model: "configured-model", APIKey: "test-key"},
	})
	require.NoError(t, err)
	assert.Equal(t, "configured-model", got.Model)
	assert.Len(t, got.Messages, 1)
	assert.Equal(t, "user", got.Messages[0].Role)
	assert.Equal(t, "exact prompt", got.Messages[0].Content)
	assert.False(t, got.Stream)
	assert.Equal(t, "answer", result.Content)
	assert.Equal(t, "openai", result.Agent)
	assert.Equal(t, "served-model", result.Model)
}

func TestOpenAIEndpoint_BearerAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: server.URL, Model: "m", APIKey: "secret"}, "p")
	require.NoError(t, err)
}

func TestOpenAIEndpoint_AnonymousRequestOmitsBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: server.URL, Model: "m"}, "p")
	require.NoError(t, err)
}

func TestOpenAIEndpoint_HonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		close(handlerDone)
	}))
	defer func() {
		releaseHandler()
		server.CloseClientConnections()
		server.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := generateEndpoint(ctx, EndpointConfig{Endpoint: server.URL, Model: "m"}, "p")
		errs <- err
	}()
	<-started
	cancel()
	select {
	case err := <-errs:
		require.Error(t, err)
		releaseHandler()
		<-handlerDone
		server.CloseClientConnections()
	case <-time.After(time.Second):
		t.Fatal("endpoint request did not honor cancellation")
	}
}

func TestOpenAIEndpoint_RejectsUnsafeTransport(t *testing.T) {
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: "ftp://models.example/v1", Model: "m"}, "p")
	require.Error(t, err)
	_, err = generateEndpoint(context.Background(), EndpointConfig{Endpoint: "http://models.example/v1", Model: "m"}, "p")
	require.Error(t, err)
}

func TestOpenAIEndpoint_ResponseContract(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		bad  bool
	}{
		{name: "valid without model", body: `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`},
		{name: "wrong role", body: `{"choices":[{"message":{"role":"user","content":"ok"}}]}`, bad: true},
		{name: "malformed", body: `{`, bad: true},
		{name: "no choices", body: `{"choices":[]}`, bad: true},
		{name: "non string", body: `{"choices":[{"message":{"content":[]}}]}`, bad: true},
		{name: "empty", body: `{"choices":[{"message":{"content":"  "}}]}`, bad: true},
		{name: "status", body: `bad`, code: http.StatusBadGateway, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.code != 0 {
					w.WriteHeader(tt.code)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: server.URL, Model: "m"}, "p")
			if tt.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOpenAIEndpoint_RefusesRedirect(t *testing.T) {
	var called bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: redirect.URL, Model: "m"}, "prompt")
	require.Error(t, err)
	assert.False(t, called)
}

func TestOpenAIEndpoint_RedactsErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("prompt and secret-key"))
	}))
	defer server.Close()
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: server.URL, Model: "m", APIKey: "secret-key"}, "prompt")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "prompt")
	assert.NotContains(t, err.Error(), "secret-key")
}

func TestOpenAIEndpoint_RejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", int(maxEndpointResponseBytes)+1)))
	}))
	defer server.Close()
	_, err := generateEndpoint(context.Background(), EndpointConfig{Endpoint: server.URL, Model: "m"}, "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too large")
}

func TestGenerateStreamWithOptions_EndpointFailureDoesNotFallbackToCLI(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cli-invoked")
	t.Setenv("AGENTSVIEW_TEST_CLI_MARKER", marker)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	_, err := GenerateStreamWithOptions(context.Background(), "claude", "prompt", nil, GenerateOptions{
		Agents:   map[string]AgentConfig{"claude": {Binary: os.Args[0]}},
		Endpoint: &EndpointConfig{Endpoint: server.URL, Model: "m"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 503")
	assert.NoFileExists(t, marker)
}

func TestGenerateStreamWithOptions_EndpointUnsetUsesCLI(t *testing.T) {
	_, err := GenerateStreamWithOptions(context.Background(), "claude", "prompt", nil, GenerateOptions{
		Agents: map[string]AgentConfig{"claude": {Binary: "missing-cli-for-endpoint-unset-test"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start claude")
}
