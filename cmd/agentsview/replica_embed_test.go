package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// embedEndpoint is an OpenAI-compatible embeddings stub that records each
// request's model and inputs.
type embedEndpoint struct {
	*httptest.Server
	mu       stdsync.Mutex
	models   []string
	inputs   []string
	requests int
	onCall   func()
}

func newEmbedEndpoint(t *testing.T, dim int) *embedEndpoint {
	t.Helper()
	e := &embedEndpoint{}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.requests++
		e.models = append(e.models, body.Model)
		e.inputs = append(e.inputs, body.Input...)
		onCall := e.onCall
		e.mu.Unlock()
		if onCall != nil {
			onCall()
		}
		data := make([]map[string]any, len(body.Input))
		for i := range data {
			vec := make([]float32, dim)
			vec[0] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *embedEndpoint) snapshot() (requests int, models, inputs []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests, append([]string(nil), e.models...), append([]string(nil), e.inputs...)
}

func (e *embedEndpoint) server() config.VectorEmbeddingsServerConfig {
	return config.VectorEmbeddingsServerConfig{
		Endpoint: e.URL + "/v1", BatchSize: 16, Concurrency: 1,
		Timeout: "10s", MaxRetries: 1,
	}
}

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

// recipeWithDimension is publishedRecipe sized for the stub endpoint.
func recipeWithDimension(dim int) (config.VectorEmbeddingsConfig, storage.VectorGenerationInfo) {
	c := publishedRecipeConfig("published-model")
	c.Dimension = dim
	gen := vectorGeneration(c)
	return c, storage.VectorGenerationInfo{
		Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: dim, Params: gen.Params,
	}
}

func TestReplicaPushEmbedBuildsBeforePush(t *testing.T) {
	t.Run("local [vector]", func(t *testing.T) {
		endpoint := newEmbedEndpoint(t, 3)
		cfg := testConfigWithClaudeFixture(t)
		vec := vectorTestConfig(cfg.DataDir).Vector
		vec.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"local": endpoint.server()}
		cfg.Vector = vec
		replica := &embedPushReplica{}
		endpoint.onCall = func() { replica.record("embed") }

		backend := &localArchiveWriteBackend{
			appCfg:        cfg,
			database:      dbtest.OpenTestDBAt(t, cfg.DBPath),
			ensurePricing: func(context.Context, *db.DB) error { return nil },
		}
		var result storage.PushResult
		var err error
		captureStdout(t, func() {
			result, err = backend.ReplicaPush(t.Context(), replica, embedTarget(),
				ReplicaPushConfig{Embed: true}, nil, nil)
		})
		require.NoError(t, err)
		assert.False(t, result.Vectors.Skipped)

		_, models, inputs := endpoint.snapshot()
		assert.Contains(t, inputs, "hello", "the build embeds sessions this push synced")
		assert.Equal(t, "test-model", models[0])
		require.NotEmpty(t, replica.events)
		assert.Equal(t, "embed", replica.events[0], "the build runs before connecting")
		assert.Equal(t, []string{"connect", "push"}, replica.events[len(replica.events)-2:])
		require.Len(t, replica.pushed, 1)
		want := vectorGeneration(cfg.Vector.Embeddings)
		assert.Equal(t, want.Fingerprint(), replica.pushed[0].Fingerprint)
		assert.Equal(t, want.Params, replica.pushed[0].Params)
	})

	t.Run("adopted recipe", func(t *testing.T) {
		endpoint := newEmbedEndpoint(t, 4)
		cfg := testConfigWithClaudeFixture(t)
		cfg.Vector.Embed.BackstopInterval = "24h"
		server := endpoint.server()
		cfg.DeploymentEmbeddings = &server
		_, published := recipeWithDimension(4)
		replica := &embedPushReplica{}
		replica.gens = []storage.VectorGenerationInfo{published}

		backend := &localArchiveWriteBackend{
			appCfg:        cfg,
			database:      dbtest.OpenTestDBAt(t, cfg.DBPath),
			ensurePricing: func(context.Context, *db.DB) error { return nil },
		}
		var err error
		captureStdout(t, func() {
			_, err = backend.ReplicaPush(t.Context(), replica, embedTarget(),
				ReplicaPushConfig{Embed: true}, nil, nil)
		})
		require.NoError(t, err)
		_, models, inputs := endpoint.snapshot()
		require.NotEmpty(t, models)
		assert.Equal(t, "published-model", models[0])
		assert.Contains(t, inputs, "passage: hello", "documents use the adopted prefix")
		require.Len(t, replica.pushed, 1)
		assert.Equal(t, published.Fingerprint, replica.pushed[0].Fingerprint)
		assert.Equal(t, published.Params, replica.pushed[0].Params)
		assert.NoFileExists(t, filepath.Join(cfg.DataDir, "config.toml"),
			"adoption never writes config.toml")
	})

	t.Run("one-shot build failure fails the push", func(t *testing.T) {
		cfg := testConfigWithClaudeFixture(t)
		cfg.Vector = vectorTestConfig(cfg.DataDir).Vector // endpoint 127.0.0.1:1 refuses
		replica := &embedPushReplica{}
		backend := &localArchiveWriteBackend{
			appCfg:        cfg,
			database:      dbtest.OpenTestDBAt(t, cfg.DBPath),
			ensurePricing: func(context.Context, *db.DB) error { return nil },
		}
		var err error
		captureStdout(t, func() {
			_, err = backend.ReplicaPush(t.Context(), replica, embedTarget(),
				ReplicaPushConfig{Embed: true}, nil, nil)
		})
		require.ErrorContains(t, err, "embedding before push")
		assert.NotContains(t, replica.events, "push")
	})
}

func TestReplicaWatchEmbedsBeforeEachPush(t *testing.T) {
	endpoint := newEmbedEndpoint(t, 4)
	dataDir := t.TempDir()
	cfg := config.Config{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
		Vector:  config.VectorConfig{Embed: config.VectorEmbedConfig{BackstopInterval: "24h"}},
	}
	server := endpoint.server()
	cfg.DeploymentEmbeddings = &server
	archive := dbtest.OpenTestDBAt(t, cfg.DBPath)
	dbtest.SeedSessionWithMessages(t, archive, "s1", "project",
		[]db.Message{dbtest.UserMsg("s1", 0, "watched content")},
		func(s *db.Session) { s.EndedAt = new("2026-01-01T00:00:00Z") })

	replica := &embedPushReplica{}
	embedder, err := newReplicaEmbedder(cfg, replica, embedTarget(), ReplicaPushConfig{Embed: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, embedder.Close()) })
	endpoint.onCall = func() { replica.record("embed") }

	logs := captureLogOutput(t)
	pusher := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync: func(context.Context) error {
			replica.record("sync")
			return nil
		},
		beforePush: embedder.prepare,
		connect: func(ctx context.Context) (storage.Pusher, error) {
			return replica.NewPusher(ctx, storage.ReplicaTarget{}, nil,
				storage.PusherOptions{VectorSource: embedder})
		},
	}

	// Cycle 1: nothing published yet, so the recipe cannot be resolved.
	// Sessions still push; the vector phase is skipped.
	require.NoError(t, pusher.push(t.Context(), reasonStartup, false))
	assert.Equal(t, []string{"sync", "connect", "push"}, replica.events)
	assert.Equal(t, []bool{true}, replica.skipped)
	assert.Contains(t, logs.String(), "embedding before push")
	assert.True(t, pusher.vectorReconcileNeeded)

	// Cycle 2: a workstation published a recipe; it is adopted, built and
	// pushed.
	_, published := recipeWithDimension(4)
	replica.setGenerations([]storage.VectorGenerationInfo{published})
	replica.events = nil
	require.NoError(t, pusher.push(t.Context(), reasonChange, false))
	require.NotEmpty(t, replica.events)
	assert.Equal(t, "sync", replica.events[0])
	assert.Equal(t, "embed", replica.events[1], "the build runs before the push")
	assert.Equal(t, "push", replica.events[len(replica.events)-1])
	require.Len(t, replica.pushed, 1)
	assert.Equal(t, published.Fingerprint, replica.pushed[0].Fingerprint)
	assert.Equal(t, []bool{true, false}, replica.skipped)

	// Cycle 3: the recipe stays pinned even when the replica changes.
	replica.setGenerations(nil)
	dbtest.SeedSessionWithMessages(t, archive, "s2", "project",
		[]db.Message{dbtest.UserMsg("s2", 0, "second content")},
		func(s *db.Session) { s.EndedAt = new("2026-01-02T00:00:00Z") })
	replica.events = nil
	require.NoError(t, pusher.push(t.Context(), reasonChange, false))
	assert.Equal(t, "embed", replica.events[1])
	require.Len(t, replica.pushed, 2)
	assert.Equal(t, published.Fingerprint, replica.pushed[1].Fingerprint)
	assert.Equal(t, 2, replica.opens, "resolution retried once, then pinned")
	_, _, inputs := endpoint.snapshot()
	assert.Contains(t, inputs, "passage: second content")
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

	t.Run("push builds locally", func(t *testing.T) {
		dataDir, starts := setup(t)
		endpoint := newEmbedEndpoint(t, 3)
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
		require.Error(t, err, "the unreachable hub fails only after the local build")
		require.NotErrorIs(t, err, errEmbedNeedsLocalArchive)
		assert.Zero(t, *starts, "no daemon is started")
		requests, _, inputs := endpoint.snapshot()
		assert.Positive(t, requests, "embeddings were built in this process")
		assert.Contains(t, inputs, "local content")
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
		name    string
		cfg     config.Config
		backend storage.Replica
		target  storage.ConfiguredReplica
		push    ReplicaPushConfig
		want    string
	}{
		{name: "disabled vector without endpoint", cfg: disabled, want: "[vector] enabled"},
		{name: "usage-only archive", cfg: usageOnly, want: "usage-only"},
		{name: "push_vectors false", cfg: enabled, target: noTargetVectors, want: "push_vectors = false"},
		{name: "no-vectors", cfg: enabled, push: ReplicaPushConfig{NoVectors: true}, want: "--no-vectors"},
		{name: "other backend", cfg: enabled, backend: clickhouse.Backend{}, want: "only by pg push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := tc.backend
			if backend == nil {
				backend = pgReplica{}
			}
			target := tc.target
			if target.Target.URL == "" {
				target = embedTarget()
			}
			tc.push.Embed = true
			_, err := newReplicaEmbedder(tc.cfg, backend, target, tc.push)
			require.ErrorContains(t, err, tc.want)
			if tc.name == "disabled vector without endpoint" {
				assert.Contains(t, err.Error(), "AGENTSVIEW_EMBEDDINGS_ENDPOINT")
			}
		})
	}
}
