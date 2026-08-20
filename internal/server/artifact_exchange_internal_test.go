package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
)

func TestArtifactExchangeRouteRequiresRunner(t *testing.T) {
	srv := testServer(t, time.Second)
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/artifacts/exchange", nil,
	)

	_, pattern := srv.mux.Handler(req)

	assert.Equal(t, "/", pattern)
}

func TestArtifactExchangeAcceptsAuthenticatedLoopbackRequest(t *testing.T) {
	target := filepath.Join(t.TempDir(), "archive")
	var got ArtifactExchangeRequest
	srv := testArtifactExchangeServer(t, func(
		_ context.Context,
		req ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		got = req
		return artifact.SyncResult{
			Origin:             "node-a1b2c3",
			ExportedSessions:   2,
			PublishedArtifacts: 4,
		}, nil
	})

	rec := serveArtifactExchange(
		t, srv, "127.0.0.1:43125",
		artifactExchangeBody(t, target, true),
	)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, ArtifactExchangeRequest{
		Target: target,
		Full:   true,
	}, got)
	var result artifact.SyncResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	assert.Equal(t, "node-a1b2c3", result.Origin)
	assert.Equal(t, 2, result.ExportedSessions)
	assert.Equal(t, 4, result.PublishedArtifacts)
}

func TestArtifactExchangeRejectsNonLoopbackBeforeRunner(t *testing.T) {
	called := false
	srv := testArtifactExchangeServer(t, func(
		context.Context,
		ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		called = true
		return artifact.SyncResult{}, nil
	})

	tests := []struct {
		name       string
		remoteAddr string
		host       string
	}{
		{
			name:       "remote connection",
			remoteAddr: "192.0.2.10:4321",
			host:       "127.0.0.1:43125",
		},
		{
			name:       "non-loopback host",
			remoteAddr: "127.0.0.1:4321",
			host:       "example.test:43125",
		},
		{
			name:       "forwarded connection",
			remoteAddr: "127.0.0.1:4321",
			host:       "127.0.0.1:43125",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := artifactExchangeRequest(
				t, tt.remoteAddr, tt.host, `{"target":"archive"}`,
			)
			if tt.name == "forwarded connection" {
				req.Header.Set("Forwarded", "for=192.0.2.10")
			}
			rec := httptest.NewRecorder()

			srv.Handler().ServeHTTP(rec, req)

			assert.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
	assert.False(t, called)
}

func TestArtifactExchangeRequiresBearerAuthWhenAuthIsOptional(t *testing.T) {
	srv := testArtifactExchangeServer(t, func(
		context.Context,
		ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		return artifact.SyncResult{}, nil
	})
	req := artifactExchangeRequest(
		t, "127.0.0.1:4321", "127.0.0.1:43125",
		`{"target":"archive"}`,
	)
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestArtifactExchangeRejectsInvalidBodies(t *testing.T) {
	srv := testArtifactExchangeServer(t, func(
		context.Context,
		ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		return artifact.SyncResult{}, nil
	})

	tests := []struct {
		name   string
		body   string
		status int
	}{
		{
			name:   "unknown field",
			body:   `{"target":"archive","extra":true}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "trailing value",
			body:   `{"target":"archive"} {}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "empty body",
			body:   "",
			status: http.StatusBadRequest,
		},
		{
			name:   "empty target",
			body:   `{"target":" "}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "relative target",
			body:   `{"target":"shared-artifacts"}`,
			status: http.StatusBadRequest,
		},
		{
			name: "body over limit",
			body: `{"target":"` +
				strings.Repeat("x", artifactExchangeMaxBodyBytes) + `"}`,
			status: http.StatusRequestEntityTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveArtifactExchange(
				t, srv, "127.0.0.1:43125", tt.body,
			)
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
		})
	}
}

func TestArtifactExchangeAcceptsLocalhostHost(t *testing.T) {
	target := filepath.Join(t.TempDir(), "archive")
	called := false
	srv := testArtifactExchangeServer(t, func(
		context.Context,
		ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		called = true
		return artifact.SyncResult{}, nil
	})

	rec := serveArtifactExchange(
		t,
		srv,
		"localhost:43125",
		artifactExchangeBody(t, target, false),
	)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, called)
}

func TestArtifactExchangeRunnerErrorIsRedacted(t *testing.T) {
	privateTarget := filepath.Join(t.TempDir(), "private", "archive")
	srv := testArtifactExchangeServer(t, func(
		context.Context,
		ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		return artifact.SyncResult{}, errors.New(
			"opening " + privateTarget + ": permission denied",
		)
	})

	rec := serveArtifactExchange(
		t, srv, "127.0.0.1:43125",
		artifactExchangeBody(t, privateTarget, false),
	)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, "artifact exchange failed\n", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), privateTarget)
	assert.NotContains(t, rec.Body.String(), "permission denied")
}

func TestArtifactExchangePropagatesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := testArtifactExchangeServer(t, func(
		ctx context.Context,
		_ ArtifactExchangeRequest,
	) (artifact.SyncResult, error) {
		close(started)
		<-ctx.Done()
		return artifact.SyncResult{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	body, err := json.Marshal(ArtifactExchangeRequest{Target: t.TempDir()})
	require.NoError(t, err)
	req := artifactExchangeRequest(
		t, "127.0.0.1:4321", "127.0.0.1:43125",
		string(body),
	).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	<-started
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(
			t,
			"artifact exchange runner did not observe cancellation",
		)
	}
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

func testArtifactExchangeServer(
	t *testing.T,
	runner ArtifactExchangeRunner,
) *Server {
	t.Helper()
	srv := testServer(
		t, time.Second, WithArtifactExchangeRunner(runner),
	)
	srv.cfg.AuthToken = "test-token"
	srv.cfg.RequireAuth = false
	srv.cfg.Host = "127.0.0.1"
	srv.cfg.Port = 43125
	return srv
}

func serveArtifactExchange(
	t *testing.T,
	srv *Server,
	host string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := artifactExchangeRequest(
		t, "127.0.0.1:4321", host, body,
	)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func artifactExchangeRequest(
	t *testing.T,
	remoteAddr string,
	host string,
	body string,
) *http.Request {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/artifacts/exchange",
		bytes.NewBufferString(body),
	)
	req.RemoteAddr = remoteAddr
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func artifactExchangeBody(t *testing.T, target string, full bool) string {
	t.Helper()
	body, err := json.Marshal(ArtifactExchangeRequest{
		Target: target,
		Full:   full,
	})
	require.NoError(t, err)
	return string(body)
}
