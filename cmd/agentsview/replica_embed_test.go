package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// embedPushReplica is a PostgreSQL-named replica whose pusher exports the
// vector source the push hands it, the way the real vector phase does.
type embedPushReplica struct {
	recipeProvider
	mu      stdsync.Mutex
	events  []string
	opens   int
	pushed  []storage.VectorGenerationInfo
	skipped []bool
}

func (r *embedPushReplica) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *embedPushReplica) setGenerations(gens []storage.VectorGenerationInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gens = gens
}

func (r *embedPushReplica) VectorGenerations(
	context.Context, storage.ReplicaStore,
) ([]storage.VectorGenerationInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gens, nil
}

func (r *embedPushReplica) OpenStore(storage.ReplicaTarget) (storage.ReplicaStore, error) {
	r.mu.Lock()
	r.opens++
	r.mu.Unlock()
	return embedRecipeStore{}, nil
}

func (r *embedPushReplica) NewPusher(
	_ context.Context, _ storage.ReplicaTarget, _ *db.DB, opts storage.PusherOptions,
) (storage.Pusher, error) {
	r.record("connect")
	return &embedSourcePusher{replica: r, source: opts.VectorSource}, nil
}

type embedRecipeStore struct{ storage.ReplicaStore }

func (embedRecipeStore) Close() error { return nil }

type embedSourcePusher struct {
	replica *embedPushReplica
	source  storage.VectorPushSource
}

func (p *embedSourcePusher) EnsureSchema(context.Context) error { return nil }

// PushWithOptions mirrors the vector phase's contract: no exported
// generation skips the phase while sessions still push.
func (p *embedSourcePusher) PushWithOptions(
	ctx context.Context, _ storage.PushOptions, _ func(storage.PushProgress),
) (storage.PushResult, error) {
	p.replica.record("push")
	res := storage.PushResult{SessionsPushed: 1}
	if p.source == nil {
		res.Vectors.Skipped = true
		return res, nil
	}
	export, ok, err := p.source.BeginExport(ctx, nil)
	if err != nil {
		return res, err
	}
	if !ok {
		res.Vectors.Skipped, res.Vectors.SkippedReason = true, "no active local generation"
	} else {
		gen := export.Generation()
		if err := export.Close(); err != nil {
			return res, err
		}
		res.Vectors.GenerationID = 1
		p.replica.mu.Lock()
		p.replica.pushed = append(p.replica.pushed, gen)
		p.replica.mu.Unlock()
	}
	p.replica.mu.Lock()
	p.replica.skipped = append(p.replica.skipped, res.Vectors.Skipped)
	p.replica.mu.Unlock()
	return res, nil
}

func (p *embedSourcePusher) Close() error { return nil }

func embedTarget() storage.ConfiguredReplica {
	return storage.ConfiguredReplica{Target: storage.ReplicaTarget{
		URL: "postgres://hub.example/agentsview", PushVectors: true,
	}}
}

func TestReplicaPushEmbedPushesSessionsThenVectors(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(fmt.Sprintf("adopted=%v", adopted), func(t *testing.T) {
			endpoint := newEmbeddingsStubServer(t, 4)
			t.Cleanup(endpoint.Close)
			cfg := testConfigWithClaudeFixture(t)
			server := config.VectorEmbeddingsServerConfig{
				Endpoint: endpoint.URL + "/v1", BatchSize: 16, Concurrency: 1,
				Timeout: "10s", MaxRetries: 1,
			}
			published := publishedRecipe()
			replica := &embedPushReplica{}
			if adopted {
				cfg.Vector.Embed.BackstopInterval = "24h"
				cfg.DeploymentEmbeddings = &server
				replica.gens = []storage.VectorGenerationInfo{published}
			} else {
				cfg.Vector = vectorTestConfig(cfg.DataDir).Vector
				cfg.Vector.Embeddings = publishedRecipeConfig("published-model")
				cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": server}
			}
			backend := &localArchiveWriteBackend{
				appCfg: cfg, database: dbtest.OpenTestDBAt(t, cfg.DBPath),
				ensurePricing: func(context.Context, *db.DB) error { return nil },
			}
			var result storage.PushResult
			var err error
			captureStdout(t, func() {
				result, err = backend.ReplicaPush(t.Context(), replica, embedTarget(), ReplicaPushConfig{Embed: true}, nil, nil)
			})
			require.NoError(t, err)
			assert.Equal(t, 2, result.SessionsPushed)
			assert.False(t, result.Vectors.Skipped)
			assert.Equal(t, []string{"connect", "push", "push"}, replica.events)
			assert.Equal(t, []bool{true, false}, replica.skipped)
			require.Len(t, replica.pushed, 1)
			assert.Equal(t, published.Fingerprint, replica.pushed[0].Fingerprint)
			if adopted {
				assert.Nil(t, replica.pushed[0].Params)
			} else {
				assert.Equal(t, published.Params, replica.pushed[0].Params)
			}
			assert.NoFileExists(t, filepath.Join(cfg.DataDir, "config.toml"))
		})
	}
	t.Run("build failure follows session push", func(t *testing.T) {
		cfg := testConfigWithClaudeFixture(t)
		cfg.Vector = vectorTestConfig(cfg.DataDir).Vector
		replica := &embedPushReplica{}
		backend := &localArchiveWriteBackend{
			appCfg: cfg, database: dbtest.OpenTestDBAt(t, cfg.DBPath),
			ensurePricing: func(context.Context, *db.DB) error { return nil },
		}
		var result storage.PushResult
		var err error
		captureStdout(t, func() {
			result, err = backend.ReplicaPush(t.Context(), replica, embedTarget(), ReplicaPushConfig{Embed: true}, nil, nil)
		})
		require.ErrorContains(t, err, "building embeddings")
		assert.Equal(t, []string{"connect", "push"}, replica.events)
		assert.Equal(t, 1, result.SessionsPushed)
	})
}

func TestReplicaWatchEmbedAdoptsPublishedRecipe(t *testing.T) {
	endpoint := newEmbeddingsStubServer(t, 4)
	t.Cleanup(endpoint.Close)
	cfg := deploymentRecipeConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	cfg.DeploymentEmbeddings.Endpoint = endpoint.URL + "/v1"
	cfg.Vector.Embed.BackstopInterval = "1s"
	archive := dbtest.OpenTestDBAt(t, cfg.DBPath)
	dbtest.SeedSessionWithMessages(t, archive, "s1", "project",
		[]db.Message{dbtest.UserMsg("s1", 0, "watched content")},
		func(s *db.Session) { s.EndedAt = new("2026-01-01T00:00:00Z") })
	replica := &embedPushReplica{}
	embedder, err := newReplicaEmbedder(cfg, replica, embedTarget(), archive)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, embedder.Close()) })
	pusher := &replicaPusher{
		localSync: func(context.Context) error { return nil },
		connect: func(ctx context.Context) (storage.Pusher, error) {
			return replica.NewPusher(ctx, storage.ReplicaTarget{}, archive, storage.PusherOptions{VectorSource: embedder})
		},
	}
	t.Cleanup(pusher.reset)
	require.NoError(t, pusher.push(t.Context(), reasonStartup, false))
	require.Error(t, embedder.startScheduler(t.Context()))
	assert.Equal(t, []bool{true}, replica.skipped)

	published := publishedRecipe()
	replica.setGenerations([]storage.VectorGenerationInfo{published})
	require.NoError(t, pusher.push(t.Context(), reasonChange, false))
	require.NoError(t, embedder.startScheduler(t.Context()))
	require.NotNil(t, embedder.serving.Scheduler)
	assert.Equal(t, time.Second, embedder.serving.Scheduler.backstop)
	require.Eventually(t, func() bool {
		export, ok, err := embedder.BeginExport(t.Context(), nil)
		if err != nil || !ok {
			return false
		}
		return export.Close() == nil
	}, 10*time.Second, 10*time.Millisecond)

	replica.setGenerations(nil)
	require.NoError(t, pusher.push(t.Context(), reasonInterval, false))
	require.Len(t, replica.pushed, 1)
	assert.Equal(t, published.Fingerprint, replica.pushed[0].Fingerprint)
	assert.Nil(t, replica.pushed[0].Params)
	assert.Equal(t, []bool{true, true, false}, replica.skipped)
	assert.Equal(t, 2, replica.opens, "failed resolution retries, then stays pinned")
}

func TestReplicaWatchEmbedPushesSessionsWhileBuildWaits(t *testing.T) {
	started := make(chan struct{})
	var once stdsync.Once
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(endpoint.Close)
	cfg := deploymentRecipeConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	cfg.DeploymentEmbeddings.Endpoint = endpoint.URL + "/v1"
	cfg.Vector.Embed.BackstopInterval = "1s"
	archive := dbtest.OpenTestDBAt(t, cfg.DBPath)
	dbtest.SeedSessionWithMessages(t, archive, "s1", "project",
		[]db.Message{dbtest.UserMsg("s1", 0, "watched content")},
		func(s *db.Session) { s.EndedAt = new("2026-01-01T00:00:00Z") })
	replica := &embedPushReplica{recipeProvider: recipeProvider{gens: []storage.VectorGenerationInfo{publishedRecipe()}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var push func(context.Context, pushReason, *syncpkg.WatchBatch) error
	var shutdownStart time.Time
	backend := &localArchiveWriteBackend{
		appCfg: cfg, database: archive,
		ensurePricing: func(context.Context, *db.DB) error { return nil },
		watchHooks: &archivePushWatchHooks{
			newLoop: func(label string, debounce, interval time.Duration, work func(context.Context, pushReason, *syncpkg.WatchBatch) error) (*pushLoop, func()) {
				push = work
				loop, ticker := newPushLoopWithLabel(label, debounce, interval, work)
				return loop, ticker.Stop
			},
			replicaStartupSync: func(context.Context, *syncpkg.Engine, bool) (bool, error) { return false, nil },
			startWatcher: func(config.Config, *syncpkg.Engine, syncpkg.WatchCallback, syncpkg.WatcherOptions) (func(), func(), []string) {
				return func() {}, func() {
					select {
					case <-started:
					case <-time.After(10 * time.Second):
						require.FailNow(t, "build never reached endpoint")
					}
					require.NoError(t, push(ctx, reasonChange, nil))
					require.NoError(t, push(ctx, reasonInterval, nil))
					assert.Equal(t, []string{"connect", "push", "push", "push"}, replica.events)
					shutdownStart = time.Now()
					cancel()
				}, nil
			},
		},
	}
	captureStdout(t, func() {
		require.NoError(t, backend.ReplicaPushWatch(ctx, replica, embedTarget(), ReplicaPushConfig{Embed: true}, nil, nil, time.Hour, time.Hour))
	})
	assert.Less(t, time.Since(shutdownStart), time.Second, "shutdown cancels the rate-limited build")
}

func TestReplicaPushEmbedUsesLocalWriterWithoutDaemon(t *testing.T) {
	setup := func(t *testing.T) (string, *int) {
		t.Helper()
		dataDir := t.TempDir()
		isolateDeploymentEnv(t)
		t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
		t.Setenv("AGENTSVIEW_NO_DAEMON", "")
		clearConfiguredAgentEnvVars(t)
		isolateDefaultAgentDirs(t, dataDir)
		restoreTestLogger(t)
		starts := 0
		stubStartBackgroundServeForTransport(t, func(
			context.Context, *config.Config, time.Duration, bool,
		) (*DaemonRuntime, error) {
			starts++
			return nil, errors.New("pg push --embed must not start a daemon")
		})
		return dataDir, &starts
	}

	t.Run("push connects through the local writer", func(t *testing.T) {
		dataDir, starts := setup(t)
		endpoint := newEmbeddingsStubServer(t, 3)
		t.Cleanup(endpoint.Close)
		writeTestConfig(t, dataDir, fmt.Sprintf(`
[pg]
url = "postgres://agentsview@127.0.0.1:1/agentsview?connect_timeout=1"
allow_insecure = true

[vector]
enabled = true

[vector.embeddings]
model = "test-model"
dimension = 3

[vector.embeddings.servers.local]
endpoint = %q
`, endpoint.URL+"/v1"))
		dbtest.SeedSessionWithMessages(t, dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db")),
			"s1", "project", []db.Message{dbtest.UserMsg("s1", 0, "local content")},
			func(s *db.Session) { s.EndedAt = new("2026-01-01T00:00:00Z") })

		var err error
		captureStdout(t, func() {
			err = runReplicaPush(pgReplica{}, ReplicaPushConfig{Embed: true}, "")
		})
		require.Error(t, err, "the unreachable hub fails before building")
		require.NotErrorIs(t, err, errEmbedNeedsLocalArchive)
		assert.Zero(t, *starts, "no daemon is started")
		assert.Nil(t, FindDaemonRuntime(dataDir, ""), "no daemon runtime file appears")
	})

	t.Run("watch resolves the local writer", func(t *testing.T) {
		dataDir, starts := setup(t)
		writeTestConfig(t, dataDir, `
[pg]
url = "postgres://agentsview@127.0.0.1:1/agentsview"
allow_insecure = true
`)
		// Neither [vector] nor a deployment endpoint: the local writer's
		// embedder rejects the request before the watch loop starts.
		err := runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{Embed: true}, "")
		require.ErrorContains(t, err, "AGENTSVIEW_EMBEDDINGS_ENDPOINT")
		require.NotErrorIs(t, err, errEmbedNeedsLocalArchive)
		assert.Zero(t, *starts)
		assert.Nil(t, FindDaemonRuntime(dataDir, ""))
	})

	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale live daemon watch=%v", watch), func(t *testing.T) {
			dataDir, starts := setup(t)
			writeTestConfig(t, dataDir, `
[pg]
url = "postgres://agentsview@127.0.0.1:1/agentsview"
allow_insecure = true
`)
			setTestVersion(t, "v1.1.0-2-g123456")
			pushed := 0
			daemon := pushRuntimeServer(t, "/api/v1/push/pg", func(w http.ResponseWriter, _ *http.Request) {
				pushed++
				writeTestJSON(t, w, storage.PushResult{})
			})
			host, port := splitTestServerURL(t, daemon.URL)
			writeDaemonRuntimeForTest(t, dataDir, host, port, "v1.1.0-3-gabcdef", false)

			var err error
			if watch {
				err = runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{Embed: true}, "")
			} else {
				captureStdout(t, func() {
					err = runReplicaPush(pgReplica{}, ReplicaPushConfig{Embed: true}, "")
				})
			}
			require.ErrorIs(t, err, errEmbedNeedsLocalArchive)
			assert.Zero(t, *starts, "a stale daemon is rejected, not replaced")
			assert.Zero(t, pushed)
		})
	}
}

func TestReplicaPushEmbedRejectedThroughDaemon(t *testing.T) {
	requests := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)
	backend := newDaemonArchiveWriteBackendForTest(config.Config{DataDir: t.TempDir()}, daemon.URL)

	_, err := backend.ReplicaPush(t.Context(), pgReplica{}, embedTarget(),
		ReplicaPushConfig{Embed: true}, nil, nil)
	require.ErrorIs(t, err, errEmbedNeedsLocalArchive)
	assert.Contains(t, err.Error(), "agentsview daemon stop")
	err = backend.ReplicaPushWatch(t.Context(), pgReplica{}, embedTarget(),
		ReplicaPushConfig{Embed: true}, nil, nil, time.Second, time.Second)
	require.ErrorIs(t, err, errEmbedNeedsLocalArchive)
	assert.Zero(t, requests)
}

func TestReplicaPushEmbedFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"pg", "push", "--embed", "--no-vectors"}, want: "--embed cannot be combined with --no-vectors"},
		{args: []string{"pg", "push", "--embed", "--all"}, want: "--embed cannot be combined with --all"},
		{args: []string{"pg", "push", "--embed", "--all", "--watch"}, want: "--embed cannot be combined with --all"},
		{args: []string{"clickhouse", "push", "--embed"}, want: "unknown flag: --embed"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			isolateDeploymentEnv(t)
			root := newRootCommand()
			root.SetArgs(tc.args)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			require.ErrorContains(t, root.ExecuteContext(t.Context()), tc.want)
		})
	}

	enabled := vectorTestConfig(t.TempDir())
	disabled := enabled
	disabled.Vector.Enabled = false
	usageOnly := enabled
	usageOnly.ArchiveContent = config.ArchiveContentUsage
	noTargetVectors := embedTarget()
	noTargetVectors.Target.PushVectors = false
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		target storage.ConfiguredReplica
		want   string
	}{
		{name: "disabled vector without endpoint", cfg: disabled, want: "[vector] enabled"},
		{name: "usage-only archive", cfg: usageOnly, want: "usage-only"},
		{name: "push_vectors false", cfg: enabled, target: noTargetVectors, want: "push_vectors = false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target
			if target.Target.URL == "" {
				target = embedTarget()
			}
			_, err := newReplicaEmbedder(tc.cfg, pgReplica{}, target, nil)
			require.ErrorContains(t, err, tc.want)
			if tc.name == "disabled vector without endpoint" {
				assert.Contains(t, err.Error(), "AGENTSVIEW_EMBEDDINGS_ENDPOINT")
			}
		})
	}
}
