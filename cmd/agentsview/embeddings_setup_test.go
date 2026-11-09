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
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/vector"
	"go.kenn.io/kit/daemon"
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
	for _, recipe := range []string{"fresh", "automatic", "nomic", "changed-provider"} {
		t.Run(recipe, func(t *testing.T) {
			setTestHome(t, t.TempDir())
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
				"changed-provider": "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text'\ndimension = 768\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\n",
				"nomic":            "[vector]\nenabled = false\n[vector.embeddings]\nmodel = 'nomic-embed-text:v1.5'\ndimension = 768\n[vector.embeddings.servers.local]\nendpoint = '" + stub.URL + "/v1'\nollama_cpu_fallback = true\n",
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
			initial := status()
			require.NotNil(t, initial.Setup)
			assert.False(t, initial.Setup.Enabled)
			assert.Equal(t, "turn-on", initial.Setup.Action)
			assert.Zero(t, installs.Load())
			// The row discloses a download only for the built-in provider.
			var shown int64
			if initial.Setup.Provider == "built-in" {
				shown = initial.Setup.MissingBytes
			}
			if recipe == "changed-provider" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = false\n"), 0o600))
			}
			if recipe == "fresh" {
				owner.mu.Lock()
				owner.setup.Error = "earlier setup error"
				owner.mu.Unlock()
			}
			request("POST", "/api/v1/embeddings/setup", fmt.Sprintf(`{"approved_bytes":%d}`, shown), 202)
			if recipe == "changed-provider" {
				<-owner.done
				retry := status()
				assert.Equal(t, "built-in", retry.Setup.Provider)
				assert.NotEmpty(t, retry.Setup.Error)
				assert.Equal(t, "turn-on", retry.Setup.Action)
				assert.Zero(t, reloads.Load(), "declined activation must not publish a config")
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
			assert.EqualValues(t, 1, reloads.Load())
			assert.Empty(t, status().Setup.Action)
			assert.Equal(t, recipe == "fresh", installs.Load() > 0)
			if recipe == "fresh" {
				var result db.ContentSearchPage
				require.NoError(t, json.Unmarshal(request("GET", "/api/v1/search/content?pattern=network%20pooling&mode=semantic&include_one_shot=true", "", 200), &result))
				require.NotEmpty(t, result.Matches)
				assert.Equal(t, "session-one", result.Matches[0].SessionID)
				dbtest.SeedSessionWithMessages(t, database, "automated", "sample", []db.Message{dbtest.UserMsg("automated", 0, "automated connections")}, func(s *db.Session) { s.IsAutomated = true })
				request("POST", "/api/v1/embeddings/build", `{"full_rebuild":true}`, 202)
				awaitBuild()
				generations, err := owner.serving.manager.Generations(t.Context())
				require.NoError(t, err)
				assert.EqualValues(t, 2, generations[0].Embedded)
				// Quiesce the consumers so the notification remains observable.
				owner.cancel()
				require.Eventually(t, func() bool {
					select {
					case <-owner.serving.Scheduler.done:
						return true
					default:
						return false
					}
				}, time.Second, time.Millisecond)
				request("DELETE", "/api/v1/sessions/session-one", "", 204)
				require.Len(t, owner.serving.Scheduler.dirty, 1,
					"DELETE must queue the scheduler created by the setup reload")
				(<-owner.serving.Scheduler.dirty)()
			}
		})
	}
}

func TestLiveEmbeddingsSetupStatusAndExportDuringWriteLockRetry(t *testing.T) {
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint
	})
	setTestHome(t, t.TempDir())
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
	before := inspections.Load()
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Busy)
	assert.Equal(t, before, inspections.Load(), "setup progress must not probe Ollama")
	<-owner.done
	status, err = owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.False(t, status.Busy)
	assert.Equal(t, "turn-on", status.Action)
	assert.Equal(t, "Another process may be using search. Close it and retry.", status.Error)
	export, ok, err := owner.push.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok, "push must see the generation built by the process holding the write lock")
	hashes, err := export.SessionDocHashes(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, hashes, "session-1")
	require.NoError(t, export.Close())

	movedPath := filepath.Join(t.TempDir(), "moved.db")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), fmt.Appendf(nil, "[vector]\nenabled = true\ndb_path = %q\n", movedPath), 0o600))
	releaseHeld()
	require.NoError(t, owner.Setup(0, nil))
	<-owner.done
	setup, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	require.Empty(t, setup.Error)
	require.Empty(t, setup.Action)
	// An empty build can activate before setup returns; inspect its completed export.
	owner.serving.manager.Wait()
	next, ok, err := owner.push.BeginExport(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok, "the empty build must publish an exportable generation")
	defer func() { require.NoError(t, next.Close()) }()
	hashes, err = next.SessionDocHashes(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, hashes, "the reloaded index must not export the previous index's sessions")
}

func TestLiveEmbeddingsSetupProgress(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
	})
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{DownloadBytes: 1000}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	started := make(chan struct{})
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		opts.Progress(100, 1000)
		close(started)
		<-ctx.Done()
		return embedlocal.Installation{}, ctx.Err()
	}
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	require.NoError(t, owner.Setup(0, nil))
	<-started
	require.ErrorIs(t, owner.Setup(0, nil), vector.ErrBuildRunning)
	status, err := owner.SetupStatus(t.Context())
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

func TestLiveEmbeddingsSetupFailures(t *testing.T) {
	for _, failure := range []string{"already enabled", "configured provider", "invalid TOML", "model inspection", "config read", "repair and full", "unknown server"} {
		t.Run(failure, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			inspectionError := errors.New("model status unavailable")
			switch failure {
			case "already enabled":
				writeTestConfig(t, dir, "[vector]\nenabled=true\n")
				cfg, err = config.LoadReadOnly()
				require.NoError(t, err)
				oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
				t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{DownloadBytes: 1000}, nil
				}
				localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
			case "model inspection":
				oldInspect := inspectLocalEmbeddings
				t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{}, inspectionError
				}
			case "configured provider":
				cfg.DeploymentEmbeddings = &config.VectorEmbeddingsServerConfig{Endpoint: "https://embeddings.example/v1"}
			}
			var owner *liveVectorServing
			if failure == "already enabled" {
				owner, err = newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
				require.NoError(t, err)
				defer func() { require.NoError(t, owner.Close()) }()
				require.NotNil(t, owner.Manager(""))
			} else {
				owner = liveVectorServingFixture(t, cfg, database, vectorServing{})
			}
			if failure == "invalid TOML" {
				writeTestConfig(t, dir, "[vector\n")
				ln, port := listenLoopback(t)
				cfg.Host, cfg.Port = "127.0.0.1", port
				ts := startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
				embeddingRequest(t, ts.Client(), ts.URL, "POST", "/api/v1/embeddings/setup", "application/json", strings.NewReader(`{}`), http.StatusBadRequest)
				return
			}
			if failure == "model inspection" {
				err := owner.Setup(0, nil)
				require.ErrorIs(t, err, inspectionError)
				assert.NotErrorIs(t, err, vector.ErrInvalidBuildRequest)
				return
			}
			if failure == "config read" {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "config.toml"), 0o700))
				var pathError *os.PathError
				err := owner.Setup(0, nil)
				require.ErrorAs(t, err, &pathError)
				assert.NotErrorIs(t, err, vector.ErrInvalidBuildRequest)
				return
			}
			if failure == "configured provider" {
				status, err := owner.SetupStatus(t.Context())
				require.NoError(t, err)
				assert.Empty(t, status.Action)
			}
			var build *vector.BuildRequest
			switch failure {
			case "repair and full":
				build = &vector.BuildRequest{RepairInvalid: true, FullRebuild: true}
			case "unknown server":
				build = &vector.BuildRequest{Using: "missing"}
			}
			err = owner.Setup(0, build)
			require.ErrorIs(t, err, vector.ErrInvalidBuildRequest)
		})
	}
}

func TestEmbeddingsSetupStatusIdentifiesProviders(t *testing.T) {
	oldInspect := inspectLocalEmbeddings
	t.Cleanup(func() { inspectLocalEmbeddings = oldInspect })
	for _, provider := range []string{"configured", "configured CPU fallback", "configured Ollama port", "discovered Ollama", "built-in"} {
		t.Run(provider, func(t *testing.T) {
			cfg, err := config.Default()
			require.NoError(t, err)
			cfg.Vector.Enabled = true
			switch provider {
			case "configured Ollama port":
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"ollama": {Endpoint: "http://127.0.0.1:11434/v1"}}
			case "configured", "configured CPU fallback":
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": {Endpoint: "http://127.0.0.1:30000/v1", OllamaCPUFallback: provider == "configured CPU fallback"}}
			}
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
				return embedlocal.Status{Installing: true, DownloadBytes: 295000000, OllamaAvailable: provider == "discovered Ollama"}, nil
			}
			status, err := embeddingsSetupStatus(t.Context(), server.EmbeddingsSetupStatus{}, cfg)
			require.NoError(t, err)
			if provider == "built-in" {
				assert.Empty(t, status.Action)
				assert.Equal(t, int64(295000000), status.MissingBytes, "built-in status discloses the shared download size")
				assert.Equal(t, "built-in", status.Provider)
				assert.True(t, status.Installing)
			} else {
				if provider == "configured" {
					assert.Equal(t, "configured", status.Provider)
				} else {
					assert.Equal(t, "ollama", status.Provider)
				}
				if provider == "discovered Ollama" {
					assert.False(t, status.Installing, "another app's install must not replace the active Ollama state")
				}
				assert.Empty(t, status.Action)
			}
		})
	}
}

func TestLiveEmbeddingsSetupDisclosesVerifiedRepairSize(t *testing.T) {
	setTestHome(t, t.TempDir())
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
	inspectLocalEmbeddings = func(ctx context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
		opts.SkipOllama = true
		return embedlocal.Inspect(ctx, opts)
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
	require.NoError(t, owner.Setup(initial.MissingBytes, nil))
	<-owner.done
	assert.False(t, approved.Load())
	retry, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "built-in", retry.Provider)
	assert.Equal(t, "turn-on", retry.Action)
	assert.Contains(t, retry.Error, "declined")
	assert.Greater(t, retry.MissingBytes, initial.MissingBytes)
	assert.Equal(t, requiredBytes.Load(), retry.MissingBytes)
	require.NoError(t, owner.Setup(retry.MissingBytes, nil))
	<-owner.done
	assert.Equal(t, retry.MissingBytes, requiredBytes.Load())
	assert.True(t, approved.Load(), "Retry must approve the verified size disclosed after repair was refused")
}

func TestCLIEmbeddingsSetup(t *testing.T) {
	for _, tc := range []struct {
		enabledAfterStart bool
		store             string
	}{{false, ""}, {true, ""}, {false, "recall"}} {
		t.Run(fmt.Sprintf("enabled-after-start=%t/store=%s", tc.enabledAfterStart, tc.store), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			encoder := newEmbeddingsStubServer(t, 3)
			defer encoder.Close()
			writeEmbeddingsTestConfig(t, dir, encoder.URL)
			path := filepath.Join(dir, "config.toml")
			contents, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(contents), "enabled = true", "enabled = false", 1)), 0o600))
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			ln, port := listenLoopback(t)
			cfg.Host, cfg.Port = "127.0.0.1", port
			ts := startTestServer(t, ln, server.New(cfg, database, nil, append(owner.ServerOptions(), server.WithBaseContext(t.Context()))...).Handler())
			endpoint := serverEndpoint(t, ts)
			writeDaemonRuntimeForTest(t, dir, endpoint.Host, endpoint.Port, "test", false)
			if tc.enabledAfterStart {
				require.NoError(t, os.WriteFile(path, contents, 0o600))
			}
			oldInterval := embeddingsPollInterval
			embeddingsPollInterval = 10 * time.Millisecond
			t.Cleanup(func() { embeddingsPollInterval = oldInterval })
			var out strings.Builder
			err = runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{Using: "local", Store: tc.store})
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(out.String(), "[y/N]"))
			cfg, err = config.LoadReadOnly()
			require.NoError(t, err)
			assert.True(t, cfg.Vector.Enabled)
			assert.NotNil(t, owner.Manager(tc.store).Status().LastResult)
			if tc.store == "recall" {
				assert.Nil(t, owner.Manager("").Status().LastResult)
			}
		})
	}
}

func TestCLIEmbeddingsSetupDeclined(t *testing.T) {
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	for _, tc := range []struct {
		input    string
		download bool
	}{{"no\n", false}, {"no\n", true}} {
		t.Run(fmt.Sprintf("input=%q/download=%t", tc.input, tc.download), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			writeEmbeddingsTestConfig(t, dir, "http://127.0.0.1:1")
			path := filepath.Join(dir, "config.toml")
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			before = []byte(strings.Replace(string(before), "enabled = true", "enabled = false", 1))
			require.NoError(t, os.WriteFile(path, before, 0o600))
			if tc.download {
				oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
				t.Cleanup(func() {
					inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
				})
				require.NoError(t, os.WriteFile(path, []byte("[vector]\nenabled = false\n"), 0o600))
				before, err = os.ReadFile(path)
				require.NoError(t, err)
				inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
					return embedlocal.Status{DownloadBytes: 100}, nil
				}
				localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
				installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
					_, err := opts.Confirm(ctx, 101)
					return embedlocal.Installation{}, err
				}
			}
			startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
				"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, `{"setup":{"action":"turn-on"}}`)
				},
			})
			var out strings.Builder
			require.ErrorContains(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader(tc.input), EmbeddingsBuildOptions{Yes: true}), "search setup declined")
			if tc.download {
				assert.Contains(t, out.String(), "Downloads approximately 1 MB")
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Contains(t, out.String(), "[y/N]")
			_, err = os.Stat(filepath.Join(dir, "vectors.db"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

func TestCLIEmbeddingsSetupDownload(t *testing.T) {
	oldInspect, oldEndpoint, oldInstall := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings = oldInspect, oldEndpoint, oldInstall
	})
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	encoder := newEmbeddingsStubServer(t, 768)
	t.Cleanup(encoder.Close)
	var installed atomic.Bool
	var installs atomic.Int64
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{DownloadBytes: 100}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		if !installed.Load() {
			return "", embedlocal.ErrNotFound
		}
		return encoder.URL, nil
	}
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		installs.Add(1)
		approved, err := opts.Confirm(ctx, 101)
		if err != nil {
			return embedlocal.Installation{}, err
		}
		if !approved {
			return embedlocal.Installation{}, errors.New("download was not approved")
		}
		installed.Store(true)
		return embedlocal.Installation{DataDir: opts.DataDir, Executable: "fixture"}, nil
	}
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, append(owner.ServerOptions(), server.WithBaseContext(t.Context()))...).Handler())
	endpoint := serverEndpoint(t, ts)
	writeDaemonRuntimeForTest(t, dir, endpoint.Host, endpoint.Port, "test", false)
	oldInterval := embeddingsPollInterval
	embeddingsPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { embeddingsPollInterval = oldInterval })
	var out strings.Builder
	err = runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
	assert.Contains(t, out.String(), "Downloads approximately 1 MB")
	assert.Equal(t, 1, strings.Count(out.String(), "[y/N]"))
	cfg, loadErr := config.LoadReadOnly()
	require.NoError(t, loadErr)
	require.NoError(t, err)
	assert.EqualValues(t, 1, installs.Load())
	assert.True(t, cfg.Vector.Enabled)
	assert.Contains(t, out.String(), "Embedded 1 documents")
}

func TestCLIEmbeddingsSetupJoinsDifferentStore(t *testing.T) {
	oldEndpoint := localEmbeddingEndpoint
	t.Cleanup(func() { localEmbeddingEndpoint = oldEndpoint })
	for _, busy := range []bool{true, false} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			writeTestConfig(t, dir, "[vector]\nenabled = false\n")
			var statusCalls atomic.Int64
			localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				if busy {
					assert.GreaterOrEqual(t, statusCalls.Load(), int64(2), "provider preparation must wait for setup")
				}
				return "http://127.0.0.1:1", nil
			}
			var builds atomic.Int64
			startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
				"GET /api/v1/embeddings/status": func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "recall", r.URL.Query().Get("store"))
					call := statusCalls.Add(1)
					first := call == 1
					w.Header().Set("Content-Type", "application/json")
					if first {
						if busy {
							_, _ = io.WriteString(w, `{"setup":{"action":"turn-on","busy":true}}`)
						} else {
							_, _ = io.WriteString(w, `{"setup":{"action":"turn-on"}}`)
						}
						return
					}
					if call == 2 {
						cfg, err := config.LoadReadOnly()
						require.NoError(t, err)
						require.NoError(t, cfg.SaveVectorSetup())
					}
					_, _ = io.WriteString(w, `{"running":false,"setup":{"enabled":true,"busy":false},"last_result":{}}`)
				},
				"POST /api/v1/embeddings/setup": func(w http.ResponseWriter, _ *http.Request) {
					assert.False(t, busy)
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"detail":"an embeddings build is already running"}`)
				},
				"POST /api/v1/embeddings/build": func(w http.ResponseWriter, r *http.Request) {
					var request vector.BuildRequest
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, "recall", request.Store)
					assert.True(t, request.Backstop)
					builds.Add(1)
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"started":true}`)
				},
			})
			var out strings.Builder
			require.NoError(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{Store: "recall", Backstop: true}))
			assert.EqualValues(t, 1, builds.Load())
			if busy {
				assert.NotContains(t, out.String(), "[y/N]")
			}
		})
	}
}

func TestCLIEmbeddingsFreshSetupAutostartsAndSyncs(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
	fixture := testConfigWithClaudeFixture(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", fixture.DataDir)
	path := filepath.Join(fixture.DataDir, "config.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, "claude_project_dirs = [%q]\n", fixture.AgentDirs[parser.AgentClaude][0]), 0o600))
	encoder := newEmbeddingsStubServer(t, 768)
	t.Cleanup(encoder.Close)
	oldInspect, oldEndpoint := inspectLocalEmbeddings, localEmbeddingEndpoint
	t.Cleanup(func() { inspectLocalEmbeddings, localEmbeddingEndpoint = oldInspect, oldEndpoint })
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		return embedlocal.Status{OllamaAvailable: true}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) { return encoder.URL, nil }
	var started bool
	stubStartBackgroundServeForTransport(t, func(ctx context.Context, cfg *config.Config, _ time.Duration, _ bool) (*DaemonRuntime, error) {
		started = true
		database := dbtest.OpenTestDBAt(t, cfg.DBPath)
		engine := syncpkg.NewEngine(ctx, database, localSyncEngineConfig(*cfg))
		t.Cleanup(engine.Close)
		engine.SyncAll(ctx, nil)
		owner, err := newLiveVectorServing(t.Context(), *cfg, database, nil, config.LoadReadOnly)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, owner.Close()) })
		ln, port := listenLoopback(t)
		cfg.Host, cfg.Port = "127.0.0.1", port
		ts := startTestServer(t, ln, server.New(*cfg, database, nil, append(owner.ServerOptions(), server.WithBaseContext(t.Context()))...).Handler())
		endpoint := serverEndpoint(t, ts)
		writeDaemonRuntimeForTest(t, cfg.DataDir, endpoint.Host, endpoint.Port, version, false)
		runtime := FindWritableDaemonRuntime(cfg.DataDir, cfg.AuthToken)
		require.NotNil(t, runtime)
		return runtime, nil
	})
	oldInterval := embeddingsPollInterval
	embeddingsPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { embeddingsPollInterval = oldInterval })
	var out strings.Builder
	require.NoError(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{}))
	assert.True(t, started)
	assert.Equal(t, 1, strings.Count(out.String(), "[y/N]"))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	assert.True(t, cfg.Vector.Enabled)
}

func TestCLIEmbeddingsSetupRequiresDaemon(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	dir := testDataDir(t)
	path := filepath.Join(dir, "config.toml")
	contents := []byte("[vector]\nenabled = false\n")
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	var out strings.Builder
	err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
	require.ErrorContains(t, err, "search setup requires a daemon")
	assert.ErrorContains(t, err, "agentsview daemon start")
	assert.ErrorContains(t, err, "agentsview index embeddings build")
	assert.ErrorContains(t, err, "Settings → Embeddings → Turn on")
	assert.NotContains(t, out.String(), "[y/N]")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, contents, after)
}

func TestPollDaemonSetupPrintsDownloadProgress(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			_, _ = io.WriteString(w, `{"setup":{"busy":true,"downloaded_bytes":500000,"total_bytes":1000000}}`)
		case 2:
			_, _ = io.WriteString(w, `{"setup":{"busy":true,"downloaded_bytes":500001,"total_bytes":1000000}}`)
		case 3:
			_, _ = io.WriteString(w, `{"setup":{"busy":true,"downloaded_bytes":500001,"total_bytes":1000001}}`)
		default:
			_, _ = io.WriteString(w, `{"setup":{"busy":false,"downloaded_bytes":1000001,"total_bytes":1000001}}`)
		}
	}))
	t.Cleanup(srv.Close)
	oldInterval := embeddingsPollInterval
	embeddingsPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { embeddingsPollInterval = oldInterval })
	var out strings.Builder
	require.NoError(t, pollDaemonSetup(t.Context(), &out, embeddingsDaemonClient{baseURL: srv.URL}))
	assert.Equal(t, "Download: 50% of 1 MB\nDownload: 50% of 2 MB\nDownload: 100% of 2 MB\n", out.String())
}

func TestCLIEmbeddingsSetupRejectsOlderDaemonBeforeConsent(t *testing.T) {
	for _, missingStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-status=%t", missingStatus), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			setTestVersion(t, "2.0.0")
			dir := testDataDir(t)
			path := filepath.Join(dir, "config.toml")
			contents := []byte("[vector]\nenabled = false\n")
			require.NoError(t, os.WriteFile(path, contents, 0o600))
			var posts atomic.Int64
			mux := http.NewServeMux()
			serverVersion := "1.0.0"
			if missingStatus {
				serverVersion = version
			}
			mux.Handle("GET /api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: serverVersion}))
			mux.HandleFunc("GET /api/v1/embeddings/status", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"running":false}`)
			})
			if missingStatus {
				mux.HandleFunc("POST /api/v1/embeddings/setup", func(w http.ResponseWriter, _ *http.Request) {
					posts.Add(1)
					w.WriteHeader(http.StatusNotFound)
				})
			}
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			endpoint := serverEndpoint(t, srv)
			writeDaemonRuntimeForTest(t, dir, endpoint.Host, endpoint.Port, serverVersion, false)
			var out strings.Builder
			err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
			require.ErrorIs(t, err, errEmbeddingsSetupDaemonRestart)
			assert.ErrorContains(t, err, "agentsview daemon restart")
			assert.NotContains(t, out.String(), "[y/N]")
			if missingStatus {
				assert.Zero(t, posts.Load())
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, contents, after)
		})
	}
}

func TestCLIEmbeddingsSetupReportsRemovedServer(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	encoder := newEmbeddingsStubServer(t, 3)
	t.Cleanup(encoder.Close)
	writeEmbeddingsTestConfig(t, dir, encoder.URL)
	path := filepath.Join(dir, "config.toml")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(contents), "enabled = true", "enabled = false", 1)), 0o600))
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, func() (config.Config, error) {
		current, err := os.ReadFile(path)
		if err != nil {
			return config.Config{}, err
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(current), "servers.local", "servers.other", 1)), 0o600); err != nil {
			return config.Config{}, err
		}
		return config.LoadReadOnly()
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	ts := startTestServer(t, ln, server.New(cfg, database, nil, append(owner.ServerOptions(), server.WithBaseContext(t.Context()))...).Handler())
	endpoint := serverEndpoint(t, ts)
	writeDaemonRuntimeForTest(t, dir, endpoint.Host, endpoint.Port, version, false)
	oldInterval := embeddingsPollInterval
	embeddingsPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { embeddingsPollInterval = oldInterval })
	var out strings.Builder
	err = runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{Using: "local"})
	require.ErrorContains(t, err, `no embeddings server named "local"`)
	cfg, err = config.LoadReadOnly()
	require.NoError(t, err)
	assert.True(t, cfg.Vector.Enabled)

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	updated := strings.Replace(string(current), "[vector.embeddings]", "[vector.embeddings]\ndefault_server = 'other'", 1)
	updated += fmt.Sprintf("\n[vector.embeddings.servers.local]\nendpoint = %q\n", encoder.URL)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o600))
	out.Reset()
	require.NoError(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader(""), EmbeddingsBuildOptions{Using: "other"}))
	status, err := owner.SetupStatus(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.Error)
}
