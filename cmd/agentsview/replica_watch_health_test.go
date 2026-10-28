package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestReplicaWatchHealthRecordsPushOutcomes(t *testing.T) {
	dir := t.TempDir()
	path := replicaWatchHealthPath(dir, "clickhouse")
	clock := time.Date(2026, 10, 26, 15, 59, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	at := func(hhmm string) time.Time {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, "2026-10-26T"+hhmm+":00Z")
		require.NoError(t, err)
		return ts
	}
	load := func() replicaWatchRecord {
		t.Helper()
		rec, err := loadReplicaWatchRecord(path)
		require.NoError(t, err)
		return rec
	}
	target := storage.ConfiguredReplica{
		Name:   "archive",
		Target: storage.ReplicaTarget{URL: "clickhouse://db.example:9000", Schema: "agentsview"},
	}
	dest := replicaWatchDestination(target.Target)

	h := newReplicaWatchHealth(dir, "clickhouse", target, 15*time.Minute, now)
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", Destination: dest,
		WatcherStartedAt: at("15:59"), IntervalSeconds: 900,
	}, load(), "a starting watcher records itself before its first push")
	clock = at("16:00")
	h.start()
	assert.Equal(t, at("16:00"), load().PushStartedAt)
	clock = at("16:01")
	h.finish(nil)

	clock = at("16:16")
	h.start()
	h.finish(errors.New("schema: not enough privileges"))
	clock = at("16:17")
	h.start()
	h.finish(errors.New("schema: still not enough privileges"))
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", Destination: dest,
		WatcherStartedAt: at("15:59"), IntervalSeconds: 900,
		LastSuccessAt: at("16:01"),
		FailingSince:  at("16:16"),
		LastFailureAt: at("16:17"),
		LastError:     "schema: still not enough privileges",
	}, load(), "a failure streak keeps its first failure")

	clock = at("16:30")
	h.start()
	h.finish(nil)
	clock = at("16:45")
	h.start()
	// A watcher that exits mid-push leaves the push marked running. A
	// restart counts it as a failure, so a crash loop builds a streak.
	clock = at("16:46")
	restarted := newReplicaWatchHealth(dir, "clickhouse", target, 15*time.Minute, now)
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", Destination: dest,
		WatcherStartedAt: at("16:46"), IntervalSeconds: 900,
		LastSuccessAt: at("16:30"),
		FailingSince:  at("16:45"),
		LastFailureAt: at("16:45"),
		LastError:     "the watcher exited during a push",
	}, load(), "a restart turns an abandoned push into a failure")
	clock = at("17:00")
	restarted.start()
	newReplicaWatchHealth(dir, "clickhouse", target, 15*time.Minute, now)
	rec := load()
	assert.Equal(t, at("16:45"), rec.FailingSince, "repeated crashes keep the first failure")
	assert.Equal(t, at("17:00"), rec.LastFailureAt)

	clock = at("21:30")
	other := target
	other.Name = "other"
	newReplicaWatchHealth(dir, "clickhouse", other, time.Hour, now)
	assert.Equal(t, replicaWatchRecord{
		Target: "other", Destination: dest,
		WatcherStartedAt: at("21:30"), IntervalSeconds: 3600,
	}, load(), "another target starts its own history")

	newReplicaWatchHealth(dir, "clickhouse", target, 15*time.Minute, now).finish(nil)
	moved := target
	moved.Target.Schema = "agentsview_v2"
	require.False(t, load().matches(moved))
	clock = at("21:31")
	newReplicaWatchHealth(dir, "clickhouse", moved, 15*time.Minute, now)
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", Destination: replicaWatchDestination(moved.Target),
		WatcherStartedAt: at("21:31"), IntervalSeconds: 900,
	}, load(), "a new schema for the same target starts its own history")
}

func TestReplicaWatchStall(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	// The watcher pushes at least every 15 minutes; the check allows 10
	// more.
	const interval = 900
	tests := []struct {
		name string
		rec  replicaWatchRecord
		want string
	}{
		{
			name: "floor push within the interval",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(20 * time.Minute),
			},
		},
		{
			name: "no push past the interval and limit",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(26 * time.Minute),
			},
			want: "no push has finished since 2026-10-26T21:04:00Z (26m0s ago), though the watcher pushes at least every 15m0s; it has stopped or is waiting for a local sync, so check that it is running",
		},
		{
			name: "watcher never pushed",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(time.Hour), IntervalSeconds: interval,
			},
			want: "no push has finished since 2026-10-26T20:30:00Z (1h0m0s ago), though the watcher pushes at least every 15m0s; it has stopped or is waiting for a local sync, so check that it is running",
		},
		{
			name: "restarted watcher before its first push",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(time.Minute), IntervalSeconds: interval,
				LastSuccessAt: ago(5 * time.Hour),
			},
		},
		{
			name: "short failure streak",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(3 * time.Minute),
				FailingSince:  ago(2 * time.Minute),
				LastFailureAt: ago(time.Minute),
				LastError:     "connection reset",
			},
		},
		{
			name: "long failure streak",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(5*time.Hour + 15*time.Minute),
				FailingSince:  ago(5*time.Hour + 14*time.Minute),
				LastFailureAt: ago(time.Minute),
				LastError:     "not enough privileges",
			},
			want: "pushes have failed since 2026-10-26T16:16:00Z (5h14m0s ago): not enough privileges",
		},
		{
			name: "retry running during a long failure streak",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				PushStartedAt: ago(time.Second),
				FailingSince:  ago(time.Hour),
				LastFailureAt: ago(time.Minute),
				LastError:     "not enough privileges",
			},
			want: "pushes have failed since 2026-10-26T20:30:00Z (1h0m0s ago): not enough privileges",
		},
		{
			name: "push running within the limit",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(time.Hour),
				PushStartedAt: ago(5 * time.Minute),
			},
		},
		{
			name: "push running past the limit",
			rec: replicaWatchRecord{
				WatcherStartedAt: ago(8 * time.Hour), IntervalSeconds: interval,
				LastSuccessAt: ago(time.Hour),
				PushStartedAt: ago(40 * time.Minute),
			},
			want: "the push that started 2026-10-26T20:50:00Z (40m0s ago) has not finished; it is stuck or the watcher stopped mid-push",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replicaWatchStall(tt.rec, now, 10*time.Minute))
		})
	}
}

// TestReplicaPushWatchOwnersRecordHealth runs both archive owners' watch
// loops and checks that each push they run lands in the health record.
func TestReplicaPushWatchOwnersRecordHealth(t *testing.T) {
	local := testLocalArchiveWriteBackend(t)
	owners := []struct {
		name string
		run  func(context.Context, *archivePushWatchHooks, ReplicaPushConfig) error
	}{
		{
			name: "daemon",
			run: func(ctx context.Context, hooks *archivePushWatchHooks, cfg ReplicaPushConfig) error {
				return (daemonArchiveWriteBackend{watchHooks: hooks}).ReplicaPushWatch(
					ctx, pgReplica{}, storage.ConfiguredReplica{}, cfg, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
		{
			name: "local",
			run: func(ctx context.Context, hooks *archivePushWatchHooks, cfg ReplicaPushConfig) error {
				local.watchHooks = hooks
				return local.ReplicaPushWatch(
					ctx, pgReplica{}, storage.ConfiguredReplica{}, cfg, nil, nil,
					localPushWatchTestDebounce, time.Hour,
				)
			},
		},
	}
	for _, owner := range owners {
		t.Run(owner.name, func(t *testing.T) {
			dir := t.TempDir()
			path := replicaWatchHealthPath(dir, "pg")
			synctest.Test(t, func(t *testing.T) {
				h := newPushWatchOwnerHarness(1)
				health := newReplicaWatchHealth(dir, "pg", storage.ConfiguredReplica{}, time.Hour, time.Now)
				startedAt := time.Now().UTC()
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() {
					done <- owner.run(ctx, h.hooks(), ReplicaPushConfig{Health: health})
				}()

				receiveArchiveTest(t, h.attempts)
				synctest.Wait()
				failed, err := loadReplicaWatchRecord(path)
				require.NoError(t, err)
				dest := replicaWatchDestination(storage.ReplicaTarget{})
				assert.Equal(t, replicaWatchRecord{
					Destination:      dest,
					WatcherStartedAt: startedAt,
					IntervalSeconds:  3600,
					FailingSince:     time.Now().UTC(),
					LastFailureAt:    time.Now().UTC(),
					LastError:        "1 session(s) failed to push",
				}, failed)

				time.Sleep(time.Minute)
				h.floor <- time.Now()
				receiveArchiveTest(t, h.attempts)
				synctest.Wait()
				succeeded, err := loadReplicaWatchRecord(path)
				require.NoError(t, err)
				assert.Equal(t, replicaWatchRecord{
					Destination:      dest,
					WatcherStartedAt: startedAt,
					IntervalSeconds:  3600,
					LastSuccessAt:    time.Now().UTC(),
					LastFailureAt:    failed.LastFailureAt,
				}, succeeded)

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}

// TestReplicaWatchRecordsStartupFailures restarts a watcher that exits
// before its first push, as a service manager would, and checks that the
// restarts build a failure streak the stall check reports even right after a
// restart.
func TestReplicaWatchRecordsStartupFailures(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares dataDir so the watcher fails, and returns the error
		// it must report.
		setup func(t *testing.T, dataDir string) string
	}{
		{
			name: "writer does not open",
			setup: func(*testing.T, string) string {
				return "opening writer: " + errNoWritableDaemon.Error()
			},
		},
		{
			name: "watch exits after the writer opens",
			setup: func(t *testing.T, dataDir string) string {
				t.Helper()
				daemon := pushRuntimeServer(t, "/api/v1/push/pg", func(http.ResponseWriter, *http.Request) {
					assert.Fail(t, "a watcher that failed to start must not push")
				})
				host, port := splitTestServerURL(t, daemon.URL)
				writeDaemonRuntimeForTest(t, dataDir, host, port, version, false)
				require.NoError(t, os.WriteFile(filepath.Join(dataDir, "sessions.db"), []byte("not a database"), 0o600))
				return "reading archive mode"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := testDataDir(t)
			t.Setenv("AGENTSVIEW_PG_URL", "postgres://user@127.0.0.1:1/archive")
			cfg, _, err := loadPGServeConfigForTest(t)
			require.NoError(t, err)
			cfg.PG.AllowInsecure = true
			wantErr := tt.setup(t, dir)
			restart := func() replicaWatchRecord {
				t.Helper()
				run, cleanup, err := prepareReplicaPushWatch(
					t.Context(), pgReplica{}, cfg, ReplicaPushConfig{NoStartDaemon: true}, "", false,
				)
				if err == nil {
					err = run()
					cleanup()
				}
				require.ErrorContains(t, err, wantErr)
				rec, err := loadReplicaWatchRecord(replicaWatchHealthPath(dir, "pg"))
				require.NoError(t, err)
				return rec
			}

			first := restart()
			require.False(t, first.FailingSince.IsZero(), "a failed start begins a failure streak")
			restart()
			rec := restart()
			assert.Equal(t, first.FailingSince, rec.FailingSince, "restarts keep the first failure")
			assert.Contains(t, rec.LastError, wantErr)
			stall := replicaWatchStall(rec, rec.WatcherStartedAt.Add(11*time.Minute), 10*time.Minute)
			assert.Contains(t, stall, "pushes have failed since ")
			assert.Contains(t, stall, wantErr)
		})
	}
}

// statusStubReplica answers the replica status query with status.
type statusStubReplica struct {
	pgReplica
	status func(context.Context) error
}

func (r statusStubReplica) Status(
	ctx context.Context, _ *db.DB, _ storage.ConfiguredReplica, _, _ []string,
) (storage.ReplicaStatus, error) {
	return storage.ReplicaStatus{Rows: []storage.StatusRow{{Label: "PG sessions:", Value: "7"}}}, r.status(ctx)
}

// TestReplicaStatusMaxStaleDoesNotWaitOnReplica checks that a hung replica
// status query cannot hide a stall: a stall the record shows is reported
// without querying, and a query that hangs fails at the deadline.
func TestReplicaStatusMaxStaleDoesNotWaitOnReplica(t *testing.T) {
	prevTimeout := replicaStatusQueryTimeout
	t.Cleanup(func() { replicaStatusQueryTimeout = prevTimeout })
	replicaStatusQueryTimeout = 50 * time.Millisecond

	tests := []struct {
		name        string
		lastSuccess time.Duration
		wantErr     string
		wantOutput  string
	}{
		{
			name:        "stalled watcher",
			lastSuccess: 2 * time.Hour,
			wantErr:     "push watcher stalled for more than --max-stale 10m0s: no push has finished since",
			wantOutput:  "Watcher last success:",
		},
		{
			name:        "healthy watcher, hung replica",
			lastSuccess: time.Minute,
			wantErr:     "PostgreSQL did not answer the status query within 50ms: context deadline exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			clearConfiguredAgentEnvVars(t)
			isolateDefaultAgentDirs(t, dataDir)
			restoreTestLogger(t)
			writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.archive]
url = "postgres://archive"
`)
			appCfg, err := config.LoadMinimalFor(replicaConfigScope(pgReplica{}, nil))
			require.NoError(t, err)
			refs, err := storage.SelectTargets(pgReplica{}, appCfg, "archive", false)
			require.NoError(t, err)
			target, err := pgReplica{}.ResolveTarget(appCfg, refs[0])
			require.NoError(t, err)
			now := time.Now().UTC()
			data, err := json.Marshal(replicaWatchRecord{
				Target: target.Name, Destination: replicaWatchDestination(target.Target),
				WatcherStartedAt: now.Add(-8 * time.Hour), IntervalSeconds: 900,
				LastSuccessAt: now.Add(-tt.lastSuccess),
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(replicaWatchHealthPath(dataDir, "pg"), data, 0o600))

			queried := false
			backend := statusStubReplica{status: func(ctx context.Context) error {
				queried = true
				<-ctx.Done()
				return ctx.Err()
			}}
			var runErr error
			out := captureStdout(t, func() {
				runErr = runReplicaStatus(t.Context(), backend, "archive", ReplicaStatusConfig{MaxStale: 10 * time.Minute})
			})
			require.Error(t, runErr)
			assert.Contains(t, runErr.Error(), tt.wantErr)
			assert.Contains(t, out, tt.wantOutput)
			assert.Equal(t, tt.wantOutput == "", queried, "only a healthy record queries the replica")
		})
	}
}
