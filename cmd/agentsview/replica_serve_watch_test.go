package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
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
	for _, miss := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial_miss=%t", miss), func(t *testing.T) {
			testDataDir(t)
			cfg, _, err := loadPGServeConfigForTest(t)
			require.NoError(t, err)
			cfg.Vector = vectorTestConfig(cfg.DataDir).Vector
			cfg.PG.URL = "postgres://user@127.0.0.1:1/archive"
			cfg.PG.AllowInsecure = true
			provider := &serveWatchVectorProvider{miss: miss}
			store := &postgres.Store{}
			ctx, cancel := context.WithCancel(t.Context())
			startup := replicaServeStartup{cfg: cfg, ctx: ctx, cleanup: cancel}
			watch := &ReplicaPushConfig{}
			require.NoError(t, attachReplicaServeWatch(&startup, cancel, provider, store, watch))
			t.Cleanup(startup.cleanup)
			if miss {
				watch.OnPushSuccess(ctx)
				assert.False(t, store.HasSemantic())
				provider.miss = false
			}
			watch.OnPushSuccess(ctx)
			require.True(t, store.HasSemantic())
			opens := provider.opens
			watch.OnPushSuccess(ctx)
			assert.Equal(t, opens, provider.opens, "later pushes must reuse the enabled searcher")
		})
	}
}

func TestReplicaServeWatchFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--embed"}, "requires --push-watch"},
		{[]string{"--debounce=1s"}, "requires --push-watch"},
		{[]string{"--interval=1m"}, "requires --push-watch"},
		{[]string{"--push-watch", "--no-sync"}, "cannot be combined"},
		{nil, ""},
		{[]string{"--push-watch", "--embed", "--debounce=1s", "--interval=1m"}, ""},
	} {
		t.Run(fmt.Sprint(tc.args), func(t *testing.T) {
			cmd := newReplicaServeCommand(pgReplica{})
			require.NoError(t, cmd.Flags().Parse(tc.args))
			err := validateReplicaServeWatchFlags(cmd)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			watch, err := loadReplicaServeWatch(cmd, config.Config{})
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

func TestReplicaServeWatchHostedRejected(t *testing.T) {
	for _, raw := range []config.PGConfig{{RawTenant: "tenant"}, {RawDerivation: true}} {
		cmd := newReplicaServeCommand(pgReplica{})
		require.NoError(t, cmd.Flags().Parse([]string{"--push-watch"}))
		cfg := config.Config{PG: raw}
		_, err := loadReplicaServeWatch(cmd, cfg)
		require.ErrorContains(t, err, "raw_tenant/raw_derivation")
	}
}

func TestReplicaServeWatchOwnership(t *testing.T) {
	for _, embed := range []bool{false, true} {
		t.Run(map[bool]string{false: "without embed", true: "with embed"}[embed], func(t *testing.T) {
			testDataDir(t)
			t.Setenv("AGENTSVIEW_PG_URL", "postgres://user@127.0.0.1:1/archive")
			cfg, _, err := loadPGServeConfigForTest(t)
			require.NoError(t, err)
			cfg.PG.AllowInsecure = true
			stubStartBackgroundServeForTransport(t, func(context.Context, *config.Config, time.Duration, bool) (*DaemonRuntime, error) {
				assert.Fail(t, "unexpected daemon launch")
				return nil, errors.New("unexpected daemon launch")
			})
			_, cleanup, err := prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.NoError(t, err)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
			require.ErrorContains(t, err, "already locked")
			cleanup()
			database, lock, err := openWriteDB(t.Context(), cfg)
			require.NoError(t, err)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.Error(t, err, "a competing archive writer must be refused")
			closeWriteDB(database, lock)
			daemon := pushRuntimeServer(t, "/api/v1/push/pg", func(http.ResponseWriter, *http.Request) { assert.Fail(t, "must not delegate") })
			host, port := splitTestServerURL(t, daemon.URL)
			writeDaemonRuntimeForTest(t, cfg.DataDir, host, port, version, false)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.ErrorContains(t, err, "requires the local archive writer")
		})
	}
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
				require.Len(t, startup.workerResult, 1, "worker did not report completion")
				if early {
					require.ErrorContains(t, <-startup.workerResult, "watch failed")
				} else {
					require.NoError(t, <-startup.workerResult)
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

func TestReplicaServeWatchLoadedArchive(t *testing.T) {
	dir := testDataDir(t)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id), 0o600))
	root := filepath.Join(dir, "claude")
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, "explicit.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "identity").String()), 0o600))
	home := filepath.Join(dir, "codex")
	sessions := filepath.Join(dir, "archive", "sessions")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	requireSymlinkOrSkip(t, sessions, filepath.Join(home, "sessions"))
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "rollout-2026-01-01T00-00-00-"+uuid+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", uuid, "/workspace/project", "codex_cli_rs").AddCodexMessage("2026-01-01T00:00:01Z", "user", "metadata").String()), 0o600))
	writeTestConfig(t, dir, fmt.Sprintf(`[[session_sources]]
agent = "claude"
dir = %q
machine = %q
[[session_sources]]
agent = "codex"
dir = %q
`, root, "mounted-workstation", filepath.Join(home, "sessions")))
	cfg, _, err := loadPGServeConfigForTest(t, "--push-watch")
	require.NoError(t, err)
	database, lock, err := openWriteDB(t.Context(), cfg)
	require.NoError(t, err)
	defer closeWriteDB(database, lock)
	require.NoError(t, os.WriteFile(filepath.Join(home, "session_index.jsonl"), fmt.Appendf(nil, `{"id":%q,"thread_name":"Original title","updated_at":"2026-01-01T01:00:00Z"}`+"\n", uuid), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := false
	backend := localArchiveWriteBackend{appCfg: cfg, database: database, watchHooks: &archivePushWatchHooks{
		startWatcher: func(_ config.Config, engine *syncpkg.Engine, _ syncpkg.WatchCallback, _ syncpkg.WatcherOptions) (func(), func(), []string) {
			observed = true
			assert.Equal(t, id, engine.Machine())
			assert.Equal(t, cfg.ResolveDirs(parser.AgentCodex), engine.ReconciliationRootsForAgent("codex"))
			metadata, ok := engine.ProviderStatHasher(parser.AgentCodex).(parser.CodexMetadataProvider)
			require.True(t, ok)
			assert.Equal(t, []string{filepath.Join(cfg.ProviderMetadata[parser.AgentCodex][cfg.SessionSources[1].Dir][0], "session_index.jsonl")}, metadata.Metadata().IndexFiles(cfg.SessionSources[1].Dir))
			return func() {}, func() {}, nil
		},
		startupSync: func(ctx context.Context, engine *syncpkg.Engine, _ bool) (bool, error) {
			require.False(t, engine.SyncAll(ctx, nil).Aborted)
			cancel()
			return false, nil
		},
	}}
	require.NoError(t, backend.ReplicaPushWatch(ctx, pgReplica{}, storage.ConfiguredReplica{}, ReplicaPushConfig{}, nil, nil, time.Hour, time.Hour))
	assert.True(t, observed)
	session, err := database.GetSession(t.Context(), "explicit")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "mounted-workstation", session.Machine)
}
