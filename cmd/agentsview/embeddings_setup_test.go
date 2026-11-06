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

func TestLiveEmbeddingsSetup(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
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
			var encoded, installs atomic.Int64
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
				"configured":      "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'test-model'\ndimension = 3\nmax_input_chars = 1024\nmodel_context_tokens = 0\nquery_prefix = ''\ndocument_prefix = ''\ninput_suffix = '!'\nrequest_dimensions = false\ndefault_server = 'local'\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nmax_retries = 0\n[vector.embeddings.servers.backup]\nendpoint = '" + stub.URL + "/v1'\n",
				"nomic":           "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text:v1.5'\ndimension = 768\nquery_prefix = 'q: '\ndocument_prefix = 'd: '\ninput_suffix = '!'\nrequest_dimensions = true\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\n",
				"other":           "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'test-model'\ndimension = 3\ndefault_server = 'local'\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\n[vector.embeddings.servers.sibling]\nendpoint = '" + stub.URL + "/v1'\napi_key_env = 'EMBEDDINGS_TEST_KEY'\nmax_retries = 0\n",
			}[recipe]
			if raw != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
				cfg, err = config.LoadReadOnly()
				require.NoError(t, err)
			}
			t.Setenv("EMBEDDINGS_TEST_KEY", "fixture-key")
			expected := cfg.Vector
			expected.Enabled = true
			var installed atomic.Bool
			inspectLocalEmbeddings = func(_ context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
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
				require.NoError(t, err)
				require.True(t, approved)
				opts.Progress(100, 295_000_000)
				if recipe == "fresh" {
					// Simulate a file edit while the approved download is in progress.
					require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("port = 12345\n[vector]\ninclude_automated = true\n[vector.embeddings]\nquery_prefix = 'edited: '\n"), 0o600))
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
			ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
			request := func(method, path, body string, wantStatus int) []byte {
				return embeddingRequest(t, ts.Client(), ts.URL, method, path, "application/json", strings.NewReader(body), wantStatus)
			}
			status := func() vector.BuildStatus {
				var result vector.BuildStatus
				require.NoError(t, json.Unmarshal(request("GET", "/api/v1/embeddings/status", "", 200), &result))
				return result
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
				idleCtx, stopIdle := context.WithCancel(t.Context())
				go idle.Run(idleCtx)
				assert.Never(t, idleFired.Load, 2*time.Second, 25*time.Millisecond)
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
			if recipe == "configured" {
				assert.Empty(t, status().Setup.Action)
				request("POST", "/api/v1/embeddings/setup", `{"action":"use-built-in"}`, 400)
			}
			export, ok, exportErr := owner.push.BeginExport(t.Context(), nil)
			require.NoError(t, exportErr)
			require.True(t, ok)
			require.NoError(t, export.Close())
			assert.Equal(t, (recipe == "fresh" || recipe == "enabled-missing"), installs.Load() > 0)
			generations, err := owner.serving.manager.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, generations, 1)
			fingerprint := generations[0].Fingerprint
			assert.EqualValues(t, 1, generations[0].Embedded)
			if recipe == "nomic" || recipe == "other" || recipe == "automatic" {
				assert.EqualValues(t, 295_000_000, status().Setup.MissingBytes)
				beforeSwitch := encoded.Load()
				request("POST", "/api/v1/embeddings/setup", `{"action":"use-built-in"}`, 202)
				awaitBuild()
				generations, err = owner.serving.manager.Generations(t.Context())
				require.NoError(t, err)
				if recipe != "other" {
					require.Len(t, generations, 1)
					saved, err := os.ReadFile(filepath.Join(dir, "config.toml"))
					require.NoError(t, err)
					assert.NotContains(t, string(saved), "timeout")
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
			var result db.ContentSearchPage
			require.NoError(t, json.Unmarshal(request("GET", "/api/v1/search/content?pattern=connections&mode=semantic&include_one_shot=true", "", 200), &result))
			require.NotEmpty(t, result.Matches)
			assert.Equal(t, "session-one", result.Matches[0].SessionID)
			if recipe == "fresh" {
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
			persisted, err := config.LoadReadOnly()
			require.NoError(t, err)
			assert.True(t, persisted.Vector.Enabled)
			assert.Equal(t, persisted.Vector, owner.cfg.Vector)
			if recipe == "other" {
				assert.Equal(t, expected.Embeddings.Servers["sibling"], persisted.Vector.Embeddings.Servers["sibling"])
			}
			if recipe == "fresh" {
				stored, err := os.ReadFile(filepath.Join(dir, "config.toml"))
				require.NoError(t, err)
				assert.NotContains(t, string(stored), "model =")
				assert.Equal(t, "edited: ", persisted.Vector.Embeddings.QueryPrefix)
				assert.Equal(t, 12345, persisted.Port)
			}
			if recipe == "automatic" {
				stored, err := os.ReadFile(filepath.Join(dir, "config.toml"))
				require.NoError(t, err)
				assert.NotContains(t, string(stored), "model =")
				assert.True(t, persisted.Vector.Embeddings.Servers["automatic"].Local)
			}
			if recipe == "configured" {
				assert.Equal(t, expected, persisted.Vector)
			}
		})
	}
}

func TestLiveEmbeddingsSetupStatusDuringWriteLockRetry(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	oldTimeout := vectorsWriteLockRetryTimeout
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint
		vectorsWriteLockRetryTimeout = oldTimeout
	})
	t.Setenv("HOME", t.TempDir())
	dir := testDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = true\n"), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	held, err := tryAcquireNamedLock(dir, vectorsWriteLockFile)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Close()) }()
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:1/v1", nil }
	reloaded := make(chan struct{})
	vectorsWriteLockRetryTimeout = time.Millisecond
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, func() (config.Config, error) {
		cfg, err := config.LoadReadOnly()
		close(reloaded)
		return cfg, err
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	vectorsWriteLockRetryTimeout = oldTimeout
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{"action":"use-built-in"}`), 202)
	<-reloaded
	client := &http.Client{Timeout: time.Second}
	for start := time.Now(); time.Since(start) < time.Second; {
		var status vector.BuildStatus
		require.NoError(t, json.Unmarshal(embeddingRequest(t, client, ts.URL, "GET", "/api/v1/embeddings/status", "application/json", nil, 200), &status))
		require.NotNil(t, status.Setup)
		assert.True(t, status.Setup.Busy)
	}
	<-owner.done
	var status vector.BuildStatus
	require.NoError(t, json.Unmarshal(embeddingRequest(t, client, ts.URL, "GET", "/api/v1/embeddings/status", "application/json", nil, 200), &status))
	require.NotNil(t, status.Setup)
	assert.False(t, status.Setup.Busy)
	assert.Equal(t, "use-built-in", status.Setup.Action)
	assert.Contains(t, status.Setup.Error, "vectors.write.lock")
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
	require.NoError(t, owner.Close())
}

func TestLiveEmbeddingsSetupDeploymentOwned(t *testing.T) {
	owner := &liveVectorServing{cfg: config.Config{DeploymentEmbeddings: &config.VectorEmbeddingsServerConfig{Endpoint: "http://127.0.0.1:1/v1"}}}
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.Action)
	require.ErrorContains(t, owner.Setup("turn-on"), "unavailable")
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
