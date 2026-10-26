package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

type serveWatchVectorProvider struct {
	pgReplica
	opens int
	miss  bool
}

func (p *serveWatchVectorProvider) OpenVectorSearcher(context.Context, storage.ReplicaStore, storage.VectorGenerationInfo, int, storage.VectorQueryEncoder) (db.VectorSearcher, string, error) {
	p.opens++
	if p.miss {
		return nil, "generation not published", nil
	}
	return struct{ db.VectorSearcher }{}, "", nil
}

func (*serveWatchVectorProvider) VectorGenerations(context.Context, storage.ReplicaStore) ([]storage.VectorGenerationInfo, error) {
	return nil, nil
}

func TestReplicaServeSemanticRefreshStopsOnceEnabled(t *testing.T) {
	testDataDir(t)
	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err)
	cfg.Vector = vectorTestConfig(cfg.DataDir).Vector
	cfg.PG.URL = "postgres://user@127.0.0.1:1/archive"
	cfg.PG.AllowInsecure = true
	provider := &serveWatchVectorProvider{miss: true}
	store := &postgres.Store{}
	ctx, cancel := context.WithCancel(t.Context())
	startup := replicaServeStartup{cfg: cfg, ctx: ctx, cleanup: cancel}
	watch := &ReplicaPushConfig{}
	require.NoError(t, attachReplicaServeWatch(&startup, cancel, provider, store, watch))
	t.Cleanup(startup.cleanup)
	watch.OnPushSuccess(ctx)
	assert.False(t, store.HasSemantic())
	provider.miss = false
	watch.OnPushSuccess(ctx)
	require.True(t, store.HasSemantic())
	opens := provider.opens
	watch.OnPushSuccess(ctx)
	assert.Equal(t, opens, provider.opens, "later pushes must reuse the enabled searcher")
}

func TestReplicaServeWatchFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		raw  config.PGConfig
	}{
		{[]string{"--embed"}, "requires --push-watch", config.PGConfig{}},
		{[]string{"--debounce=1s"}, "requires --push-watch", config.PGConfig{}},
		{[]string{"--interval=1m"}, "requires --push-watch", config.PGConfig{}},
		{[]string{"--push-watch", "--no-sync"}, "cannot be combined", config.PGConfig{}},
		{nil, "", config.PGConfig{}},
		{[]string{"--push-watch", "--embed", "--debounce=1s", "--interval=1m"}, "", config.PGConfig{}},
		{[]string{"--push-watch"}, "raw_tenant/raw_derivation", config.PGConfig{RawTenant: "tenant"}},
		{[]string{"--push-watch"}, "raw_tenant/raw_derivation", config.PGConfig{RawDerivation: true}},
	} {
		t.Run(fmt.Sprint(tc.args, tc.raw), func(t *testing.T) {
			cmd := newReplicaServeCommand(pgReplica{})
			require.NoError(t, cmd.Flags().Parse(tc.args))
			var watch *ReplicaPushConfig
			err := validateReplicaServeWatchFlags(cmd)
			if err == nil {
				watch, err = loadReplicaServeWatch(cmd, config.Config{PG: tc.raw})
			}
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			if len(tc.args) == 0 {
				assert.Nil(t, watch)
				return
			}
			require.NotNil(t, watch)
			assert.Equal(t, ReplicaPushConfig{Embed: true, Debounce: time.Second, Interval: time.Minute}, *watch)
		})
	}
}

func TestReplicaServeWatchOwnership(t *testing.T) {
	testDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://user@127.0.0.1:1/archive")
	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err)
	cfg.PG.AllowInsecure = true
	stubStartBackgroundServeForTransport(t, func(context.Context, *config.Config, time.Duration, bool) (*DaemonRuntime, error) {
		assert.Fail(t, "unexpected daemon launch")
		return nil, errors.New("unexpected daemon launch")
	})
	_, cleanup, err := prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
	require.NoError(t, err)
	cleanup = sync.OnceFunc(cleanup)
	t.Cleanup(cleanup)
	_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
	require.ErrorContains(t, err, "already locked")
	_, _, err = openWriteDB(t.Context(), cfg)
	var held writeOwnerLockHeldError
	require.ErrorAs(t, err, &held)
	cleanup()
	daemon := pushRuntimeServer(t, "/api/v1/push/pg", func(http.ResponseWriter, *http.Request) { assert.Fail(t, "must not delegate") })
	host, port := splitTestServerURL(t, daemon.URL)
	writeDaemonRuntimeForTest(t, cfg.DataDir, host, port, version, false)
	_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
	require.ErrorContains(t, err, "requires the local archive writer")
}

func TestReplicaServeWatchWorkerLifecycle(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(fmt.Sprintf("fatal=%t", early), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				entered, release, exited, cleaned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 2)
				releaseWorker := sync.OnceFunc(func() { close(release) })
				defer releaseWorker()
				startup := replicaServeStartup{ctx: ctx, cleanup: func() {
					assert.Len(t, exited, 1, "base cleanup preceded worker exit")
					assert.Len(t, cleaned, 1, "base cleanup preceded watch cleanup")
					cleaned <- struct{}{}
				}}
				joinReplicaServeWorker(&startup, cancel, func() error {
					entered <- struct{}{}
					if !early {
						<-ctx.Done()
					}
					<-release
					exited <- struct{}{}
					if early {
						return errors.New("watch failed")
					}
					return nil
				}, func() { cleaned <- struct{}{} })
				startup.startWorker()
				synctest.Wait()
				require.Len(t, entered, 1, "worker did not start")
				if !early {
					cancel()
					go startup.cleanup()
					synctest.Wait()
					assert.Empty(t, cleaned, "cleanup did not join worker")
				}
				releaseWorker()
				synctest.Wait()
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				if early {
					require.ErrorContains(t, startup.joinWorker(), "watch failed")
				} else {
					require.NoError(t, startup.joinWorker())
				}
				if early {
					startup.cleanup()
				}
				require.Len(t, cleaned, 2, "cleanup did not finish")
			})
		})
	}
}

func TestReplicaServeWatchFailureStopsHTTP(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), Host: "127.0.0.1", Port: 0, NoBrowser: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := serveRuntimeOptions{Mode: "pg-serve"}
	cfg, err := prepareServeRuntimeConfig(ctx, cfg, opts)
	require.NoError(t, err)
	startup := replicaServeStartup{cfg: cfg, ctx: ctx, rtOpts: opts, srv: server.New(cfg, nil, nil), cleanup: cancel}
	url := fmt.Sprintf("http://%s:%d/", cfg.Host, cfg.Port)
	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	joinReplicaServeWorker(&startup, cancel, func() error {
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		_ = response.Body.Close()
		return errors.New("fatal ingestion failure")
	}, func() {})
	require.ErrorContains(t, runPreparedReplicaServe("pg", startup), "fatal ingestion failure")
	response, err := client.Do(request.Clone(t.Context()))
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err, "HTTP listener must close before serve returns")
}

func TestReplicaServeWatchShutdownReturnsWorkerError(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), Host: "127.0.0.1", Port: 0, NoBrowser: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := serveRuntimeOptions{Mode: "pg-serve"}
	cfg, err := prepareServeRuntimeConfig(ctx, cfg, opts)
	require.NoError(t, err)
	startup := replicaServeStartup{cfg: cfg, ctx: ctx, rtOpts: opts, srv: server.New(cfg, nil, nil), cleanup: cancel}
	release := make(chan struct{})
	stop := sync.OnceFunc(func() {
		cancel()
		close(release)
	})
	want := errors.New("final push failed")
	joinReplicaServeWorker(&startup, stop, func() error {
		// Finish only when serve joins the worker, after its context is canceled.
		<-release
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
		return want
	}, func() {})
	start := startup.startWorker
	startup.startWorker = func() {
		start()
		cancel()
	}
	require.ErrorIs(t, runPreparedReplicaServe("pg", startup), want)
}
