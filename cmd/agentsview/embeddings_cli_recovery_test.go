package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestLiveEmbeddingsSetupCLIRecovery(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		prebuilt, wantActivated bool
	}{
		{"first build", false, true},
		{"existing index", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := testDataDir(t)
			writeTestConfig(t, dir, "[vector]\nenabled=true\n")
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
			stub := newEmbeddingsStubServer(t, 768)
			defer stub.Close()
			oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
			t.Cleanup(func() {
				inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
			})
			var installed atomic.Bool
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				if installed.Load() {
					return embedlocal.Status{}, nil
				}
				return embedlocal.Status{DownloadBytes: 295000000}, nil
			}
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				if installed.Load() {
					return stub.URL + "/v1", nil
				}
				return "", embedlocal.ErrNotFound
			}
			installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				ok, err := opts.Confirm(ctx, 295000000)
				if err != nil {
					return embedlocal.Installation{}, err
				}
				if !ok {
					return embedlocal.Installation{}, errors.New("download was not approved")
				}
				installed.Store(true)
				return embedlocal.Installation{Executable: "kennembed", DataDir: opts.DataDir}, nil
			}
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			if tc.prebuilt {
				installed.Store(true)
				manager, _ := owner.ManagerSetup("")
				_, err := manager.StartBuild(vector.BuildRequest{})
				require.NoError(t, err)
				manager.Wait()
				require.True(t, manager.Status().LastResult.Activated)
				installed.Store(false)
			}
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
			writeDaemonRuntimeForTest(t, dir, cfg.Host, cfg.Port, "test", false)
			embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"turn-on"}`), http.StatusAccepted)
			<-owner.done
			status, err := owner.SetupStatus(t.Context())
			require.NoError(t, err)
			require.Equal(t, "download was not approved", status.Error)
			command := newEmbeddingsBuildCommand()
			command.SetContext(t.Context())
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetIn(strings.NewReader("y\n"))
			command.SetArgs([]string{"--yes"})
			require.NoError(t, command.Execute())
			require.Eventually(t, func() bool {
				status, err := owner.SetupStatus(t.Context())
				return err == nil && status.Error == "" && status.Action == ""
			}, time.Second, 10*time.Millisecond, "a completed recovery must reach setup status")
			var build vector.BuildStatus
			raw := embeddingRequest(t, ts.Client(), ts.URL, http.MethodGet, "/api/v1/embeddings/status", "", nil, http.StatusOK)
			require.NoError(t, json.Unmarshal(raw, &build))
			require.NotNil(t, build.Setup)
			require.NotNil(t, build.LastResult)
			require.Empty(t, build.LastError)
			assert.Equal(t, tc.wantActivated, build.LastResult.Activated)
			assert.Empty(t, build.Setup.Error, "a successful CLI recovery must clear the stale setup failure")
			assert.Empty(t, build.Setup.Action, "a built index must stop offering Turn on")
		})
	}
}

// TestDaemonBuildReportsReplacedBuild covers a browser setup replacing the
// manager after the CLI's build failed: the replacement's successful build
// must not become the CLI's outcome, whether it finishes before or after
// the CLI's first status poll.
func TestDaemonBuildReportsReplacedBuild(t *testing.T) {
	for _, tc := range []struct{ beforeFirstPoll, followRunning bool }{{true, false}, {false, true}} {
		beforeFirstPoll := tc.beforeFirstPoll
		t.Run(fmt.Sprintf("before-first-poll=%t/follow-running=%t", beforeFirstPoll, tc.followRunning), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := testDataDir(t)
			oldInspect, oldEndpoint, oldInterval := inspectLocalEmbeddings, localEmbeddingEndpoint, embeddingsPollInterval
			t.Cleanup(func() {
				inspectLocalEmbeddings, localEmbeddingEndpoint, embeddingsPollInterval = oldInspect, oldEndpoint, oldInterval
			})
			embeddingsPollInterval = time.Millisecond
			fail := make(chan struct{})
			releaseFailure := sync.OnceFunc(func() { close(fail) })
			var calls atomic.Int32
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					<-fail
					http.Error(w, "original encoder failure", http.StatusBadRequest)
					return
				}
				var request struct {
					Input []string `json:"input"`
				}
				assert.NoError(t, json.UnmarshalRead(r.Body, &request))
				writeEmbeddingsResponse(t, w, len(request.Input), 768)
			}))
			defer stub.Close()
			defer releaseFailure()
			writeTestConfig(t, dir, "[vector]\nenabled=true\n[vector.embeddings]\nmodel='nomic-embed-text'\ndimension=768\n[vector.embeddings.servers.ollama]\nendpoint='"+stub.URL+"/v1'\nollama_cpu_fallback=true\nmax_retries=0\n")
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return stub.URL + "/v1", nil }
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			oldManager, _ := owner.ManagerSetup("")
			firstPoll, replaced := make(chan struct{}), make(chan struct{})
			releaseReplacement := sync.OnceFunc(func() { close(replaced) })
			defer releaseReplacement()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			handler := server.New(cfg, database, nil, owner.ServerOptions()...).Handler()
			var polls atomic.Int32
			ts := startTestServer(t, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/embeddings/status" {
					if polls.Add(1) == 1 {
						if beforeFirstPoll {
							close(firstPoll)
							<-replaced
						}
						handler.ServeHTTP(w, r)
						if !beforeFirstPoll {
							close(firstPoll)
						}
						return
					}
					<-replaced
				}
				handler.ServeHTTP(w, r)
			}))
			writeDaemonRuntimeForTest(t, dir, cfg.Host, cfg.Port, "test", false)
			var out bytes.Buffer
			command := newEmbeddingsBuildCommand()
			command.SetContext(t.Context())
			command.SetOut(&out)
			command.SetErr(io.Discard)
			command.SetArgs([]string{"--include-automated=true"})
			if tc.followRunning {
				_, err := oldManager.StartBuild(vector.BuildRequest{IncludeAutomated: true})
				require.NoError(t, err)
			}
			result := make(chan error, 1)
			go func() { result <- command.Execute() }()
			select {
			case <-firstPoll:
			case err := <-result:
				require.FailNow(t, "CLI returned before its first poll", "%v", err)
			}
			releaseFailure()
			oldManager.Wait()
			require.Contains(t, oldManager.Status().LastError, "returned 400")
			embeddingRequest(t, ts.Client(), ts.URL, http.MethodPost, "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
			<-owner.done
			replacement, _ := owner.ManagerSetup("")
			require.NotNil(t, replacement)
			replacement.Wait()
			status := replacement.Status()
			require.Empty(t, status.LastError)
			require.NotNil(t, status.LastResult)
			releaseReplacement()
			require.ErrorContains(t, <-result, "replaced this build",
				"the CLI must not report the replacement's success as its original build outcome")
			assert.NotContains(t, out.String(), "Generation activated")
		})
	}
}
