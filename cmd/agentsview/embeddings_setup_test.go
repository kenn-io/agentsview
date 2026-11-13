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

func TestSharedChoiceStartsIndexing(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	require.NoError(t, embedlocal.RecordChoice())
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	encoded := make(chan struct{}, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeEmbeddingsResponse(t, w, len(request.Input), 768)
		select {
		case encoded <- struct{}{}:
		default:
		}
	}))
	defer stub.Close()
	oldInspect, oldEndpoint, oldBattery := inspectLocalEmbeddings, localEmbeddingEndpoint, onBattery
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, onBattery = oldInspect, oldEndpoint, oldBattery
	})
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }
	localEmbeddingEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		assert.True(t, opts.SkipOllama)
		return stub.URL + "/v1", nil
	}
	onBattery = func() bool { return false }
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close()) }()
	select {
	case <-encoded:
	case <-time.After(45 * time.Second):
		require.FailNow(t, "shared choice did not start indexing")
	}
	saved, err := config.LoadReadOnly()
	require.NoError(t, err)
	assert.False(t, saved.Vector.Enabled)
	owner.Manager("").Wait()
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	writeDaemonRuntimeForTest(t, dir, "127.0.0.1", port, "test", false)
	out, err := executeCommand(newRootCommand(), "index", "embeddings", "build", "--repair-invalid")
	require.NoError(t, err, out)
	assert.Contains(t, out, "Repair")
}

func TestSharedChoiceRespectsSettings(t *testing.T) {
	for _, raw := range []string{
		"[vector]\nenabled = false\n",
		"[vector.embeddings]\nmodel = 'custom'\n",
		"[vector.embeddings]\nmodel = 'custom'\ndimension = 768\n[vector.embeddings.servers.remote]\nendpoint = 'https://example.com/v1'\n",
		"[pg]\nurl = 'postgres://example.com/db'\n",
		"[clickhouse]\nurl = 'clickhouse://example.com/db'\n",
	} {
		t.Run(strings.Split(raw, "\n")[0]+raw, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			writeTestConfig(t, dir, raw)
			require.NoError(t, embedlocal.RecordChoice())
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
			old := inspectLocalEmbeddings
			t.Cleanup(func() { inspectLocalEmbeddings = old })
			inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) { return embedlocal.Status{}, nil }
			owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
			require.NoError(t, err)
			defer func() { require.NoError(t, owner.Close()) }()
			assert.Nil(t, owner.Manager(""))
		})
	}
}

func TestSharedChoiceExpandsIndexPath(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	writeTestConfig(t, dir, "[vector]\ndb_path = '~/vectors.db'\n")
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	candidate, eligible, err := cfg.SharedVectorSetup()
	require.NoError(t, err)
	require.True(t, eligible)
	assert.Equal(t, filepath.Join(os.Getenv("HOME"), "vectors.db"), candidate.Vector.DBPath)
}

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
			if recipe == "fresh" || recipe == "automatic" {
				chosen, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				assert.Equal(t, recipe == "fresh", chosen)
			}
			assert.EqualValues(t, 1, reloads.Load())
			assert.Empty(t, status().Setup.Action)
			assert.Equal(t, recipe == "fresh", installs.Load() > 0)
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
	saved, err := config.LoadReadOnly()
	require.NoError(t, err)
	require.True(t, saved.Vector.Enabled)
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
	writeDaemonRuntimeForTest(t, dir, "127.0.0.1", port, "test", false)
	var out strings.Builder
	require.NoError(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{}))
	assert.Contains(t, out.String(), "Retry search setup?")
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
	require.NoError(t, owner.Setup(0))
	<-started
	require.ErrorIs(t, owner.Setup(0), vector.ErrBuildRunning)
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
	for _, failure := range []string{"already enabled", "configured provider", "invalid TOML", "model inspection", "config read"} {
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
	require.NoError(t, owner.Setup(initial.MissingBytes))
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
}

func TestCLIEmbeddingsSetup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		postStatus int
		stale      bool
	}{
		{"finished-before-status", 0, false},
		{"setup-conflict", http.StatusConflict, false},
		{"finished-during-confirmation", http.StatusBadRequest, false},
		{"stale-config", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = false\n"), 0o600))
			var reads, setupPosts, buildPosts int
			startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
				"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
					reads++
					w.Header().Set("Content-Type", "application/json")
					switch {
					case tc.postStatus != 0 && reads == 1:
						io.WriteString(w, `{"setup":{"action":"turn-on","provider":"ollama"}}`)
					case tc.postStatus == http.StatusConflict && reads == 2:
						io.WriteString(w, `{"setup":{"action":"turn-on","busy":true}}`)
					case tc.postStatus != http.StatusConflict && buildPosts == 0:
						if !tc.stale {
							if !assert.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = true\n[vector.embeddings]\nmodel = 'nomic-embed-text'\ndimension = 768\n"), 0o600)) {
								w.WriteHeader(http.StatusInternalServerError)
								return
							}
						}
						io.WriteString(w, `{"setup":{"enabled":true},"running":true}`)
					default:
						io.WriteString(w, `{"setup":{},"last_result":{}}`)
					}
				},
				"POST /api/v1/embeddings/setup": func(w http.ResponseWriter, _ *http.Request) {
					setupPosts++
					w.WriteHeader(tc.postStatus)
				},
				"POST /api/v1/embeddings/build": func(w http.ResponseWriter, _ *http.Request) {
					buildPosts++
					w.WriteHeader(http.StatusAccepted)
				},
			})
			var out strings.Builder
			err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
			if tc.stale {
				require.ErrorContains(t, err, "search by meaning is off")
				require.ErrorContains(t, err, "agentsview daemon restart")
				assert.Zero(t, buildPosts)
				return
			}
			require.NoError(t, err)
			if tc.postStatus == 0 {
				assert.NotContains(t, out.String(), "[y/N]")
				assert.Zero(t, setupPosts)
			} else {
				assert.Equal(t, 1, setupPosts)
			}
			if tc.postStatus == http.StatusConflict {
				assert.Contains(t, out.String(), "Search setup is already running.")
				assert.Zero(t, buildPosts)
			} else {
				assert.Equal(t, 1, buildPosts)
			}
		})
	}
	for _, tc := range []struct {
		name, provider, input string
		missing               int64
		flagged, enabled      bool
	}{
		{"download", "built-in", "yes\n", 1000001, false, false},
		{"flagged", "built-in", "yes\n", 0, true, false},
		{"enabled-flagged", "ollama", "yes\n", 0, true, true},
		{"enabled", "ollama", "yes\n", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			if tc.provider == "ollama" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[vector]\nenabled = %t\n[vector.embeddings]\nmodel = 'nomic-embed-text'\ndimension = 768\n", tc.enabled)), 0o600))
			}
			var posts, reads int
			var approved int64
			handlers := map[string]http.HandlerFunc{
				"POST /api/v1/embeddings/build": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNotImplemented)
				},
				"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					reads++
					if posts == 0 {
						fmt.Fprintf(w, `{"setup":{"action":"turn-on","provider":%q,"missing_bytes":%d,"enabled":%t}}`, tc.provider, tc.missing, tc.enabled)
						return
					}
					if reads == 2 && tc.missing > 0 {
						fmt.Fprintf(w, `{"setup":{"action":"turn-on","busy":true,"downloaded_bytes":%d,"total_bytes":%d}}`, tc.missing, tc.missing)
						return
					}
					io.WriteString(w, `{"setup":{"enabled":true,"error":"inspection failed"},"last_result":{"Fill":{"Documents":3,"Chunks":4}}}`)
				},
				"POST /api/v1/embeddings/setup": func(w http.ResponseWriter, r *http.Request) {
					posts++
					var body struct {
						ApprovedBytes int64 `json:"approved_bytes"`
					}
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					approved = body.ApprovedBytes
					w.WriteHeader(http.StatusAccepted)
					io.WriteString(w, `{"started":true}`)
				},
			}
			startEmbeddingsTestDaemon(t, dir, handlers)
			var out strings.Builder
			err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader(tc.input), EmbeddingsBuildOptions{FullRebuild: tc.flagged})
			if tc.flagged {
				require.ErrorContains(t, err, "--full-rebuild")
				require.ErrorContains(t, err, "agentsview index embeddings build")
				require.ErrorContains(t, err, "Settings → Embeddings → Turn on")
				assert.NotContains(t, out.String(), "[y/N]")
				assert.Zero(t, posts)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out.String(), "Embedded 3 documents (4 chunks)")
			assert.Equal(t, 1, posts)
			assert.Equal(t, tc.missing, approved)
			if tc.enabled {
				assert.Contains(t, out.String(), "Search setup is incomplete.")
				assert.Contains(t, out.String(), "Finish search setup?")
				assert.NotContains(t, out.String(), "Search by meaning is off.")
			}
			if tc.missing > 0 {
				assert.Contains(t, out.String(), "2 MB")
				assert.Contains(t, out.String(), "Download: 100%")
			} else {
				assert.Contains(t, out.String(), "uses Ollama with nomic-embed-text")
			}
		})
	}
	t.Run("enabled-built-in-one-confirmation", func(t *testing.T) {
		setTestHome(t, t.TempDir())
		dir := testDataDir(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = true\n[vector.embeddings]\nmodel = 'nomic-embed-text'\ndimension = 768\n"), 0o600))
		oldEndpoint, oldInstall := localEmbeddingEndpoint, installLocalEmbeddings
		t.Cleanup(func() { localEmbeddingEndpoint, installLocalEmbeddings = oldEndpoint, oldInstall })
		var installed bool
		localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
			if !installed {
				return "", embedlocal.ErrNotFound
			}
			return "http://127.0.0.1:1/v1", nil
		}
		installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
			approved, err := opts.Confirm(ctx, 1000001)
			if err != nil || !approved {
				return embedlocal.Installation{}, errors.New("download declined")
			}
			installed = true
			return embedlocal.Installation{}, nil
		}
		var setupPosts int
		startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
			"POST /api/v1/embeddings/build": func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotImplemented)
			},
			"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if setupPosts == 0 {
					io.WriteString(w, `{"setup":{"action":"turn-on","provider":"built-in","missing_bytes":1000001}}`)
				} else {
					io.WriteString(w, `{"setup":{"enabled":true},"last_result":{}}`)
				}
			},
			"POST /api/v1/embeddings/setup": func(w http.ResponseWriter, r *http.Request) {
				setupPosts++
				var body struct {
					ApprovedBytes int64 `json:"approved_bytes"`
				}
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.EqualValues(t, 1000001, body.ApprovedBytes)
				w.WriteHeader(http.StatusAccepted)
			},
		})
		var out strings.Builder
		err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, setupPosts)
		assert.Equal(t, 1, strings.Count(out.String(), "[y/N]"), out.String())
		assert.NotContains(t, out.String(), "Download approximately")
	})
	for _, input := range []string{"no\n", "yes\n"} {
		t.Run("inspection-error-"+strings.TrimSpace(input), func(t *testing.T) {
			setTestHome(t, t.TempDir())
			dir := testDataDir(t)
			var posts int
			startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
				"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"setup":{"action":"turn-on","provider":"built-in","error":"cannot inspect model directory"}}`)
				},
				"POST /api/v1/embeddings/setup": func(w http.ResponseWriter, _ *http.Request) {
					posts++
					w.WriteHeader(http.StatusAccepted)
				},
			})
			var out strings.Builder
			err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader(input), EmbeddingsBuildOptions{})
			require.Error(t, err)
			cause, prompt := strings.Index(out.String(), "cannot inspect model directory"), strings.Index(out.String(), "Retry search setup?")
			assert.GreaterOrEqual(t, cause, 0)
			assert.Greater(t, prompt, cause)
			assert.NotContains(t, out.String(), " MB")
			if input == "no\n" {
				assert.Zero(t, posts)
			} else {
				assert.Equal(t, 1, posts)
				assert.ErrorContains(t, err, "cannot inspect model directory")
			}
		})
	}
}

func TestCLIEmbeddingsBuildStartsBeforeStatus(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	writeEmbeddingsTestConfig(t, dir, "http://127.0.0.1:1")
	var builds atomic.Int32
	startEmbeddingsTestDaemon(t, dir, map[string]http.HandlerFunc{
		"POST /api/v1/embeddings/build": func(w http.ResponseWriter, _ *http.Request) {
			builds.Add(1)
			w.WriteHeader(http.StatusAccepted)
		},
		"GET /api/v1/embeddings/status": func(w http.ResponseWriter, _ *http.Request) {
			assert.EqualValues(t, 1, builds.Load(), "build must start before status is requested")
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"setup":{"enabled":true,"error":"inspection failed"},"last_result":{}}`)
		},
	})
	var out strings.Builder
	require.NoError(t, runEmbeddingsBuild(t.Context(), &out, strings.NewReader(""), EmbeddingsBuildOptions{}))
}

func TestCLIEmbeddingsSetupRequiresWritableDaemon(t *testing.T) {
	setTestHome(t, t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	dir := testDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[vector]\nenabled = false\n"), 0o600))
	var out strings.Builder
	err := runEmbeddingsBuild(t.Context(), &out, strings.NewReader("yes\n"), EmbeddingsBuildOptions{})
	require.ErrorContains(t, err, "search setup requires a writable daemon")
	assert.NotContains(t, out.String(), "[y/N]")
}

func TestCLIEmbeddingsFreshSetup(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "sessions.db"))
	dbtest.SeedSessionWithMessages(t, database, "session-one", "sample", []db.Message{dbtest.UserMsg("session-one", 0, "reuse database connections")})
	encoder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeEmbeddingsResponse(t, w, len(request.Input), 768)
	}))
	t.Cleanup(encoder.Close)
	oldInspect, oldEndpoint, oldInstall, oldBattery := inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings, onBattery
	t.Cleanup(func() {
		inspectLocalEmbeddings, localEmbeddingEndpoint, installLocalEmbeddings, onBattery = oldInspect, oldEndpoint, oldInstall, oldBattery
	})
	onBattery = func() bool { return false }
	var installed atomic.Bool
	inspectLocalEmbeddings = func(context.Context, embedlocal.Options) (embedlocal.Status, error) {
		if installed.Load() {
			return embedlocal.Status{}, nil
		}
		return embedlocal.Status{DownloadBytes: 100}, nil
	}
	localEmbeddingEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		if !installed.Load() {
			return "", embedlocal.ErrNotFound
		}
		return encoder.URL + "/v1", nil
	}
	installLocalEmbeddings = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		allowed, err := opts.Confirm(ctx, 100)
		if err != nil {
			return embedlocal.Installation{}, err
		}
		if !allowed {
			return embedlocal.Installation{}, errors.New("download declined")
		}
		opts.Progress(100, 100)
		installed.Store(true)
		return embedlocal.Installation{DataDir: opts.DataDir, Executable: "fixture"}, nil
	}
	cfg, err := config.LoadReadOnly()
	require.NoError(t, err)
	owner, err := newLiveVectorServing(t.Context(), cfg, database, nil, config.LoadReadOnly)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	startTestServer(t, ln, server.New(cfg, database, nil, owner.ServerOptions()...).Handler())
	writeDaemonRuntimeForTest(t, dir, "127.0.0.1", port, "test", false)
	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader("yes\n"))
	out, err := executeCommand(cmd, "index", "embeddings", "build")
	require.NoError(t, err, out)
	assert.Equal(t, 1, strings.Count(out, "[y/N]"))
	cfg, err = config.LoadReadOnly()
	require.NoError(t, err)
	assert.True(t, cfg.Vector.Enabled)
	out, err = executeCommand(newRootCommand(), "session", "search", "pooling", "--hybrid", "--include-one-shot", "--json")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"session_id":"session-one"`)
}
