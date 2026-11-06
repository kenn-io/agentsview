package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/embedlocal"
)

func TestLiveEmbeddingsSetup(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	oldOnBattery := onBattery
	onBattery = func() bool { return false }
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
		onBattery = oldOnBattery
	})
	for _, recipe := range []string{"fresh", "enabled-missing", "automatic", "configured", "nomic", "other"} {
		t.Run(recipe, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := testDataDir(t)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
			building, resume := make(chan struct{}), make(chan struct{})
			resumeBuild := sync.OnceFunc(func() { close(resume) })
			defer resumeBuild()
			var encoded, installs, inspections atomic.Int64
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string   `json:"model"`
					Input []string `json:"input"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				dimension := 768
				if request.Model == "test-model" {
					dimension = 3
				}
				if recipe == "fresh" && encoded.Load() == 0 {
					close(building)
					select {
					case <-resume:
					case <-r.Context().Done():
						return
					}
				}
				encoded.Add(int64(len(request.Input)))
				data := make([]map[string]any, len(request.Input))
				for i := range data {
					values := make([]float32, dimension)
					values[0] = 1
					data[i] = map[string]any{"index": i, "embedding": values}
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
			}))
			defer stub.Close()
			cfg, err := config.Default()
			require.NoError(t, err)
			cfg.DataDir, cfg.DBPath = dir, filepath.Join(dir, "sessions.db")
			raw := map[string]string{
				"enabled-missing": "[vector]\nenabled = true\n",
				"configured":      "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'test-model'\ndimension = 3\nmax_input_chars = 1024\nmodel_context_tokens = 0\nquery_prefix = ''\ndocument_prefix = ''\ninput_suffix = '!'\nrequest_dimensions = false\ndefault_server = 'local'\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nmax_retries = 0\n[vector.embeddings.servers.backup]\nendpoint = 'http://127.0.0.1:1/v1'\nmax_retries = 0\n",
				"nomic":           "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text:v1.5'\ndimension = 768\nmodel_context_tokens = 512\nquery_prefix = 'q: '\ndocument_prefix = 'd: '\ninput_suffix = '!'\nrequest_dimensions = true\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\nmax_batch_tokens = 512\n",
				"other":           "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'test-model'\ndimension = 3\nmodel_context_tokens = 512\ndefault_server = 'local'\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\nmax_batch_tokens = 512\n[vector.embeddings.servers.sibling]\nendpoint = '" + stub.URL + "/v1'\napi_key_env = 'EMBEDDINGS_TEST_KEY'\nmax_batch_tokens = 8192\nmax_retries = 0\n",
			}[recipe]
			if raw != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
				cfg, err = config.LoadReadOnly()
				require.NoError(t, err)
			}
			t.Setenv("EMBEDDINGS_TEST_KEY", "fixture-key")
			var installed atomic.Bool
			inspectLocalEmbeddings = func(_ context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
				inspections.Add(1)
				if recipe == "automatic" && !opts.SkipOllama {
					return embedlocal.Status{OllamaAvailable: true, DownloadBytes: 295_000_000}, nil
				}
				if installed.Load() {
					return embedlocal.Status{}, nil
				}
				return embedlocal.Status{DownloadBytes: 295_000_000}, nil
			}
			localEmbeddingEndpoint = func(ctx context.Context, opts embedlocal.Options) (string, error) {
				if recipe == "automatic" && !opts.SkipOllama {
					return stub.URL + "/v1", nil
				}
				if !installed.Load() {
					return "", embedlocal.ErrNotFound
				}
				return stub.URL + "/v1", nil
			}
			installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				installs.Add(1)
				approved, err := opts.Confirm(ctx, 295_000_000)
				if err != nil {
					return embedlocal.Installation{}, err
				}
				if !approved {
					return embedlocal.Installation{}, errors.New("download was not approved")
				}
				opts.Progress(100, 295_000_000)
				if recipe == "fresh" {
					// Simulate a file edit while the approved download is in progress.
					if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\ninclude_automated = true\n"), 0o600); err != nil {
						return embedlocal.Installation{}, err
					}
				}
				installed.Store(true)
				return embedlocal.Installation{DataDir: opts.DataDir, Executable: "fixture"}, nil
			}
			var idleFired atomic.Bool
			var idle *server.IdleTracker
			if recipe == "fresh" {
				idle = server.NewIdleTracker(time.Second, func() { idleFired.Store(true) })
			}
			owner, err := newLiveVectorServing(t.Context(), cfg, database, idle, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			replica := &embedPushReplica{Replica: pgReplica{}}
			options := append(owner.ServerOptions(), server.WithReplicas(replica), server.WithBaseContext(t.Context()))
			ts := startTestServer(t, ln, server.New(cfg, database, nil, options...).Handler())
			request := func(method, path, body string, wantStatus int) []byte {
				return embeddingRequest(t, ts.Client(), ts.URL, method, path, "application/json", strings.NewReader(body), wantStatus)
			}
			status := func() vector.BuildStatus {
				var result vector.BuildStatus
				require.NoError(t, json.Unmarshal(request("GET", "/api/v1/embeddings/status", "", 200), &result))
				return result
			}
			push := func() storage.PushResult {
				var result storage.PushResult
				require.NoError(t, json.Unmarshal(request("POST", "/api/v1/push/pg", `{"full":false,"replica":{"url":"postgres://hub.example/agentsview","machine_name":"receiver","push_vectors":true}}`, 200), &result))
				return result
			}
			if recipe == "fresh" {
				assert.True(t, push().Vectors.Skipped)
			}
			initial := status()
			require.NotNil(t, initial.Setup)
			assert.Equal(t, recipe == "enabled-missing", initial.Setup.Enabled)
			assert.Equal(t, "turn-on", initial.Setup.Action)
			assert.Zero(t, installs.Load())
			switch recipe {
			case "nomic":
				assert.False(t, initial.Setup.RebuildRequired)
			case "other":
				assert.True(t, initial.Setup.RebuildRequired)
			}
			request("POST", "/api/v1/embeddings/setup", `{"action":"turn-on"}`, 202)
			if recipe == "fresh" {
				<-building
				<-owner.done
				before := inspections.Load()
				require.True(t, status().Running)
				assert.Equal(t, before, inspections.Load(), "build progress must not probe Ollama")
				idleCtx, stopIdle := context.WithCancel(t.Context())
				go idle.Run(idleCtx)
				require.Never(t, idleFired.Load, 2*time.Second, 25*time.Millisecond)
				stopIdle()
				resumeBuild()
			}
			awaitBuild := func() vector.BuildStatus {
				var result vector.BuildStatus
				require.Eventually(t, func() bool {
					result = status()
					return result.Setup != nil && !result.Setup.Busy && !result.Running && (result.LastResult != nil || result.Setup.Error != "" || result.LastError != "")
				}, 10*time.Second, 10*time.Millisecond)
				require.Empty(t, result.Setup.Error)
				require.Empty(t, result.LastError)
				require.NotNil(t, result.LastResult)
				return result
			}
			awaitBuild()
			if recipe == "fresh" {
				assert.False(t, push().Vectors.Skipped, "browser activation must publish vectors without restart")
				require.Len(t, replica.pushed, 1)
				assert.NotEmpty(t, replica.pushed[0].Params)
			}
			if recipe == "configured" {
				assert.Empty(t, status().Setup.Action)
			}
			assert.Equal(t, (recipe == "fresh" || recipe == "enabled-missing"), installs.Load() > 0)
			if recipe == "nomic" || recipe == "other" {
				generations, err := owner.serving.manager.Generations(t.Context())
				require.NoError(t, err)
				require.Len(t, generations, 1)
				fingerprint := generations[0].Fingerprint
				assert.EqualValues(t, 1, generations[0].Embedded)
				assert.EqualValues(t, 295_000_000, status().Setup.MissingBytes)
				beforeSwitch := encoded.Load()
				request("POST", "/api/v1/embeddings/setup", `{"action":"use-built-in"}`, 202)
				awaitBuild()
				generations, err = owner.serving.manager.Generations(t.Context())
				require.NoError(t, err)
				if recipe != "other" {
					require.Len(t, generations, 1)
					assert.Equal(t, fingerprint, generations[0].Fingerprint)
					assert.Equal(t, beforeSwitch, encoded.Load())
				} else {
					require.Len(t, generations, 2)
					for _, generation := range generations {
						if generation.State == "active" {
							assert.NotEqual(t, fingerprint, generation.Fingerprint)
						}
					}
				}
			}
			if recipe == "other" {
				assert.False(t, push().Vectors.Skipped)
				require.Len(t, replica.pushed, 1)
				assert.NotEmpty(t, replica.pushed[0].Params, "switched vectors must carry their current recipe")
			}
			if recipe == "fresh" {
				var result db.ContentSearchPage
				require.NoError(t, json.Unmarshal(request("GET", "/api/v1/search/content?pattern=connections&mode=semantic&include_one_shot=true", "", 200), &result))
				require.NotEmpty(t, result.Matches)
				assert.Equal(t, "session-one", result.Matches[0].SessionID)
				dbtest.SeedSessionWithMessages(t, database, "automated", "sample", []db.Message{dbtest.UserMsg("automated", 0, "automated connections")}, func(s *db.Session) { s.IsAutomated = true })
				request("POST", "/api/v1/embeddings/build", `{"full_rebuild":true}`, 202)
				awaitBuild()
				generations, err := owner.serving.manager.Generations(t.Context())
				require.NoError(t, err)
				assert.EqualValues(t, 2, generations[0].Embedded)
				request("DELETE", "/api/v1/sessions/automated", "", 204)
				request("DELETE", "/api/v1/sessions/automated/permanent", "", 204)
				request("DELETE", "/api/v1/sessions/session-one", "", 204)
				request("DELETE", "/api/v1/sessions/session-one/permanent", "", 204)
				require.Eventually(t, func() bool {
					generations, err := owner.serving.manager.Generations(t.Context())
					return err == nil && len(generations) == 1 && generations[0].Embedded == 0
				}, 40*time.Second, 100*time.Millisecond)
			}
		})
	}
}

func TestLiveEmbeddingsSetupRejectsConcurrentBuild(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i := range data {
			values := make([]float32, 768)
			values[0] = 1
			data[i] = map[string]any{"index": i, "embedding": values}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	defer stub.Close()
	raw := "[vector]\nenabled=true\n[vector.embeddings]\nmodel='nomic-embed-text'\ndimension=768\n[vector.embeddings.servers.ollama]\nendpoint='" + stub.URL + "/v1'\nollama_cpu_fallback=true\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{DownloadBytes: 295_000_000}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	downloading, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	installLocalEmbeddings = func(ctx context.Context, _ embedlocal.InstallOptions) (embedlocal.Installation, error) {
		close(downloading)
		select {
		case <-resume:
			return embedlocal.Installation{}, errors.New("download interrupted")
		case <-ctx.Done():
			return embedlocal.Installation{}, ctx.Err()
		}
	}
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() {
		release()
		require.NoError(t, owner.Close())
	}()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	handler := server.New(cfg, database, nil, owner.ServerOptions()...).Handler()
	ts := startTestServer(t, ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/embeddings/status" {
			// Setup can finish after the build refusal and before the CLI polls.
			release()
			<-owner.done
		}
		handler.ServeHTTP(w, r)
	}))
	writeDaemonRuntimeForTest(t, dir, cfg.Host, port, "test", false)
	embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), http.StatusAccepted)
	<-downloading
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/embeddings/build", strings.NewReader(`{}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	response, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	cliDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := newEmbeddingsBuildCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})
	go func() { cliDone <- cmd.ExecuteContext(ctx) }()
	require.ErrorContains(t, <-cliDone, "being set up", "CLI must refuse the build while setup owns the index")
	release()
	<-owner.done
}

func TestLiveEmbeddingsSetupDrainsExistingSearch(t *testing.T) {
	oldInspect, oldEndpoint, oldOnBattery := inspectLocalEmbeddings, localEmbeddingEndpoint, onBattery
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, onBattery = oldInspect, oldEndpoint, oldOnBattery
	})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	onBattery = func() bool { return false }
	dir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	queryStarted, resume := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(resume) })
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(request.Input) == 1 && strings.HasPrefix(request.Input[0], "search_query:") {
			close(queryStarted)
			<-resume
		}
		data := make([]map[string]any, len(request.Input))
		for i := range data {
			values := make([]float32, 768)
			values[0] = 1
			data[i] = map[string]any{"index": i, "embedding": values}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	defer stub.Close()
	defer unblock()
	raw := "[vector]\nenabled=true\n[vector.embeddings]\nmodel='nomic-embed-text'\ndimension=768\nmax_input_chars=4000\nquery_prefix='search_query: '\ndocument_prefix='search_document: '\n[vector.embeddings.servers.ollama]\nendpoint='" + stub.URL + "/v1'\nollama_cpu_fallback=true\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return stub.URL + "/v1", nil }
	reloaded := make(chan struct{})
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, func() (config.Config, error) {
		next, err := config.LoadReadOnly()
		close(reloaded)
		return next, err
	})
	require.NoError(t, err)
	defer func() {
		unblock()
		require.NoError(t, owner.Close())
	}()
	require.NoError(t, owner.serving.manager.StartBuild(vector.BuildRequest{}))
	owner.serving.manager.Wait()
	require.Empty(t, owner.serving.manager.Status().LastError)
	var closed atomic.Bool
	oldClose := owner.serving.Close
	owner.serving.Close = func() error {
		err := oldClose()
		closed.Store(true)
		return err
	}
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	responseCh := make(chan struct {
		status int
		body   string
		err    error
	}, 1)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/search/content?pattern=connections&mode=semantic&include_one_shot=true", nil)
	require.NoError(t, err)
	req.Header.Set("X-AgentsView-Search-Intent", "semantic")
	go func() {
		var result struct {
			status int
			body   string
			err    error
		}
		res, err := ts.Client().Do(req)
		result.err = err
		if err == nil {
			body, err := io.ReadAll(res.Body)
			result.status, result.body = res.StatusCode, string(body)
			result.err = errors.Join(err, res.Body.Close())
		}
		responseCh <- result
	}()
	<-queryStarted
	require.NoError(t, owner.Setup("use-built-in"))
	<-reloaded
	assert.Never(t, closed.Load, 100*time.Millisecond, time.Millisecond, "active search must finish before old index closes")
	statusClient := &http.Client{Timeout: time.Second}
	statusReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/embeddings/status", nil)
	require.NoError(t, err)
	statusResponse, err := statusClient.Do(statusReq)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, statusResponse.StatusCode)
	require.NoError(t, statusResponse.Body.Close())
	unblock()
	res := <-responseCh
	require.NoError(t, res.err)
	assert.Equal(t, http.StatusOK, res.status, res.body)
	assert.Contains(t, res.body, "session-one")
	<-owner.done
	owner.serving.manager.Wait()
	require.Empty(t, owner.setup.Error)
}

func TestLiveEmbeddingsSetupStatusDuringWriteLockRetry(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint
	})
	t.Setenv("HOME", t.TempDir())
	dir := testDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = false\n"), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	buildTestVectorsDB(t, cfg)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	held, err := tryAcquireNamedLock(dir, vectorsWriteLockFile)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Close()) }()
	var inspections atomic.Int64
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		inspections.Add(1)
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:1/v1", nil }
	reloaded := make(chan struct{})
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, func() (config.Config, error) {
		cfg, err := config.LoadReadOnly()
		close(reloaded)
		return cfg, err
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"turn-on"}`), 202)
	<-reloaded
	client := &http.Client{Timeout: time.Second}
	before := inspections.Load()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for start := time.Now(); time.Since(start) < time.Second; {
		var status vector.BuildStatus
		require.NoError(t, json.Unmarshal(embeddingRequest(t, client, ts.URL, "GET", "/api/v1/embeddings/status", "application/json", nil, 200), &status))
		require.NotNil(t, status.Setup)
		assert.True(t, status.Setup.Busy)
		<-ticker.C
	}
	assert.Equal(t, before, inspections.Load(), "setup progress must not probe Ollama")
	<-owner.done
	var status vector.BuildStatus
	require.NoError(t, json.Unmarshal(embeddingRequest(t, client, ts.URL, "GET", "/api/v1/embeddings/status", "application/json", nil, 200), &status))
	require.NotNil(t, status.Setup)
	assert.False(t, status.Setup.Busy)
	assert.Equal(t, "turn-on", status.Setup.Action)
	assert.Equal(t, "Another process may be using search. Close it and retry.", status.Setup.Error)
	export, ok, err := owner.push.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok, "push must see the generation built by the process holding the write lock")
	defer export.Close()
	hashes, err := export.SessionDocHashes(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, hashes, "session-1")
}

func TestLiveEmbeddingsSetupFailureRetry(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
	})
	t.Setenv("HOME", t.TempDir())
	dir := testDataDir(t)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = dir
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	var calls atomic.Int64
	started := make(chan struct{})
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		if calls.Add(1) == 1 {
			return embedlocal.Installation{}, errors.New("download interrupted")
		}
		opts.Progress(100, 1000)
		close(started)
		<-ctx.Done()
		return embedlocal.Installation{}, ctx.Err()
	}
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	require.NoError(t, owner.Setup("turn-on"))
	<-owner.done
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "download interrupted", status.Error)
	require.NoError(t, owner.Setup("turn-on"))
	<-started
	require.ErrorIs(t, owner.Setup("turn-on"), vector.ErrBuildRunning)
	assert.EqualValues(t, 2, calls.Load())
	status, err = owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Busy)
	assert.EqualValues(t, 100, status.DownloadedBytes)
}

func embeddingRequest(t *testing.T, client *http.Client, base, method, path, contentType string, body io.Reader, want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, body)
	require.NoError(t, err)
	req.Header.Set("Origin", base)
	req.Header.Set("X-AgentsView-Search-Intent", "semantic")
	req.Header.Set("Content-Type", contentType)
	response, err := client.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, want, response.StatusCode, string(data))
	return data
}

func TestLiveEmbeddingsSetupProbeKeepsRuntimeAvailable(t *testing.T) {
	dir := testDataDir(t)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = dir
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	oldInspect := inspectLocalEmbeddings
	t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
	probing, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		close(probing)
		<-resume
		return embedlocal.Status{}, nil
	}
	owner := liveVectorServingFixture(t, cfg, database, vectorServing{})
	setupDone := make(chan error, 1)
	go func() {
		_, err := owner.SetupStatus(t.Context())
		setupDone <- err
	}()
	<-probing
	manager := make(chan server.EmbeddingsManager, 1)
	go func() { manager <- owner.Manager("") }()
	select {
	case got := <-manager:
		assert.Nil(t, got)
	case <-time.After(time.Second):
		assert.Fail(t, "the Ollama probe blocked access to the running server")
	}
	release()
	require.NoError(t, <-setupDone)
}

func TestLiveEmbeddingsSetupHTTPFailures(t *testing.T) {
	for _, inspectFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(inspectFailure), func(t *testing.T) {
			dir := testDataDir(t)
			cfg, err := config.Default()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			cfg.DataDir = dir
			action, want := "use-built-in", http.StatusBadRequest
			if inspectFailure {
				oldInspect := inspectLocalEmbeddings
				t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{}, errors.New("model status unavailable")
				}
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": {Local: true}}
				action, want = "turn-on", http.StatusInternalServerError
			} else {
				cfg.DeploymentEmbeddings = &config.VectorEmbeddingsServerConfig{Endpoint: "https://embeddings.example/v1"}
			}
			owner := liveVectorServingFixture(t, cfg, database, vectorServing{})
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
			if !inspectFailure {
				var status vector.BuildStatus
				data := embeddingRequest(t, ts.Client(), ts.URL, "GET", "/api/v1/embeddings/status", "", nil, http.StatusOK)
				require.NoError(t, json.Unmarshal(data, &status))
				require.NotNil(t, status.Setup)
				assert.Empty(t, status.Setup.Action)
			}
			embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"`+action+`"}`), want)
			if inspectFailure {
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }
				require.NoError(t, os.Mkdir(filepath.Join(dir, "config.toml"), 0o700))
				embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"turn-on"}`), http.StatusInternalServerError)
			}
		})
	}
}

func TestLiveEmbeddingsSetupRejectsSiblingCapBeforeDownload(t *testing.T) {
	dir := testDataDir(t)
	raw := "[vector]\nenabled = true\n[vector.embeddings]\ndefault_server = 'ollama'\nmodel = 'other-model'\ndimension = 1024\nmodel_context_tokens = 512\n[vector.embeddings.servers.ollama]\nendpoint = 'http://127.0.0.1:11434/v1'\n[vector.embeddings.servers.sibling]\nendpoint = 'http://127.0.0.1:11434/v1'\nmax_batch_tokens = 512\n"
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	require.NoError(t, cfg.Vector.Validate())
	oldInspect := inspectLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings = oldInspect
	})
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{DownloadBytes: 295_000_000}, nil
	}
	owner := &liveVectorServing{ctx: t.Context(), cfg: cfg}
	err = owner.Setup("use-built-in")
	require.ErrorIs(t, err, vector.ErrInvalidBuildRequest)
}
