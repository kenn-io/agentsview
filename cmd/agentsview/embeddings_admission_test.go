package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedlocal"
)

type pausedEmbeddingsLookup struct {
	server.EmbeddingsRuntime
	lookedUp chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func (p *pausedEmbeddingsLookup) Manager(store string) server.EmbeddingsManager {
	manager := p.EmbeddingsRuntime.Manager(store)
	p.once.Do(func() {
		close(p.lookedUp)
		<-p.resume
	})
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

// Requests may cross a completed provider replacement after resolving a manager.
func TestLiveEmbeddingsSetupRequestsAcrossReplacement(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
	for _, operation := range []string{"status", "generations", "activate", "retire"} {
		t.Run(operation, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := testDataDir(t)
			writeEmbeddingsTestConfig(t, dir, "http://127.0.0.1:11434/v1")
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			stub := newEmbeddingsStubServer(t, 768)
			defer stub.Close()
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{DownloadBytes: 295_000_000}, nil
			}
			activating, installed := make(chan struct{}), make(chan struct{})
			releaseInstall := sync.OnceFunc(func() { close(installed) })
			localEmbeddingEndpoint = func(ctx context.Context, _ embedlocal.Options) (string, error) {
				close(activating)
				select {
				case <-installed:
					return stub.URL + "/v1", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { releaseInstall(); require.NoError(t, owner.Close()) }()
			initialManager := owner.Manager("")
			require.NotNil(t, initialManager)
			require.NoError(t, initialManager.StartBuild(vector.BuildRequest{}))
			initialManager.Wait()
			generations, err := initialManager.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, generations, 1)
			runtime := &pausedEmbeddingsLookup{EmbeddingsRuntime: owner, lookedUp: make(chan struct{}), resume: make(chan struct{})}
			resume := sync.OnceFunc(func() { close(runtime.resume) })
			defer resume()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, server.WithEmbeddingsRuntime(runtime)).Handler())
			if operation == "status" {
				embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
				<-activating
			}
			method, path := http.MethodGet, "/api/v1/embeddings/"+operation
			if operation == "activate" || operation == "retire" {
				method, path = http.MethodPost, "/api/v1/embeddings/generations/"+strconv.FormatInt(generations[0].ID, 10)+"/"+operation
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, strings.NewReader(`{"force":true}`))
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
			<-runtime.lookedUp
			if operation != "status" {
				embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
				<-activating
			}
			releaseInstall()
			<-owner.done
			manager := owner.Manager("")
			require.NotNil(t, manager)
			manager.Wait()
			require.Empty(t, manager.Status().LastError)
			resume()
			got := <-completed
			require.NoError(t, got.err)
			if operation == "status" {
				assert.Equal(t, http.StatusOK, got.status, string(got.body))
			} else {
				assert.Equal(t, http.StatusConflict, got.status, string(got.body))
			}
		})
	}
}
