package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedlocal"
)

type pausedSetupSnapshot struct {
	server.EmbeddingsRuntime
	read, resume chan struct{}
	once         sync.Once
}

func (p *pausedSetupSnapshot) SetupStatus(ctx context.Context) (server.EmbeddingsSetupStatus, error) {
	status, err := p.EmbeddingsRuntime.SetupStatus(ctx)
	p.once.Do(func() {
		close(p.read)
		select {
		case <-p.resume:
		case <-ctx.Done():
		}
	})
	return status, err
}

func TestLiveEmbeddingsSetupStatusAcrossSuccessiveSetups(t *testing.T) {
	for _, outcome := range []string{"busy", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
			t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := testDataDir(t)
			stub := newEmbeddingsStubServer(t, 768)
			defer stub.Close()
			raw := "[vector]\nenabled=false\n[vector.embeddings]\nmodel='nomic-embed-text'\ndimension=768\n[vector.embeddings.servers.ollama]\nendpoint='" + stub.URL + "/v1'\nollama_cpu_fallback=true\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{DownloadBytes: 295_000_000}, nil
			}
			activating := make(chan struct{})
			localEmbeddingEndpoint = func(ctx context.Context, _ embedlocal.Options) (string, error) {
				if outcome == "failed" {
					return stub.URL + "/v1", nil
				}
				close(activating)
				<-ctx.Done()
				return "", ctx.Err()
			}
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			lookup := &pausedEmbeddingsLookup{EmbeddingsRuntime: owner, lookedUp: make(chan struct{}), resume: make(chan struct{})}
			resumeLookup := sync.OnceFunc(func() { close(lookup.resume) })
			defer resumeLookup()
			runtime := &pausedSetupSnapshot{EmbeddingsRuntime: lookup, read: make(chan struct{}), resume: make(chan struct{})}
			resumeSnapshot := sync.OnceFunc(func() { close(runtime.resume) })
			defer resumeSnapshot()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, server.WithEmbeddingsRuntime(runtime)).Handler())
			type result struct {
				status int
				body   []byte
				err    error
			}
			completed := make(chan result, 1)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/embeddings/status", nil)
			require.NoError(t, err)
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
			<-lookup.lookedUp
			embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"turn-on"}`), http.StatusAccepted)
			<-owner.done
			manager := owner.Manager("")
			require.NotNil(t, manager)
			manager.Wait()
			require.Empty(t, manager.Status().LastError)
			resumeLookup()
			<-runtime.read
			var held *writeOwnerLock
			defer func() {
				if held != nil {
					require.NoError(t, held.Close())
				}
			}()
			if outcome == "failed" {
				oldClose := owner.serving.Close
				owner.serving.Close = func() error {
					if err := oldClose(); err != nil {
						return err
					}
					var err error
					held, err = tryAcquireNamedLock(dir, vectorsWriteLockFile)
					return err
				}
			}
			embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
			if outcome == "failed" {
				<-owner.done
			} else {
				<-activating
			}
			resumeSnapshot()
			got := <-completed
			require.NoError(t, got.err)
			require.Equal(t, http.StatusOK, got.status, string(got.body))
			var status vector.BuildStatus
			require.NoError(t, json.Unmarshal(got.body, &status))
			require.NotNil(t, status.Setup)
			if outcome == "failed" {
				assert.False(t, status.Setup.Busy)
				assert.Equal(t, "Another process may be using search. Close it and retry.", status.Setup.Error)
			} else {
				assert.True(t, status.Setup.Busy)
			}
		})
	}
}
