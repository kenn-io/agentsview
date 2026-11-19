//go:build fts5

package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

type pausedResetAccounts struct {
	*rollupAccountSource
	read    chan struct{}
	release chan struct{}
}

func (s *pausedResetAccounts) StreamUsageAccounts(ctx context.Context, emit func(UsageSourceAccount) error) (string, int64, error) {
	id, revision, err := s.rollupAccountSource.StreamUsageAccounts(ctx, emit)
	close(s.read)
	select {
	case <-s.release:
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	return id, revision, err
}

func TestUsageResetRejectsOldPublications(t *testing.T) {
	t.Run("session spool", func(t *testing.T) {
		_, cache := usageSpoolExtractionFixture(t, 2)
		_, err := cache.db.ExecContext(t.Context(), `DELETE FROM usage_cached_sessions`)
		require.NoError(t, err)
		versions := []usageSourceVersion{{SessionID: "session"}}
		expected, err := cache.fill.captureFillInstallExpectations(t.Context(), versions)
		require.NoError(t, err)
		spool, extracted, err := cache.fill.extractSessions(t.Context(), versions, expected)
		require.NoError(t, err)
		defer spool.Close()
		require.NoError(t, cache.resetRewoundUsage(t.Context(), 0))
		_, _, err = cache.fill.installSpool(t.Context(), spool, []usageSourceVersion{extracted["session"]}, nil, expected, 0)
		require.ErrorIs(t, err, errUsageCacheSourceChanged)
		require.Zero(t, usageCacheCount(t, cache, "usage_facts"))
	})
	t.Run("coverage", func(t *testing.T) {
		database := testDB(t)
		seedJournaledUsage(t, database, "session")
		filter := UsageFilter{Timezone: "UTC"}
		cache := warmJournaledUsage(t, database, filter)
		identity := usageTimezoneIdentityFor(filter.Location(), nil)
		zone, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
		require.NoError(t, err)
		otherIdentity := usageTimezoneIdentityFor(time.FixedZone("other", 3600), nil)
		unclaimed, err := readUsageZoneCoverage(t.Context(), cache.db, otherIdentity.Key)
		require.NoError(t, err)
		require.False(t, unclaimed.revision.Valid)
		require.NoError(t, cache.resetRewoundUsage(t.Context(), zone.resetEpoch))
		require.NoError(t, cache.claimUsageZone(t.Context(), otherIdentity, unclaimed, zone.revision.Int64+1, zone.pricing, nil))
		current, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
		require.NoError(t, err)
		require.False(t, current.revision.Valid)
		current, err = readUsageZoneCoverage(t.Context(), cache.db, otherIdentity.Key)
		require.NoError(t, err)
		require.False(t, current.revision.Valid)
		require.NoError(t, cache.claimUsageZone(t.Context(), otherIdentity, current, zone.revision.Int64+1, zone.pricing, nil))
		current, err = readUsageZoneCoverage(t.Context(), cache.db, otherIdentity.Key)
		require.NoError(t, err)
		require.True(t, current.revision.Valid)
	})
	t.Run("accounts", func(t *testing.T) {
		database := testDB(t)
		id, err := database.GetDatabaseID(t.Context())
		require.NoError(t, err)
		source := &pausedResetAccounts{rollupAccountSource: &rollupAccountSource{database: database, databaseID: id, revision: 10, accounts: []UsageSourceAccount{{ID: 1, Model: "model-a", OutputTokens: 10}}}, read: make(chan struct{}), release: make(chan struct{})}
		reader, err := NewUsageCacheReader(t.TempDir(), source)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		cache, err := reader.engine.usageCache.Generation(t.Context(), id)
		require.NoError(t, err)
		done := make(chan error, 1)
		release := sync.OnceFunc(func() { close(source.release) })
		t.Cleanup(release)
		go func() { done <- cache.fill.replaceExternalAccounts(t.Context(), 10) }()
		select {
		case <-source.read:
		case <-time.After(10 * time.Second):
			require.FailNow(t, "account read did not reach publication")
		}
		require.NoError(t, cache.resetRewoundUsage(t.Context(), 0))
		release()
		require.ErrorIs(t, <-done, errUsageCacheSourceChanged)
		require.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
	})
	t.Run("rollup and prepared read", func(t *testing.T) {
		database := testDB(t)
		seedUsageSnapshotSession(t, database, "rollup-race", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
		id, err := database.GetDatabaseID(t.Context())
		require.NoError(t, err)
		source := &rollupAccountSource{database: database, databaseID: id, revision: 10, output: 10}
		reader, err := NewUsageCacheReader(t.TempDir(), source)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		snapshot, fills, cache := prepareUsageRollupTest(t, reader.engine)
		built, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		t.Cleanup(unblock)
		cache.rollup.observer.beforeInstall = func([]usageRollupBuild) { close(built); <-release }
		done := make(chan error, 1)
		go func() {
			_, _, err := cache.rollup.Ensure(t.Context(), snapshot, fills, export.NewPricingResolver(snapshot.PricingRows))
			done <- err
		}()
		select {
		case <-built:
		case <-time.After(10 * time.Second):
			require.FailNow(t, "rollup did not reach installation")
		}
		require.NoError(t, cache.resetRewoundUsage(t.Context(), 0))
		unblock()
		require.ErrorIs(t, <-done, errUsageCacheSourceChanged)
		_, err = cache.usageRollupQuery(t.Context(), usagePreparedWindow{snapshot: snapshot}, UsageFilter{Timezone: "UTC"}, export.NewPricingResolver(snapshot.PricingRows), 0)
		require.ErrorIs(t, err, errUsageCacheSourceChanged)
	})
}

func TestUsageDeferredAccountFillAfterReset(t *testing.T) {
	database := testDB(t)
	id, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	source := &rollupAccountSource{database: database, databaseID: id, revision: 10, accounts: []UsageSourceAccount{{ID: 1, OutputTokens: 10}}}
	reader, err := NewUsageCacheReader(t.TempDir(), source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	cache, err := reader.engine.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, cache.resetRewoundUsage(t.Context(), 0))
	err = cache.fill.ensureCursor(withUsageWorkDeferred(t.Context()), 10)
	require.ErrorIs(t, err, ErrUsageCacheWarming)
	require.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
	require.NoError(t, cache.fill.ensureCursor(t.Context(), 10))
	require.Equal(t, 1, usageCacheCount(t, cache, "cursor_usage_facts"))
}
