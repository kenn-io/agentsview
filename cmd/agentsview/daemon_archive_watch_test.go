package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDaemonArchiveNotificationsReconnectToCurrentOwner(t *testing.T) {
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), RequireDaemon: true}
	secondConnected := make(chan struct{}, 1)
	second := serveIngestionArchive(t, cfg, dbtest.OpenTestDB(t), nil, nil,
		server.WithBroadcaster(server.NewBroadcaster(0)))
	handler := second.Config.Handler
	second.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events" {
			secondConnected <- struct{}{}
		}
		handler.ServeHTTP(w, r)
	})
	port := second.Listener.Addr().(*net.TCPAddr).Port
	writeDaemonRuntimeForTest(t, cfg.DataDir, "127.0.0.1", port, version, false)

	disconnected := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: events_ready\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-disconnected:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(first.Close)
	backend := daemonArchiveWriteBackend{appCfg: cfg, tr: transport{Mode: transportHTTP, URL: first.URL}}
	changed := make(chan struct{}, 4)
	stop, open := backend.startArchiveNotifications(t.Context(), func() { changed <- struct{}{} })
	t.Cleanup(stop)
	open()
	receiveArchiveTest(t, changed)
	close(disconnected)
	receiveArchiveTest(t, secondConnected)
	receiveArchiveTest(t, changed)
	stop()
}

type archiveNotificationSnapshot struct {
	session  *db.Session
	messages []db.Message
	err      error
}

func TestDaemonArchiveWatchPublishesCommittedChangesDuringPush(t *testing.T) {
	testDaemonArchivePublication(t, false)
}

func TestDaemonLifecyclePushIngestsSourceChanges(t *testing.T) {
	testDaemonArchivePublication(t, true)
}

func testDaemonArchivePublication(t *testing.T, lifecycle bool) {
	t.Helper()
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.AuthToken = "test-token"
	cfg.NoSync = lifecycle
	wake := make(chan struct{}, 1)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	broadcaster := server.NewBroadcaster(0)
	engineConfig := localSyncEngineConfig(cfg)
	engineConfig.Emitter = broadcaster
	engine := syncpkg.NewEngine(t.Context(), database, engineConfig)
	t.Cleanup(engine.Close)
	path := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "project", "daemon-owned.jsonl")
	transcript := testjsonl.NewSessionBuilder()
	sequence := 0
	write := func(text string) {
		t.Helper()
		transcript.AddClaudeUserWithSessionID(fmt.Sprintf("2026-01-02T00:00:%02dZ", sequence), text, "daemon-owned")
		sequence++
		dbtest.WriteTestFile(t, path, []byte(transcript.String()))
	}
	read := func(ctx context.Context, database *db.DB) archiveNotificationSnapshot {
		session, sessionErr := database.GetSessionFull(ctx, "daemon-owned")
		messages, messageErr := database.GetAllMessages(ctx, "daemon-owned")
		return archiveNotificationSnapshot{session: session, messages: messages, err: errors.Join(sessionErr, messageErr)}
	}
	write("committed first")
	copies := make(chan archiveNotificationSnapshot, 8)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	probe := &archiveSourcePushProbe{
		seen: make(chan []db.Message, 8),
		inspect: func(ctx context.Context, archive *db.DB) {
			snapshot := read(ctx, archive)
			copies <- snapshot
			if len(snapshot.messages) == 2 {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		},
	}
	daemon := httptest.NewUnstartedServer(nil)
	cfg.Port = daemon.Listener.Addr().(*net.TCPAddr).Port
	srv := server.New(cfg, database, engine,
		server.WithBaseContext(t.Context()),
		server.WithBroadcaster(broadcaster),
		server.WithReplicas(archiveSourcePushReplica{Replica: postgres.Backend{}, probe: probe}),
	)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon.Config.Handler = srv.Handler()
	daemon.Start()
	t.Cleanup(daemon.Close)
	writeDaemonRuntimeForTest(t, cfg.DataDir, cfg.Host, cfg.Port, version, false)

	clientCfg := cfg
	clientCfg.RequireDaemon = true
	clientCfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}}
	dbtest.WriteTestFile(t, filepath.Join(clientCfg.AgentDirs[parser.AgentClaude][0], "project", "client-only.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "not owned by daemon", "client-only").String()))
	fire := make(chan time.Time, 4)
	backend := daemonArchiveWriteBackend{
		appCfg: clientCfg,
		tr:     transport{Mode: transportHTTP, URL: daemon.URL},
		watchHooks: &archivePushWatchHooks{
			newLoop: func(label string, _, _ time.Duration, push func(context.Context, pushReason, *syncpkg.WatchBatch) error) (*pushLoop, func()) {
				return &pushLoop{label: label, debounce: time.Hour, dirty: make(chan struct{}, 1), after: func(time.Duration) <-chan time.Time { return fire }, push: push}, func() {}
			},
			startWatcher: func(config.Config, *syncpkg.Engine, syncpkg.WatchCallback, syncpkg.WatcherOptions) (func(), func(), []string) {
				assert.Fail(t, "daemon-backed publication started a client filesystem watcher")
				return func() {}, func() {}, nil
			},
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- backend.ReplicaPushWatch(ctx, pgReplica{}, storage.ConfiguredReplica{Target: storage.ReplicaTarget{
			URL: "postgres://127.0.0.1/test?sslmode=disable", MachineName: "receiver",
		}}, ReplicaPushConfig{LifecycleWake: wake}, nil, nil, time.Hour, time.Hour)
	}()
	t.Cleanup(func() {
		unblock()
		cancel()
		require.NoError(t, receiveArchiveTest(t, done))
	})
	initial := receiveArchiveTest(t, copies)
	require.NoError(t, initial.err)
	require.NotNil(t, initial.session)
	fire <- time.Now()
	connected := receiveArchiveTest(t, copies)
	assert.Equal(t, read(t.Context(), database), connected)

	write("committed second")
	if lifecycle {
		wake <- struct{}{}
		fire <- time.Now()
		published := receiveArchiveTest(t, copies)
		assert.Equal(t, read(t.Context(), database), published)
		var contents []string
		for _, message := range published.messages {
			contents = append(contents, message.Content)
		}
		assert.Equal(t, []string{"committed first", "committed second"}, contents)
	} else {
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
		require.NoError(t, engine.SettleForPush(t.Context()))
		expectedSecond := read(t.Context(), database)
		fire <- time.Now()
		assert.Equal(t, expectedSecond, receiveArchiveTest(t, copies))
	}

	write("committed third")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	require.NoError(t, engine.SettleForPush(t.Context()))
	expectedThird := read(t.Context(), database)
	write("not committed fourth")
	unblock()
	fire <- time.Now()
	assert.Equal(t, expectedThird, receiveArchiveTest(t, copies))
	unrelated, err := database.GetSessionFull(t.Context(), "client-only")
	require.NoError(t, err)
	assert.Nil(t, unrelated)
}

func TestDaemonPushWatchFollowsArchiveWithoutClientSourceRoots(t *testing.T) {
	for _, backendName := range []string{"pg", "clickhouse", "duckdb"} {
		t.Run(backendName, func(t *testing.T) {
			events := make(chan struct{}, 1)
			subscribed := make(chan struct{})
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/events", r.URL.Path)
				assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: events_ready\ndata: {}\n\n")
				w.(http.Flusher).Flush()
				close(subscribed)
				for {
					select {
					case <-r.Context().Done():
						return
					case <-events:
						_, _ = fmt.Fprint(w, "event: data_changed\ndata: {\"scope\":\"sessions\"}\n\n")
						w.(http.Flusher).Flush()
					}
				}
			}))
			t.Cleanup(daemon.Close)
			h := newPushWatchOwnerHarness()
			hooks := h.hooks()
			hooks.startArchiveNotifications = nil
			hooks.startWatcher = func(
				config.Config, *syncpkg.Engine, syncpkg.WatchCallback, syncpkg.WatcherOptions,
			) (func(), func(), []string) {
				assert.Fail(t, "daemon-backed publication started a client filesystem watcher")
				return func() {}, func() { h.opened <- struct{}{} }, nil
			}
			backend := daemonArchiveWriteBackend{
				appCfg: config.Config{
					AuthToken: "test-token",
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentClaude: {filepath.Join(t.TempDir(), "client-only", "unavailable")},
					},
				},
				tr:         transport{Mode: transportHTTP, URL: daemon.URL},
				watchHooks: hooks,
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				if backendName == "duckdb" {
					done <- backend.DuckDBPushWatch(ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil, time.Hour, time.Hour)
					return
				}
				var replica storage.Replica = pgReplica{}
				if backendName == "clickhouse" {
					replica = clickhouse.Backend{}
				}
				done <- backend.ReplicaPushWatch(ctx, replica, storage.ConfiguredReplica{}, ReplicaPushConfig{}, nil, nil, time.Hour, time.Hour)
			}()

			startup := receiveArchiveTest(t, h.attempts)
			assert.Equal(t, reasonStartup, startup.reason)
			receiveArchiveTest(t, subscribed)
			h.fire <- time.Now()
			connected := receiveArchiveTest(t, h.attempts)
			assert.Equal(t, reasonArchiveChange, connected.reason)

			events <- struct{}{}
			h.fire <- time.Now()
			changed := receiveArchiveTest(t, h.attempts)
			assert.Equal(t, reasonArchiveChange, changed.reason)

			cancel()
			require.NoError(t, receiveArchiveTest(t, done))
		})
	}
}
