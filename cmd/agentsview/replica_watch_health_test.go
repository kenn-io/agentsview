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

	h := newReplicaWatchHealth(dir, "clickhouse", "", now)
	h.start()
	assert.Equal(t, at("16:00"), load().PushStartedAt)
	h.finish(nil)

	clock = at("16:16")
	h.start()
	h.finish(errors.New("schema: not enough privileges"))
	clock = at("16:17")
	h.start()
	h.finish(errors.New("schema: still not enough privileges"))
	assert.Equal(t, replicaWatchRecord{
		LastSuccessAt: at("16:00"),
		FailingSince:  at("16:16"),
		LastFailureAt: at("16:17"),
		LastError:     "schema: still not enough privileges",
	}, load(), "a failure streak keeps its first failure")

	// A watcher that exits mid-push leaves the push marked running. A
	// restart for the same target keeps the history and clears that mark.
	clock = at("16:18")
	h.start()
	restarted := newReplicaWatchHealth(dir, "clickhouse", "", now)
	clock = at("21:30")
	restarted.finish(nil)
	assert.Equal(t, replicaWatchRecord{
		LastSuccessAt: at("21:30"),
		LastFailureAt: at("16:17"),
	}, load(), "a success ends the failure streak")

	other := newReplicaWatchHealth(dir, "clickhouse", "archive", now)
	other.start()
	assert.Equal(t, replicaWatchRecord{
		Target: "archive", PushStartedAt: at("21:30"),
	}, load(), "another target starts its own history")
}

func TestReplicaWatchStall(t *testing.T) {
	now := time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	tests := []struct {
		name    string
		rec     replicaWatchRecord
		changed int
		// wantSince is the lower bound passed to the change count; nil
		// means the count must not run.
		wantSince *time.Time
		want      string
	}{
		{
			name: "recent success",
			rec:  replicaWatchRecord{LastSuccessAt: ago(2 * time.Minute)},
		},
		{
			name: "short failure streak",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(3 * time.Minute),
				FailingSince:  ago(2 * time.Minute),
				LastError:     "connection reset",
			},
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
			name:      "idle watcher with nothing new",
			rec:       replicaWatchRecord{LastSuccessAt: ago(8 * time.Hour)},
			wantSince: new(ago(8 * time.Hour)),
		},
		{
			name:      "changes waiting with no push",
			rec:       replicaWatchRecord{LastSuccessAt: ago(time.Hour)},
			changed:   3,
			wantSince: new(ago(time.Hour)),
			want:      "3 session(s) changed after the last successful push 2026-10-26T20:30:00Z (1h0m0s ago) and nothing has pushed them; check that the watcher is running",
		},
		{
			name:      "never succeeded with sessions waiting",
			rec:       replicaWatchRecord{},
			changed:   4,
			wantSince: new(time.Time{}),
			want:      "4 session(s) are waiting and no push has succeeded; check that the watcher is running",
		},
		{
			name: "push running within the limit",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(time.Hour),
				PushStartedAt: ago(5 * time.Minute),
			},
		},
		{
			name: "push running past the limit",
			rec: replicaWatchRecord{
				LastSuccessAt: ago(time.Hour),
				PushStartedAt: ago(40 * time.Minute),
			},
			want: "the push that started 2026-10-26T20:50:00Z (40m0s ago) has not finished; it is stuck or the watcher stopped mid-push",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotSince *time.Time
			got, err := replicaWatchStall(tt.rec, now, 15*time.Minute,
				func(since time.Time) (int, error) {
					gotSince = &since
					return tt.changed, nil
				},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantSince, gotSince)
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
				health := newReplicaWatchHealth(dir, "pg", "", time.Now)
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
				assert.Equal(t, replicaWatchRecord{
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
					LastSuccessAt: time.Now().UTC(),
					LastFailureAt: failed.LastFailureAt,
				}, succeeded)

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}
