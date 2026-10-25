package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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
		t.Run(filepath.Base(tc.want)+strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := newReplicaServeCommand(pgReplica{})
			require.NoError(t, cmd.Flags().Parse(tc.args))
			err := validateReplicaServeWatchFlags(cmd)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
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
			cfg, _, err := loadPGServeConfigForTest(t)
			require.NoError(t, err)
			cfg.PG.URL = "postgres://user@127.0.0.1:1/archive"
			cfg.PG.AllowInsecure = true
			starts := 0
			stubStartBackgroundServeForTransport(t, func(context.Context, *config.Config, time.Duration, bool) (*DaemonRuntime, error) {
				starts++
				return nil, errors.New("unexpected daemon launch")
			})
			_, cleanup, err := prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.NoError(t, err)
			assert.Zero(t, starts)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
			require.ErrorContains(t, err, "already locked")
			cleanup()
			_, cleanup, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
			require.NoError(t, err, "cleanup must release writer and watch ownership")
			cleanup()
			database, lock, err := openWriteDB(t.Context(), cfg)
			require.NoError(t, err)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.Error(t, err, "a competing archive writer must be refused")
			closeWriteDB(database, lock)
			daemon := pushRuntimeServer(t, "/api/v1/push/pg", func(http.ResponseWriter, *http.Request) { t.Error("must not delegate") })
			host, port := splitTestServerURL(t, daemon.URL)
			writeDaemonRuntimeForTest(t, cfg.DataDir, host, port, version, false)
			_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{Embed: embed}, "", true)
			require.ErrorContains(t, err, "requires the local archive writer")
		})
	}
}

func TestReplicaServeWatchWorkerLifecycle(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel joins", true: "fatal cancels serve"}[early], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			entered := make(chan struct{})
			joined := false
			cleaned := false
			startup := replicaServeStartup{ctx: ctx, cleanup: func() { assert.True(t, cleaned); assert.True(t, joined) }}
			joinReplicaServeWorker(&startup, cancel, func() error {
				close(entered)
				if !early {
					<-ctx.Done()
				}
				joined = true
				if early {
					return errors.New("watch failed")
				}
				return nil
			}, func() { cleaned = true })
			startup.startWorker()
			<-entered
			if !early {
				cancel()
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				require.FailNow(t, "worker did not cancel serve")
			}
			startup.cleanup()
			if early {
				require.ErrorContains(t, <-startup.workerResult, "watch failed")
			}
		})
	}
}

func TestReplicaServeWatchReadinessFailureReleasesOwnership(t *testing.T) {
	testDataDir(t)
	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err)
	cfg.PG.URL = "postgres://user@127.0.0.1:1/archive"
	cfg.PG.AllowInsecure = true
	cfg.Host = "invalid host"
	ctx, cancel := context.WithCancel(t.Context())
	startup := replicaServeStartup{cfg: cfg, ctx: ctx, srv: server.New(cfg, nil, nil), cleanup: cancel}
	require.NoError(t, attachReplicaServeWatch(&startup, cancel, pgReplica{}, nil, &replicaServeWatch{}))
	require.Error(t, runPreparedReplicaServe("pg", startup))
	lock := flock.New(filepath.Join(cfg.DataDir, "pg-watch.lock"))
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, lock.Unlock())
}

func TestReplicaServeWatchFailureStopsHTTP(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), Host: "127.0.0.1", Port: 0, NoBrowser: true}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := serveRuntimeOptions{Mode: "pg-serve"}
	cfg, err := prepareServeRuntimeConfig(ctx, cfg, opts)
	require.NoError(t, err)
	startup := replicaServeStartup{cfg: cfg, ctx: ctx, rtOpts: opts, srv: server.New(cfg, nil, nil), cleanup: cancel}
	joinReplicaServeWorker(&startup, cancel, func() error { return errors.New("fatal ingestion failure") }, func() {})
	require.ErrorContains(t, runPreparedReplicaServe("pg", startup), "fatal ingestion failure")
}

func TestReplicaServeWatchLoadedArchive(t *testing.T) {
	dir := testDataDir(t)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, t.TempDir())
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id), 0o600))
	alias := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Symlink(dir, alias))
	t.Setenv("AGENTSVIEW_DATA_DIR", alias)
	root := filepath.Join(dir, "claude")
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(project, "explicit.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-01T00:00:00Z", "identity").String()), 0o600))
	home := filepath.Join(dir, "codex")
	sessions := filepath.Join(dir, "archive", "sessions")
	require.NoError(t, os.MkdirAll(home, 0o700))
	require.NoError(t, os.MkdirAll(sessions, 0o700))
	require.NoError(t, os.Symlink(sessions, filepath.Join(home, "sessions")))
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	require.NoError(t, os.WriteFile(filepath.Join(sessions, "rollout-2026-01-01T00-00-00-"+uuid+".jsonl"), []byte(testjsonl.NewSessionBuilder().AddCodexMeta("2026-01-01T00:00:00Z", uuid, "/workspace/project", "codex_cli_rs").AddCodexMessage("2026-01-01T00:00:01Z", "user", "metadata").String()), 0o600))
	writeTestConfig(t, dir, fmt.Sprintf(`local_machine_name = "Collector"
[[session_sources]]
agent = "claude"
dir = %q
machine = %q
[[session_sources]]
agent = "codex"
dir = %q
machine = "previous-owner"
[[session_sources]]
agent = "codex"
dir = %q
[pg]
url = "postgres://user@127.0.0.1:1/archive"
allow_insecure = true
`, root, id, filepath.Join(home, "sessions"), filepath.Join(home, "sessions")))
	cfg, _, err := loadPGServeConfigForTest(t, "--push-watch")
	require.NoError(t, err)
	assert.Equal(t, id, cfg.InstallationID)
	canonical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Contains(t, cfg.ResolveDirs(parser.AgentCodexCloud), filepath.Join(canonical, "sources", "codex-cloud"))
	_, _, err = prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
	require.ErrorContains(t, err, "is a symlink")
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	cfg, _, err = loadPGServeConfigForTest(t, "--push-watch")
	require.NoError(t, err)
	_, cleanup, err := prepareReplicaPushWatch(t.Context(), pgReplica{}, cfg, ReplicaPushConfig{}, "", true)
	require.NoError(t, err)
	cleanup()
	database, lock, err := openWriteDB(t.Context(), cfg)
	require.NoError(t, err)
	defer closeWriteDB(database, lock)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{AgentDirs: cfg.AgentDirs, SourceMachines: cfg.SourceMachines, ProviderMetadata: cfg.ProviderMetadata, Machine: cfg.InstallationID})
	defer engine.Close()
	for _, title := range []string{"Original title", "Updated title"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, "session_index.jsonl"), fmt.Appendf(nil, `{"id":%q,"thread_name":%q,"updated_at":"2026-01-01T01:00:00Z"}`+"\n", uuid, title), 0o600))
		stats := engine.SyncAll(t.Context(), nil)
		require.False(t, stats.Aborted)
		session, err := database.GetSessionFull(t.Context(), "codex:"+uuid)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, id, session.Machine)
		require.NotNil(t, session.SessionName)
		assert.Equal(t, title, *session.SessionName)
	}
	session, err := database.GetSession(t.Context(), "explicit")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, id, session.Machine)
	labels, err := database.GetMachineLabels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Collector", labels[id])
}
