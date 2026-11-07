package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	oldOnBattery := onBattery
	onBattery = func() bool { return false }
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
		onBattery = oldOnBattery
	})
	for _, recipe := range []string{"fresh", "enabled-missing", "automatic", "configured", "nomic", "changed-provider"} {
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
				writeEmbeddingsResponse(t, w, len(request.Input), dimension)
			}))
			defer stub.Close()
			cfg, err := config.Default()
			require.NoError(t, err)
			if recipe == "nomic" {
				shared, err := embedlocal.DefaultDir()
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(shared), 0o700))
				require.NoError(t, os.WriteFile(shared, []byte("unusable built-in installation"), 0o600))
			}
			cfg.DataDir, cfg.DBPath = dir, filepath.Join(dir, "sessions.db")
			raw := map[string]string{
				"enabled-missing":  "[vector]\nenabled = true\n",
				"changed-provider": "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text'\ndimension = 768\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\n",
				"configured":       "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'test-model'\ndimension = 3\nmax_input_chars = 1024\nmodel_context_tokens = 0\nquery_prefix = ''\ndocument_prefix = ''\ninput_suffix = '!'\nrequest_dimensions = false\ndefault_server = 'local'\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nmax_retries = 0\n[vector.embeddings.servers.backup]\nendpoint = 'http://127.0.0.1:1/v1'\nmax_retries = 0\n",
				"nomic":            "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text:v1.5'\ndimension = 768\nmodel_context_tokens = 512\nquery_prefix = 'q: '\ndocument_prefix = 'd: '\ninput_suffix = '!'\nrequest_dimensions = true\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\nmax_batch_tokens = 512\n",
			}[recipe]
			if raw != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600))
				cfg, err = config.LoadReadOnly()
				require.NoError(t, err)
			}
			var installed atomic.Bool
			inspectLocalEmbeddings = func(ctx context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
				if recipe == "nomic" {
					return embedlocal.Inspect(ctx, opts)
				}
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
			var reloads atomic.Int64
			owner, err := newLiveVectorServing(t.Context(), cfg, database, idle, func() (config.Config, error) { reloads.Add(1); return config.LoadReadOnly() })
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			options := append(owner.ServerOptions(), server.WithBaseContext(t.Context()))
			ts := startTestServer(t, ln, server.New(cfg, database, nil, options...).Handler())
			request := func(method, path, body string, wantStatus int) []byte {
				return embeddingRequest(t, ts.Client(), ts.URL, method, path, "application/json", strings.NewReader(body), wantStatus)
			}
			status := func() vector.BuildStatus {
				var result vector.BuildStatus
				require.NoError(t, json.Unmarshal(request("GET", "/api/v1/embeddings/status", "", 200), &result))
				return result
			}
			originalManager := owner.Manager("")
			initial := status()
			require.NotNil(t, initial.Setup)
			assert.Equal(t, recipe == "enabled-missing", initial.Setup.Enabled)
			assert.Equal(t, "turn-on", initial.Setup.Action)
			provider := "built-in"
			switch recipe {
			case "automatic", "nomic":
				provider = "ollama"
			case "configured", "changed-provider":
				provider = "configured"
			}
			assert.Equal(t, provider, initial.Setup.Provider)
			assert.Zero(t, installs.Load())
			// The row discloses a download only for the built-in provider.
			var shown int64
			if initial.Setup.Provider == "built-in" {
				shown = initial.Setup.MissingBytes
			}
			switch recipe {
			case "changed-provider":
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = false\n"), 0o600))
			case "enabled-missing":
				// A running manager keeps its recipe even if the file changes.
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = true\n[vector.embeddings]\nmodel = 'other-model'\ndimension = 3\n[vector.embeddings.servers.local]\nendpoint = 'http://127.0.0.1:1/v1'\n"), 0o600))
			}
			request("POST", "/api/v1/embeddings/setup", fmt.Sprintf(`{"approved_bytes":%d}`, shown), 202)
			if recipe == "changed-provider" {
				<-owner.done
				retry := status()
				assert.Equal(t, "built-in", retry.Setup.Provider)
				assert.Contains(t, retry.Setup.Error, "download was not approved")
				return
			}
			if recipe == "fresh" {
				<-building
				<-owner.done
				require.True(t, status().Running)
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
			if recipe == "enabled-missing" {
				assert.Same(t, originalManager, owner.Manager(""))
				assert.Zero(t, reloads.Load())
			} else {
				assert.EqualValues(t, 1, reloads.Load())
			}
			if recipe == "configured" {
				assert.Empty(t, status().Setup.Action)
			}
			assert.Equal(t, (recipe == "fresh" || recipe == "enabled-missing" || recipe == "changed-provider"), installs.Load() > 0)
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
				request("DELETE", "/api/v1/sessions/session-one", "", 204)
				require.Eventually(t, func() bool {
					generations, err := owner.serving.manager.Generations(t.Context())
					return err == nil && len(generations) == 1 && generations[0].Embedded == 1
				}, 40*time.Second, 100*time.Millisecond)
			}
		})
	}
}

func TestLiveEmbeddingsSetupStatusAndExportDuringWriteLockRetry(t *testing.T) {
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
	releaseHeld := sync.OnceFunc(func() { require.NoError(t, held.Close()) })
	defer releaseHeld()
	var inspections atomic.Int64
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		inspections.Add(1)
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:1/v1", nil }
	reloaded := make(chan struct{})
	notifyReload := sync.OnceFunc(func() { close(reloaded) })
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, func() (config.Config, error) {
		cfg, err := config.LoadReadOnly()
		notifyReload()
		return cfg, err
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{}`), 202)
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

	movedPath := filepath.Join(t.TempDir(), "moved.db")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, "[vector]\nenabled = true\ndb_path = %q\n", movedPath), 0o600))
	releaseHeld()
	require.NoError(t, owner.Setup(0))
	<-owner.done
	setup, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	require.Empty(t, setup.Error)
	require.Empty(t, setup.Action)
	hashes, err = export.SessionDocHashes(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, hashes, "session-1", "an export in progress keeps its original snapshot")
	next, ok, err := owner.push.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	if ok {
		defer next.Close()
	}
	assert.False(t, ok, "the reloaded database path has no active generation")
}

func TestLiveEmbeddingsSetupFailureRecovery(t *testing.T) {
	for _, recovery := range []string{"installed files", "retry"} {
		t.Run(recovery, func(t *testing.T) {
			oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
			t.Cleanup(func() {
				inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
			})
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			dir := testDataDir(t)
			if recovery == "installed files" {
				writeTestConfig(t, dir, "[vector]\nenabled=true\n")
			}
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			var installed atomic.Bool
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				if installed.Load() {
					return embedlocal.Status{}, nil
				}
				return embedlocal.Status{DownloadBytes: 1000}, nil
			}
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
			require.NoError(t, owner.Setup(0))
			<-owner.done
			status, err := owner.SetupStatus(t.Context())
			require.NoError(t, err)
			require.Equal(t, "download interrupted", status.Error)
			if recovery == "installed files" {
				installed.Store(true)
				status, err = owner.SetupStatus(t.Context())
				require.NoError(t, err)
				assert.Empty(t, status.Action)
				assert.Empty(t, status.Error)
			} else {
				require.NoError(t, owner.Setup(0))
				<-started
				require.ErrorIs(t, owner.Setup(0), vector.ErrBuildRunning)
				assert.EqualValues(t, 2, calls.Load())
				status, err = owner.SetupStatus(t.Context())
				require.NoError(t, err)
				assert.True(t, status.Busy)
				assert.EqualValues(t, 100, status.DownloadedBytes)
			}
		})
	}
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

func TestLiveEmbeddingsSetupFailures(t *testing.T) {
	for _, failure := range []string{"configured provider", "invalid endpoint", "invalid TOML", "model inspection", "config read"} {
		t.Run(failure, func(t *testing.T) {
			dir := testDataDir(t)
			cfg, err := config.Default()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			cfg.DataDir = dir
			inspectionError := errors.New("model status unavailable")
			if failure == "invalid endpoint" {
				writeTestConfig(t, dir, "[vector]\nenabled=false\n[vector.embeddings]\nmodel='test-model'\ndimension=3\n[vector.embeddings.servers.local]\nendpoint='ftp://127.0.0.1/v1'\n")
				cfg, err = config.LoadReadOnly()
				require.NoError(t, err)
			} else if failure != "configured provider" {
				oldInspect := inspectLocalEmbeddings
				t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					if failure == "model inspection" {
						return embedlocal.Status{}, inspectionError
					}
					return embedlocal.Status{}, nil
				}
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": {Local: true}}
			} else {
				cfg.DeploymentEmbeddings = &config.VectorEmbeddingsServerConfig{Endpoint: "https://embeddings.example/v1"}
			}
			owner := liveVectorServingFixture(t, cfg, database, vectorServing{})
			if failure == "invalid TOML" {
				writeTestConfig(t, dir, "[vector\n")
				ln, port := listenLoopback(t)
				cfg.Host, cfg.Port = "127.0.0.1", port
				ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
				body := embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{}`), http.StatusBadRequest)
				assert.Contains(t, string(body), "existing config invalid")
				return
			}
			if failure == "model inspection" {
				err := owner.Setup(0)
				require.ErrorIs(t, err, inspectionError)
				assert.NotErrorIs(t, err, vector.ErrInvalidBuildRequest)
				return
			}
			if failure == "config read" {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "config.toml"), 0o700))
				var pathError *os.PathError
				err := owner.Setup(0)
				require.ErrorAs(t, err, &pathError)
				assert.NotErrorIs(t, err, vector.ErrInvalidBuildRequest)
				return
			}
			if failure == "configured provider" {
				status, err := owner.SetupStatus(t.Context())
				require.NoError(t, err)
				assert.Empty(t, status.Action)
			}
			err = owner.Setup(0)
			require.ErrorIs(t, err, vector.ErrInvalidBuildRequest)
			if failure == "invalid endpoint" {
				assert.ErrorIs(t, err, config.ErrInvalidVectorSetup)
			}
		})
	}
}

func TestEmbeddingsSetupStatusKeepsActiveProviderDuringSharedInstall(t *testing.T) {
	oldInspect := inspectLocalEmbeddings
	t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
	for _, provider := range []string{"configured Ollama", "discovered Ollama", "built-in"} {
		t.Run(provider, func(t *testing.T) {
			cfg, err := config.Default()
			require.NoError(t, err)
			cfg.Vector.Enabled = true
			if provider == "configured Ollama" {
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"ollama": {Endpoint: "http://127.0.0.1:11434/v1"}}
			}
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{Installing: true, DownloadBytes: 295000000, OllamaAvailable: provider == "discovered Ollama"}, nil
			}
			status, err := embeddingsSetupStatus(t.Context(), server.EmbeddingsSetupStatus{}, cfg)
			require.NoError(t, err)
			if provider == "built-in" {
				assert.Equal(t, int64(295000000), status.MissingBytes, "built-in status discloses the shared download size")
				assert.Equal(t, "built-in", status.Provider)
				assert.True(t, status.Installing)
			} else {
				assert.Equal(t, "ollama", status.Provider)
				assert.False(t, status.Installing, "another app's install must not replace the active Ollama state")
				assert.Empty(t, status.Action)
			}
		})
	}
}

func TestLiveEmbeddingsSetupDisclosesVerifiedRepairSize(t *testing.T) {
	for _, scenario := range []string{"repair", "ollama-stopped"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			dir := testDataDir(t)
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			shared, err := embedlocal.DefaultDir()
			require.NoError(t, err)
			modelDir := filepath.Join(shared, "nomic-embed-text-v1.5", "e9b6763023c676ca8431644204f50c2b100d9aab")
			require.NoError(t, os.MkdirAll(modelDir, 0o700))
			// Correct metadata hides corrupt content from Inspect's inexpensive probe.
			require.NoError(t, os.WriteFile(filepath.Join(modelDir, "tokenizer.json"), make([]byte, 711396), 0o600))
			oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
			t.Cleanup(func() {
				inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
			})
			ollama := scenario == "ollama-stopped"
			inspectLocalEmbeddings = func(ctx context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
				opts.SkipOllama = true
				status, err := embedlocal.Inspect(ctx, opts)
				status.OllamaAvailable = ollama
				return status, err
			}
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
			var approved atomic.Bool
			var requiredBytes atomic.Int64
			installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				confirm := opts.Confirm
				opts.Confirm = func(ctx context.Context, size int64) (bool, error) {
					requiredBytes.Store(size)
					consent, err := confirm(ctx, size)
					approved.Store(consent)
					if consent {
						return false, errors.New("test stops before approved download")
					}
					return false, err
				}
				return embedlocal.Install(ctx, opts)
			}
			owner := liveVectorServingFixture(t, cfg, database, vectorServing{})
			initial, err := owner.SetupStatus(t.Context())
			require.NoError(t, err)
			require.Positive(t, initial.MissingBytes)
			approvedBytes := initial.MissingBytes
			if ollama {
				require.Equal(t, "ollama", initial.Provider)
				require.Equal(t, "turn-on", initial.Action)
				ollama = false
				approvedBytes = 0 // The Ollama row disclosed no download.
			}
			require.NoError(t, owner.Setup(approvedBytes))
			<-owner.done
			assert.False(t, approved.Load())
			retry, err := owner.SetupStatus(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "built-in", retry.Provider)
			assert.Equal(t, "turn-on", retry.Action)
			assert.Contains(t, retry.Error, "declined")
			assert.Greater(t, retry.MissingBytes, initial.MissingBytes)
			assert.Equal(t, requiredBytes.Load(), retry.MissingBytes)
			require.NoError(t, owner.Setup(retry.MissingBytes))
			<-owner.done
			assert.Equal(t, retry.MissingBytes, requiredBytes.Load())
			assert.True(t, approved.Load(), "Retry must approve the verified size disclosed after repair was refused")
		})
	}
}
