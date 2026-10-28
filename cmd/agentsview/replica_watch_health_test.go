package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/storage"
)

func TestReplicaWatchHealthRecordsPushOutcomes(t *testing.T) {
	dir := t.TempDir()
	path := replicaWatchHealthPath(dir, "clickhouse")
	clock := time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC)
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
		Target: storage.ReplicaTarget{URL: "clickhouse://db.example:9000", Schema: "agentsview"},
	}
	dest := replicaWatchDestination(target.Target)

	h := newReplicaWatchHealth(dir, "clickhouse", target, now)
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
		Destination:          dest,
		LastSuccessAt:        at("16:01"),
		LastSuccessStartedAt: at("16:00"),
		FailingSince:         at("16:16"),
		LastFailureAt:        at("16:17"),
		LastError:            "schema: still not enough privileges",
	}, load(), "a failure streak keeps its first failure")

	clock = at("16:30")
	h.start()
	h.finish(nil)
	clock = at("16:45")
	h.start()
	// A watcher that exits mid-push leaves the push marked running. A
	// restart counts it as a failure, so a crash loop builds a streak.
	restarted := newReplicaWatchHealth(dir, "clickhouse", target, now)
	assert.Equal(t, replicaWatchRecord{
		Destination:          dest,
		LastSuccessAt:        at("16:30"),
		LastSuccessStartedAt: at("16:30"),
		FailingSince:         at("16:45"),
		LastFailureAt:        at("16:45"),
		LastError:            "the watcher exited during a push",
	}, load(), "a restart turns an abandoned push into a failure")
	clock = at("17:00")
	restarted.start()
	newReplicaWatchHealth(dir, "clickhouse", target, now)
	rec := load()
	assert.Equal(t, at("16:45"), rec.FailingSince, "repeated crashes keep the first failure")
	assert.Equal(t, at("17:00"), rec.LastFailureAt)

	clock = at("21:30")
	other := newReplicaWatchHealth(dir, "clickhouse", storage.ConfiguredReplica{
		Name:   "archive",
		Target: target.Target,
	}, now)
	other.start()
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", Destination: dest, PushStartedAt: at("21:30"),
	}, load(), "another target starts its own history")

	moved := target
	moved.Target.Schema = "agentsview_v2"
	assert.False(t, load().matches(moved))
	newReplicaWatchHealth(dir, "clickhouse", moved, now).start()
	assert.Equal(t, replicaWatchRecord{
		Destination: replicaWatchDestination(moved.Target), PushStartedAt: at("21:30"),
	}, load(), "a new URL or schema starts its own history")
}

func TestReplicaWatchStall(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name string
		rec  replicaWatchRecord
		// changes are the archive's session change times.
		changes []time.Time
		want    string
	}{
		{
			name: "recent success",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(2 * time.Minute), LastSuccessStartedAt: ago(3 * time.Minute),
			},
			changes: []time.Time{ago(time.Minute)},
		},
		{
			name: "short failure streak",
			rec: replicaWatchRecord{
				LastSuccessAt:        ago(3 * time.Hour),
				LastSuccessStartedAt: ago(3 * time.Hour),
				FailingSince:         ago(2 * time.Minute),
				LastError:            "connection reset",
			},
			changes: []time.Time{ago(time.Hour)},
		},
		{
			name: "long failure streak",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(5*time.Hour + 15*time.Minute),
				FailingSince:  ago(5*time.Hour + 14*time.Minute),
				LastError:     "not enough privileges",
			},
			want: "pushes have failed since 2026-10-26T16:16:00Z (5h14m0s ago): not enough privileges",
		},
		{
			name: "retry running during a long failure streak",
			rec: replicaWatchRecord{
				PushStartedAt: ago(time.Second),
				FailingSince:  ago(time.Hour),
				LastError:     "not enough privileges",
			},
			want: "pushes have failed since 2026-10-26T20:30:00Z (1h0m0s ago): not enough privileges",
		},
		{
			name:    "idle watcher with nothing new",
			rec:     replicaWatchRecord{LastSuccessStartedAt: ago(8 * time.Hour)},
			changes: []time.Time{ago(9 * time.Hour)},
		},
		{
			name:    "change after a long idle still within the debounce",
			rec:     replicaWatchRecord{LastSuccessStartedAt: ago(8 * time.Hour)},
			changes: []time.Time{ago(20 * time.Second)},
		},
		{
			name: "changes waiting with no push",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(time.Hour), LastSuccessStartedAt: ago(time.Hour + time.Minute),
			},
			changes: []time.Time{ago(time.Hour + time.Minute), ago(30 * time.Minute), ago(20 * time.Minute), ago(time.Minute)},
			want:    "3 session(s) changed after the last successful push started 2026-10-26T20:29:00Z (1h1m0s ago) and nothing has pushed them; check that the watcher is running",
		},
		{
			name:    "never succeeded with sessions waiting",
			rec:     replicaWatchRecord{},
			changes: []time.Time{ago(48 * time.Hour), ago(time.Hour)},
			want:    "2 session(s) are waiting and no push has succeeded; check that the watcher is running",
		},
		{
			name: "push running within the limit",
			rec: replicaWatchRecord{
				LastSuccessStartedAt: ago(time.Hour),
				PushStartedAt:        ago(5 * time.Minute),
			},
			changes: []time.Time{ago(30 * time.Minute)},
		},
		{
			name: "push running past the limit",
			rec: replicaWatchRecord{
				LastSuccessStartedAt: ago(time.Hour),
				PushStartedAt:        ago(40 * time.Minute),
			},
			want: "the push that started 2026-10-26T20:50:00Z (40m0s ago) has not finished; it is stuck or the watcher stopped mid-push",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replicaWatchStall(tt.rec, now, 15*time.Minute,
				func(from, until time.Time) (int, error) {
					n := 0
					for _, c := range tt.changes {
						if !c.Before(from) && !c.After(until) {
							n++
						}
					}
					return n, nil
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
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
				health := newReplicaWatchHealth(dir, "pg", storage.ConfiguredReplica{}, time.Now)
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
					Destination:   dest,
					FailingSince:  time.Now().UTC(),
					LastFailureAt: time.Now().UTC(),
					LastError:     "1 session(s) failed to push",
				}, failed)

				time.Sleep(time.Minute)
				h.floor <- time.Now()
				receiveArchiveTest(t, h.attempts)
				synctest.Wait()
				succeeded, err := loadReplicaWatchRecord(path)
				require.NoError(t, err)
				assert.Equal(t, replicaWatchRecord{
					Destination:          dest,
					LastSuccessAt:        time.Now().UTC(),
					LastSuccessStartedAt: time.Now().UTC(),
					LastFailureAt:        failed.LastFailureAt,
				}, succeeded)

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}
