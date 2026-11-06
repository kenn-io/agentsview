package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/kit/embedlocal"
)

type pausedEmbeddingsLookup struct {
	server.EmbeddingsRuntime
	lookedUp chan struct{}
	resume   chan struct{}
}

func (p *pausedEmbeddingsLookup) Manager(store string) server.EmbeddingsManager {
	manager := p.EmbeddingsRuntime.Manager(store)
	close(p.lookedUp)
	<-p.resume
	return manager
}

func TestLiveEmbeddingsSetupRejectsBuildAfterManagerLookup(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dir, "http://127.0.0.1:11434/v1")
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{DownloadBytes: 295_000_000}, nil
	}
	activating := make(chan struct{})
	localEmbeddingEndpoint = func(ctx context.Context, _ embedlocal.Options) (string, error) {
		close(activating)
		<-ctx.Done()
		return "", ctx.Err()
	}
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	runtime := &pausedEmbeddingsLookup{EmbeddingsRuntime: owner, lookedUp: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(runtime.resume) })
	defer resume()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, server.WithEmbeddingsRuntime(runtime)).Handler())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/v1/embeddings/build", strings.NewReader(`{"include_automated":true}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	type result struct {
		status int
		body   []byte
		err    error
	}
	completed := make(chan result, 1)
	go func() {
		response, err := ts.Client().Do(req)
		if err != nil {
			completed <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		completed <- result{status: response.StatusCode, body: body, err: err}
	}()
	select {
	case <-runtime.lookedUp:
	case result := <-completed:
		require.FailNow(t, "build returned before manager lookup", "%+v", result)
	}
	embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
	<-activating
	resume()
	got := <-completed
	require.NoError(t, got.err)
	assert.Equal(t, http.StatusConflict, got.status, string(got.body))
	assert.Contains(t, string(got.body), "embeddings_setup_busy")
}
