package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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

func (p *pausedEmbeddingsLookup) ManagerSetup(store string) (server.EmbeddingsManager, server.EmbeddingsSetupStatus) {
	manager, setup := p.EmbeddingsRuntime.ManagerSetup(store)
	p.once.Do(func() {
		close(p.lookedUp)
		<-p.resume
	})
	return manager, setup
}

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

// Requests may cross a completed provider replacement after resolving a manager.
func TestLiveEmbeddingsSetupRequestsAcrossReplacement(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
	for _, tc := range []struct {
		operation, outcome      string
		duringSetup, resumeBusy bool
	}{
		{"status", "busy", false, true},
		{"status", "error", false, false},
		{"status", "ready", false, false},
		{"build", "", false, false},
		{"generations", "", false, false},
		{"activate", "", false, false},
		{"retire", "", false, false},
		{"build", "", true, false},
		{"generations", "", true, false},
		{"activate", "", true, false},
		{"retire", "", true, false},
		{"build", "", false, true},
	} {
		operation, duringSetup := tc.operation, tc.duringSetup
		t.Run(fmt.Sprintf("%s/%s/during-setup=%t/resume-busy=%t", operation, tc.outcome, duringSetup, tc.resumeBusy), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := testDataDir(t)
			stub := newEmbeddingsStubServer(t, 768)
			defer stub.Close()
			raw := "[vector]\nenabled=true\n[vector.embeddings]\nmodel='nomic-embed-text'\ndimension=768\n[vector.embeddings.servers.ollama]\nendpoint='" + stub.URL + "/v1'\nollama_cpu_fallback=true\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
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
			initialManager, _ := owner.ManagerSetup("")
			require.NotNil(t, initialManager)
			_, err = initialManager.StartBuild(vector.BuildRequest{})
			require.NoError(t, err)
			initialManager.Wait()
			generations, err := initialManager.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, generations, 1)
			lookedUp, resumed := make(chan struct{}), make(chan struct{})
			var runtime server.EmbeddingsRuntime = &pausedEmbeddingsLookup{EmbeddingsRuntime: owner, lookedUp: lookedUp, resume: resumed}
			if operation == "status" {
				runtime = &pausedSetupSnapshot{EmbeddingsRuntime: owner, read: lookedUp, resume: resumed}
			}
			resume := sync.OnceFunc(func() { close(resumed) })
			defer resume()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, server.WithEmbeddingsRuntime(runtime)).Handler())
			if duringSetup {
				embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
				<-activating
			}
			method, path := http.MethodGet, "/api/v1/embeddings/"+operation
			if operation == "build" {
				method = http.MethodPost
			}
			if operation == "activate" || operation == "retire" {
				method, path = http.MethodPost, "/api/v1/embeddings/generations/"+strconv.FormatInt(generations[0].ID, 10)+"/"+operation
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, method, ts.URL+path, strings.NewReader(`{}`))
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
			<-lookedUp
			var held *writeOwnerLock
			defer func() {
				if held != nil {
					require.NoError(t, held.Close())
				}
			}()
			if tc.outcome == "error" {
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
			if !duringSetup {
				embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
				<-activating
			}
			if !tc.resumeBusy && !duringSetup {
				releaseInstall()
				<-owner.done
				if tc.outcome != "error" {
					manager, _ := owner.ManagerSetup("")
					require.NotNil(t, manager)
					manager.Wait()
					require.Empty(t, manager.Status().LastError)
				}
			}
			resume()
			got := <-completed
			require.NoError(t, got.err)
			if operation == "status" {
				require.Equal(t, http.StatusOK, got.status, string(got.body))
				var status vector.BuildStatus
				require.NoError(t, json.Unmarshal(got.body, &status))
				require.NotNil(t, status.Setup)
				assert.Equal(t, tc.outcome == "busy", status.Setup.Busy)
				if tc.outcome == "error" {
					assert.Equal(t, "Another process may be using search. Close it and retry.", status.Setup.Error)
				} else {
					assert.Empty(t, status.Setup.Error)
					if tc.outcome == "ready" {
						assert.Equal(t, "nomic-embed-text", status.Model)
					}
				}
			} else {
				assert.Equal(t, http.StatusConflict, got.status, string(got.body))
				assert.Contains(t, string(got.body), "embeddings_setup_busy")
			}
		})
	}
}
