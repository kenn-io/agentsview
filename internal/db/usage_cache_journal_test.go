//go:build fts5

package db

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageJournaledWindowDiscoverySeeksDates(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	// Pin the tiny-table statistics that can make a join prefer an install scan.
	_, err := cache.db.ExecContext(t.Context(), `ANALYZE`)
	require.NoError(t, err)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
	require.NoError(t, err)
	capture := &sessionExportQueryCapture{inner: cache.db}
	_, err = usageRollupWindowSessions(t.Context(), capture, zone.id, filter)
	require.NoError(t, err)
	for _, query := range capture.queries {
		rows, err := cache.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query.query, query.args...)
		require.NoError(t, err)
		defer rows.Close()
		plan := strings.Join(explainQueryPlanDetails(t, rows), "\n")
		require.NoError(t, rows.Close())
		// A zone-index search still visits every unrelated install. Bounded
		// discovery must seek dates first, then look up matching install IDs.
		assert.NotContains(t, plan, "timezone_id=?", plan)
		assert.NotContains(t, plan, "SCAN i", plan)
		assert.Contains(t, plan, "SEARCH i USING INTEGER PRIMARY KEY (rowid=?)", plan)
		assert.Contains(t, plan, "local_date>? AND local_date<?", plan)
	}
}

func TestUsageJournaledWindowDiscoveryBoundaries(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "inside")
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "inside", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T00:30:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
	seedUsageSnapshotSession(t, database, "earlier", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, database, "later", "project-b", "2026-08-10T10:00:00Z", 0, 20, "model-a")
	insertSession(t, database, "undated", "project")
	insertSession(t, database, "activity-only", "project")
	require.NoError(t, database.InsertMessages(t.Context(), []Message{
		{SessionID: "undated", Ordinal: 0, Role: "assistant"},
		{SessionID: "activity-only", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-12T10:00:00Z"},
	}))
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, from, to string
		want           []string
	}{
		{"daily and exceptions", "2026-08-10", "2026-08-10", []string{"inside", "earlier", "later"}},
		{"second day", "2026-08-11", "2026-08-11", []string{"inside"}},
		{"lower bound", "2026-08-11", "", []string{"inside"}},
		{"upper bound", "", "2026-08-10", []string{"inside", "earlier", "later"}},
		{"activity only", "2026-08-12", "2026-08-12", nil},
		{"unbounded", "", "", []string{"inside", "earlier", "later", "activity-only", "undated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter.From, filter.To = tc.from, tc.to
			got, err := usageRollupWindowSessions(t.Context(), cache.db, zone.id, filter)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, got)
		})
	}
	t.Run("another timezone", func(t *testing.T) {
		otherFilter := UsageFilter{Timezone: "Asia/Tokyo"}
		warmJournaledUsage(t, database, otherFilter)
		otherZone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(otherFilter.Location(), nil).Key)
		require.NoError(t, err)
		require.True(t, otherZone.revision.Valid)
		require.NotEqual(t, zone.id, otherZone.id)
		otherFilter.From, otherFilter.To = "2026-08-10", "2026-08-10"
		got, err := usageRollupWindowSessions(t.Context(), cache.db, otherZone.id, otherFilter)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"earlier", "later"}, got)
		got, err = usageRollupWindowSessions(t.Context(), cache.db, zone.id, UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"inside", "earlier", "later"}, got)
	})
}

func seedJournaledUsage(t *testing.T, database *DB, id string) {
	t.Helper()
	insertSession(t, database, id, "project", func(s *Session) { s.Agent = "codex"; s.StartedAt = new("2026-08-10T23:30:00Z") })
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T23:30:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)}}))
}

func warmJournaledUsage(t *testing.T, database *DB, filter UsageFilter) *usageCache {
	t.Helper()
	_, err := database.GetDailyUsage(t.Context(), UsageFilter{Timezone: filter.Timezone})
	require.NoError(t, err)
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	// Keep races under the test's control. Request-triggered passes remain enabled.
	database.StopUsageCacheBackfill()
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	cache, err := database.usageCache.Generation(t.Context(), prepared.snapshot.DatabaseID)
	require.NoError(t, err)
	return cache
}

func assertJournaledDailyUsageLegacy(t *testing.T, database *DB, filter UsageFilter) {
	t.Helper()
	want, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func assertJournaledUsageLegacy(t *testing.T, database *DB, filter UsageFilter) {
	t.Helper()
	assertJournaledDailyUsageLegacy(t, database, filter)
	wantTop, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 10)
	require.NoError(t, err)
	gotTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	assert.Equal(t, wantTop, gotTop, "top sessions must match the archive")
	wantCounts, err := database.getUsageSessionCountsLegacy(t.Context(), filter)
	require.NoError(t, err)
	gotCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, wantCounts, gotCounts, "session counts must match the archive")
}

func TestUsageJournaledFreshness(t *testing.T) {
	for _, name := range []string{"append", "soft delete", "hard delete", "restore", "start date", "message date", "agent", "price", "price with filter", "second handle"} {
		t.Run(name, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-11", Timezone: "UTC"}
			if name == "agent" {
				filter.Agent = "claude"
			}
			if name == "price with filter" {
				filter.Agent = "codex"
				seedJournaledUsage(t, database, "excluded")
				_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='excluded'`)
				require.NoError(t, err)
			}
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
			if name == "start date" {
				require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", nil))
				require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "session", []UsageEvent{{Source: "session", Model: "journal-model", InputTokens: 100}}))
			}
			if name == "restore" {
				_, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				require.NoError(t, database.SoftDeleteSession(t.Context(), "session"))
			}
			warmJournaledUsage(t, database, filter)
			switch name {
			case "append":
				require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T01:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
			case "soft delete":
				require.NoError(t, database.SoftDeleteSession(t.Context(), "session"))
			case "hard delete":
				require.NoError(t, database.DeleteSession(t.Context(), "session"))
			case "restore":
				_, err := database.RestoreSession(t.Context(), "session")
				require.NoError(t, err)
			case "start date":
				_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET started_at='2026-08-11T01:00:00Z' WHERE id='session'`)
				require.NoError(t, err)
			case "message date":
				require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-11T01:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)}}))
			case "agent":
				_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='session'`)
				require.NoError(t, err)
			case "price", "price with filter":
				require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 2_000_000}}}))
			case "second handle":
				other, err := OpenIsolatedContext(t.Context(), database.Path())
				require.NoError(t, err)
				require.NoError(t, other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T01:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
				require.NoError(t, other.Close())
			}
			assertJournaledDailyUsageLegacy(t, database, filter)
			if name == "price with filter" {
				got, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				assert.EqualValues(t, 200, got.Totals.TotalCost.Microdollars)
			}
		})
	}
}

func TestUsageJournaledTrustBoundaries(t *testing.T) {
	for _, name := range []string{"new generation", "revision above archive", "custom pricing"} {
		t.Run(name, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
			database.usageBackfillMu.Lock()
			database.usageBackfillEnabled = false
			database.usageBackfillMu.Unlock()
			switch name {
			case "new generation":
				require.NoError(t, database.usageCache.Close())
				database.usageCache = newUsageCacheManager(database.Path())
				database.usageCache.attachArchive(database)
				// A different archive identity selects an empty cache generation.
				_, err := database.getWriter().ExecContext(t.Context(), `UPDATE archive_metadata SET value='new-generation' WHERE key='database_id'`)
				require.NoError(t, err)
			case "revision above archive":
				_, err := cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_timezones SET change_revision=9223372036854775807, change_install_revision=9223372036854775807`)
				require.NoError(t, err)
			case "custom pricing":
				database.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 3_000_000}})
			}
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
			require.NoError(t, err)
			assert.False(t, prepared.journaled)
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledLostAndDeleted(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "live")
	seedJournaledUsage(t, database, "deleted")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	require.NoError(t, database.SoftDeleteSession(t.Context(), "deleted"))
	_, err := cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id IN ('live','deleted')`)
	require.NoError(t, err)
	assertJournaledUsageLegacy(t, database, filter)
	assertJournaledUsageLegacy(t, database, filter)
	var remaining int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_cached_sessions WHERE session_id='deleted'`).Scan(&remaining))
	assert.Zero(t, remaining)
}

func TestUsageJournaledStaleDeletionAfterPeerRestore(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "notification", run: func(t *testing.T) {
			t.Helper()
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			require.NoError(t, database.SoftDeleteSession(t.Context(), "session"))
			versions, revision, err := cache.fill.recheckSourceVersions(t.Context(), []usageSourceVersion{{SessionID: "session"}})
			require.NoError(t, err)
			require.Empty(t, versions)

			other, err := OpenIsolatedContext(t.Context(), database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			_, err = other.RestoreSession(t.Context(), "session")
			require.NoError(t, err)
			got, err := other.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Equal(t, 100, got.Totals.InputTokens)
			zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
			require.NoError(t, err)
			require.Greater(t, zone.revision.Int64, revision)
			// Resume the deletion batch after its archive recheck, once the other
			// handle has restored the session and advanced coverage past the restore.
			require.NoError(t, cache.fill.deleteNotificationSessions([]string{"session"}, revision))
			assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"), "the stale notification must delete cached facts before reads repair them")
			for range 2 {
				got, err = database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, 100, got.Totals.InputTokens)
			}
		}},
		{name: "foreground", run: func(t *testing.T) {
			t.Helper()
			t.Run("daily", func(t *testing.T) {
				database := testDB(t)
				seedJournaledUsage(t, database, "session")
				// A source timestamp ahead of the clock keeps the fingerprint
				// unchanged through trash and restore.
				_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET file_mtime=? WHERE id='session'`, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
				require.NoError(t, err)
				filter := UsageFilter{Timezone: "UTC"}
				cache := warmJournaledUsage(t, database, filter)
				snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
				require.NoError(t, err)
				other, err := OpenIsolatedContext(t.Context(), database.Path())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, other.Close()) })
				require.NoError(t, database.SoftDeleteSession(t.Context(), "session"))
				cache.fill.observer.afterExtract = func([]usageSourceVersion) {
					_, err := other.RestoreSession(t.Context(), "session")
					require.NoError(t, err)
					got, err := other.GetDailyUsage(t.Context(), filter)
					require.NoError(t, err)
					require.Equal(t, 100, got.Totals.InputTokens)
				}
				results, err := cache.fill.fillSessionsDetached(t.Context(), snapshot.Versions)
				cache.fill.observer.afterExtract = nil
				require.NoError(t, err)
				require.True(t, results["session"].Deleted, "resume the deletion captured before restore")
				assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"), "the foreground fill must apply its captured deletion")
				// The report must recover before any activity scan repairs facts.
				for range 2 {
					got, err := database.GetDailyUsage(t.Context(), filter)
					require.NoError(t, err)
					assert.Equal(t, 100, got.Totals.InputTokens)
				}
			})
		}},
		{name: "sweep", run: func(t *testing.T) {
			t.Helper()
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			deleted, err := database.DeleteParserExcludedSessions(t.Context(), []string{"session"})
			require.NoError(t, err)
			require.Equal(t, 1, deleted)
			other, err := OpenIsolatedContext(t.Context(), database.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			cache.fill.observer.beforeDeletionSweep = func() {
				// The sweep has captured the tombstone, but has yet to take the cache
				// writer. Another handle reimports and serves the same source first.
				seedJournaledUsage(t, other, "session")
				got, err := other.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				require.Equal(t, 100, got.Totals.InputTokens)
			}
			require.NoError(t, cache.sweepDeletionJournal(t.Context(), database))
			assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"), "the sweep must apply its captured tombstone")
			for range 2 {
				assertJournaledDailyUsageLegacy(t, database, filter)
			}
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestUsageJournaledClaimSurvivingInstalls(t *testing.T) {
	for _, scenario := range []string{"backfill", "reused-id"} {
		t.Run(scenario+"/daily", func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			require.NoError(t, database.SoftDeleteSession(t.Context(), "session"))
			versions, revision, err := cache.fill.recheckSourceVersions(t.Context(), []usageSourceVersion{{SessionID: "session"}})
			require.NoError(t, err)
			require.Empty(t, versions)
			_, err = database.RestoreSession(t.Context(), "session")
			require.NoError(t, err)
			// Eviction forces fresh installations after the restore.
			_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
			require.NoError(t, err)
			identity := usageTimezoneIdentityFor(filter.Location(), nil)
			zone, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
			require.NoError(t, err)
			if scenario == "backfill" {
				// A single batch must retain its own installs for the claim;
				// multiple batches get another complete verification afterward.
				snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindActivity)
				require.NoError(t, err)
				// Publish a separate tombstone while the pass builds. Its final
				// sweep provides the boundary after installation, before claiming.
				var published, resumed atomic.Bool
				cache.rollup.observer.beforeInstall = func([]usageRollupBuild) {
					if published.CompareAndSwap(false, true) {
						insertSession(t, database, "tombstone", "project")
						require.NoError(t, database.DeleteSession(t.Context(), "tombstone"))
					}
				}
				cache.fill.observer.beforeDeletionSweep = func() {
					if published.Load() && resumed.CompareAndSwap(false, true) {
						require.NoError(t, cache.fill.deleteNotificationSessions([]string{"session"}, revision))
					}
				}
				require.NoError(t, database.runUsageCacheBackfillPass(t.Context(), time.Now(), snapshot, zone.installSeq))
				require.True(t, resumed.Load(), "resume the captured deletion after the pass installs restored usage")
				cache.rollup.observer.beforeInstall = nil
				cache.fill.observer.beforeDeletionSweep = nil
			} else {
				zone, lost, window, err := cache.captureUsageZoneWindow(t.Context(), identity, filter)
				require.NoError(t, err)
				databaseID, err := database.GetDatabaseID(t.Context())
				require.NoError(t, err)
				snapshot, refresh, ids, trusted, err := database.captureJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, databaseID, zone, lost, window)
				require.NoError(t, err)
				require.True(t, trusted)
				installs, _, err := cache.refreshJournaledUsageWindow(t.Context(), &snapshot, refresh, ids, func(string) {})
				require.NoError(t, err)
				required := installs["session"]
				require.Positive(t, required.ID)
				require.NoError(t, cache.fill.deleteNotificationSessions([]string{"session"}, revision))
				// A different session reuses the highest deleted SQLite row ID.
				seedJournaledUsage(t, database, "replacement")
				peer, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
				require.NoError(t, err)
				peer.Sessions = slices.DeleteFunc(peer.Sessions, func(s usageQuerySession) bool { return s.ID != "replacement" })
				peer.Versions = slices.DeleteFunc(peer.Versions, func(v usageSourceVersion) bool { return v.SessionID != "replacement" })
				fills, err := cache.fill.Ensure(t.Context(), peer.Versions, 0)
				require.NoError(t, err)
				built, _, err := cache.rollup.Ensure(t.Context(), peer, fills, export.NewPricingResolver(peer.PricingRows))
				require.NoError(t, err)
				require.Equal(t, required.ID, built["replacement"].ID)
				require.NotEqual(t, required.InstallRevision, built["replacement"].InstallRevision)
				require.NoError(t, cache.claimUsageZone(t.Context(), identity, zone, snapshot.ChangeRevision, snapshot.PricingIdentity, installs))
			}
			wantTokens := 100
			if scenario == "reused-id" {
				wantTokens = 200
			}
			for range 2 {
				got, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, wantTokens, got.Totals.InputTokens)
			}
		})
	}
}

func TestUsageJournaledBackfillClaimChecksEarlierBatch(t *testing.T) {
	database := testDB(t)
	for i := range usageCacheBackfillBatchSize + 1 {
		seedJournaledUsage(t, database, fmt.Sprintf("session-%03d", i))
	}
	require.NoError(t, database.SoftDeleteSession(t.Context(), "session-000"))
	filter := UsageFilter{Timezone: "UTC"}
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), databaseID)
	require.NoError(t, err)
	_, revision, err := cache.fill.recheckSourceVersions(t.Context(), []usageSourceVersion{{SessionID: "session-000"}})
	require.NoError(t, err)
	_, err = database.RestoreSession(t.Context(), "session-000")
	require.NoError(t, err)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindActivity)
	require.NoError(t, err)
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
	require.NoError(t, err)
	activity, err := database.usageBackfillActivity(t.Context())
	require.NoError(t, err)
	orderUsageBackfillSnapshot(&snapshot, activity)
	firstID := snapshot.Versions[0].SessionID
	require.Equal(t, "session-000", firstID)
	var published atomic.Bool
	cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) {
		if slices.ContainsFunc(builds, func(build usageRollupBuild) bool { return build.SessionID == "session-256" }) && published.CompareAndSwap(false, true) {
			insertSession(t, database, "tombstone", "project")
			require.NoError(t, database.DeleteSession(t.Context(), "tombstone"))
		}
	}
	var resumed atomic.Bool
	cache.fill.observer.beforeDeletionSweep = func() {
		var installed int
		err := cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_rollup_installs i
			JOIN usage_rollup_timezones z ON z.id = i.timezone_id WHERE z.timezone_key = ?`, identity.Key).Scan(&installed)
		require.NoError(t, err)
		if installed == usageCacheBackfillBatchSize+1 && resumed.CompareAndSwap(false, true) {
			require.NoError(t, cache.fill.deleteNotificationSessions([]string{firstID}, revision))
		}
	}
	require.NoError(t, database.runUsageCacheBackfillPass(t.Context(), time.Now(), snapshot, zone.installSeq))
	cache.rollup.observer.beforeInstall = nil
	cache.fill.observer.beforeDeletionSweep = nil
	require.True(t, resumed.Load(), "resume the deletion after every batch installs")
	after, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
	require.NoError(t, err)
	assert.False(t, after.revision.Valid, "a missing first-batch install must prevent the coverage claim")
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 25700, got.Totals.InputTokens)
}

func holdUsageCacheWriter(t *testing.T, cache *usageCache) {
	t.Helper()
	conn, err := cache.db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := conn.ExecContext(context.WithoutCancel(t.Context()), `ROLLBACK`)
		require.NoError(t, err)
	})
}

func TestUsageJournaledEmptyNewTimezoneWhileCacheWriterBusy(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	cache := warmJournaledUsage(t, database, UsageFilter{Timezone: "UTC"})
	holdUsageCacheWriter(t, cache)
	got, err := database.GetDailyUsage(t.Context(), UsageFilter{Timezone: "America/New_York", From: "2026-09-01", To: "2026-09-01"})
	require.NoError(t, err, "an empty report in a new timezone needs no cache write")
	assert.Empty(t, got.Daily)
	assert.Zero(t, got.Totals.InputTokens)
}

func TestUsageJournaledClaimFailureWhileCacheWriterBusy(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	seedJournaledUsage(t, database, "unchanged")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	before, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
	require.NoError(t, err)
	// Project metadata advances the journal without changing the cached facts
	// or the fields baked into daily totals.
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET project = 'renamed' WHERE id = 'session'`)
	require.NoError(t, err)
	holdUsageCacheWriter(t, cache)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err, "a failed coverage claim must leave valid report data readable")
	assert.Equal(t, 200, got.Totals.InputTokens)
	counts, err := database.GetUsageSessionCounts(t.Context(), UsageFilter{Timezone: "UTC", Project: "renamed"})
	require.NoError(t, err)
	assert.Equal(t, 1, counts.Total)
	after, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
	require.NoError(t, err)
	assert.Equal(t, before.revision, after.revision, "a failed claim must leave coverage unchanged")
}

func TestUsageJournaledRefreshDiagnostics(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	assert.Positive(t, prepared.timings.capture)
	assert.Positive(t, prepared.timings.fill)
	assert.Positive(t, prepared.timings.rollup.InstallDuration)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	prepared.timings.logIfSlow(time.Now().Add(-usageRollupSlowRequestThreshold))
	assert.Contains(t, output.String(), "1 candidate sessions; built 1 daily rows")
}

func TestUsageJournaledPartialRefresh(t *testing.T) {
	database := testDB(t)
	for i := range 260 {
		seedJournaledUsage(t, database, fmt.Sprintf("session-%03d", i))
	}
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET local_modified_at='2100-01-01T00:00:00Z'`)
	require.NoError(t, err)
	var extracted atomic.Int64
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		for _, version := range versions {
			assert.Equal(t, "session-000", version.SessionID, "only the appended session needs extraction")
		}
		extracted.Add(int64(len(versions)))
	}
	assertJournaledUsageLegacy(t, database, filter)
	assert.Zero(t, extracted.Load())
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "session-000", Ordinal: 1, Role: "assistant", Model: "journal-model", Timestamp: "2026-08-10T23:31:00Z", TokenUsage: []byte(`{"input_tokens":9}`)}}))
	assertJournaledUsageLegacy(t, database, filter)
	assert.EqualValues(t, 1, extracted.Load())
}

func TestUsageJournaledForeignInstall(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			if empty {
				filter.From = "2026-08-11"
				filter.To = filter.From
			}
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
			require.NoError(t, err)
			require.True(t, prepared.journaled)
			if empty {
				_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='session'`)
				require.NoError(t, err)
			} else {
				_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET project='moved' WHERE id='session'`)
				require.NoError(t, err)
				require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "assistant", Model: "journal-model", Timestamp: "2026-08-10T23:30:00Z", TokenUsage: []byte(`{"input_tokens":900}`)}}))
				snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
				require.NoError(t, err)
				fills, err := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
				require.NoError(t, err)
				_, _, err = cache.rollup.Ensure(t.Context(), snapshot, fills, export.NewPricingResolver(snapshot.PricingRows))
				require.NoError(t, err)
			}
			_, err = cache.usageRollupQuery(t.Context(), prepared, filter, export.NewPricingResolver(prepared.snapshot.PricingRows))
			require.Error(t, err)
			assert.True(t, usageCacheReadShouldRecapture(err))
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledNotificationCapturesNewerMetadata(t *testing.T) {
	for _, field := range []string{"started_at", "agent"} {
		t.Run(field, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			// Untimed session usage is assigned to the session's start day.
			require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "session", []UsageEvent{{Source: "session", Model: "session-model", InputTokens: 300}}))
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			captured, err := database.captureUsageSessions(t.Context(), []string{"session"})
			require.NoError(t, err)
			fills, err := cache.fill.Ensure(t.Context(), captured.Versions, 0)
			require.NoError(t, err)
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
			require.NoError(t, err)
			require.True(t, prepared.journaled)
			require.Equal(t, captured.ChangeRevision, prepared.snapshot.ChangeRevision)
			session, err := database.GetSession(t.Context(), "session")
			require.NoError(t, err)
			require.NotNil(t, session)
			if field == "started_at" {
				session.StartedAt = new("2026-08-11T09:00:00Z")
			} else {
				session.Agent = "claude"
			}
			require.NoError(t, database.UpsertSession(t.Context(), *session))
			// Resume a notification after its fact capture, using its older
			// coverage revision and the metadata refreshUsageRollups reads now.
			database.usageBackfillMu.Lock()
			database.usageBackfillEnabled = true
			database.usageBackfillMu.Unlock()
			database.refreshUsageRollups(t.Context(), cache, fills, captured.ChangeRevision)
			var installedRevision int64
			var agent, startedAt string
			require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT source_change_revision, baked_agent, baked_started_at FROM usage_rollup_installs WHERE session_id='session'`).Scan(&installedRevision, &agent, &startedAt))
			require.Equal(t, captured.ChangeRevision, installedRevision)
			require.Equal(t, session.Agent, agent)
			require.Equal(t, *session.StartedAt, startedAt)
			_, err = cache.usageRollupQuery(t.Context(), prepared, filter, export.NewPricingResolver(prepared.snapshot.PricingRows))
			require.ErrorIs(t, err, errUsageCacheSourceChanged)
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledOlderRollupRepaired(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	old, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), old.Versions, 0)
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var once atomic.Bool
	// Force a rebuild with the old archive metadata.
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='session'`)
	require.NoError(t, err)
	cache.rollup.observer.beforeInstall = func([]usageRollupBuild) {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	done := make(chan error, 1)
	t.Cleanup(func() { unblock(); <-done })
	go func() {
		_, _, err := cache.rollup.Ensure(t.Context(), old, fills, export.NewPricingResolver(old.PricingRows))
		done <- err
		close(done)
	}()
	<-held
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	session, err := other.GetSession(t.Context(), "session")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.Agent = "claude"
	require.NoError(t, other.UpsertSession(t.Context(), *session))
	// A second handle owns its own coordinator, so its newer build and claim
	// finish before the older build installs.
	_, err = other.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NoError(t, other.Close())
	var newerInstallID, newerRevision int64
	var newerAgent string
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT id, source_change_revision, baked_agent FROM usage_rollup_installs WHERE session_id='session'`).Scan(&newerInstallID, &newerRevision, &newerAgent))
	require.Greater(t, newerRevision, old.ChangeRevision)
	require.Equal(t, "claude", newerAgent)
	// Replacing this surviving install must rewind coverage too. The delayed
	// peer-invalidation case instead inserts after the old install is evicted.
	unblock()
	require.NoError(t, <-done)
	var replacedInstallID int64
	var replacedAgent string
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT id, baked_agent FROM usage_rollup_installs WHERE session_id='session'`).Scan(&replacedInstallID, &replacedAgent))
	assert.Equal(t, newerInstallID, replacedInstallID)
	assert.Equal(t, "codex", replacedAgent)
	coverage, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
	require.NoError(t, err)
	require.True(t, coverage.revision.Valid)
	assert.LessOrEqual(t, coverage.revision.Int64, old.ChangeRevision, "replay changes after the older snapshot")
	filter.Agent = "claude"
	assertJournaledUsageLegacy(t, database, filter)
}

func TestUsageJournaledPassUsesCapturedPrices(t *testing.T) {
	for _, revert := range []bool{false, true} {
		t.Run(strconv.FormatBool(revert), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
			// The captured catalog predates both updates, including a rate revert.
			_, err := database.getWriter().ExecContext(t.Context(), `UPDATE model_pricing SET updated_at='2026-08-01T00:00:00Z' WHERE model_pattern='journal-model'`)
			require.NoError(t, err)
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
			require.NoError(t, err)
			snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
			require.NoError(t, err)
			// The archive now has different prices before the pass starts building.
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 2_000_000}}}))
			var once atomic.Bool
			cache.rollup.observer.beforeInstall = func([]usageRollupBuild) {
				if once.CompareAndSwap(false, true) {
					if revert {
						require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
					}
				}
			}
			zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
			require.NoError(t, err)
			require.NoError(t, database.runUsageCacheBackfillPass(t.Context(), time.Now(), snapshot, zone.installSeq))
			require.True(t, once.Load(), "the pass must build before the catalog can revert")
			var capturedCost int64
			require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT SUM(d.estimated_cost_microdollars)
                FROM usage_daily_rollups d JOIN usage_rollup_installs i ON i.id=d.rollup_install_id
                WHERE i.session_id='session' AND i.timezone_id=?`, zone.id).Scan(&capturedCost))
			assert.EqualValues(t, 100, capturedCost)
			_, ok, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
			require.NoError(t, err)
			assert.False(t, ok, "a catalog change invalidates the captured pass, even when rates revert")
			require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
			assertJournaledUsageLegacy(t, database, filter)
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			wantCost := int64(200)
			if revert {
				wantCost = 100
			}
			assert.Equal(t, wantCost, got.Totals.TotalCost.Microdollars)
		})
	}
}

func TestUsageJournaledPassRejectsForeignCatalog(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	// Rebuild an evicted install without advancing the zone claim. The pass
	// captures an install sequence newer than that claim's boundary.
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	_, _, err = cache.rollup.Ensure(t.Context(), snapshot, fills, export.NewPricingResolver(snapshot.PricingRows))
	require.NoError(t, err)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
	require.NoError(t, err)
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	other.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 3_000_000}})
	// Install the peer's catalog without making its own zone claim, so the
	// old pass still matches the compare-and-set and must hit the catalog guard.
	foreign, err := other.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	peerCache, err := other.usageCache.Generation(t.Context(), foreign.DatabaseID)
	require.NoError(t, err)
	peerFills, err := peerCache.fill.Ensure(t.Context(), foreign.Versions, 0)
	require.NoError(t, err)
	_, _, err = peerCache.rollup.Ensure(t.Context(), foreign, peerFills, export.NewPricingResolver(foreign.PricingRows))
	require.NoError(t, err)
	require.NoError(t, other.Close())
	// The old pass reuses its captured catalog; a foreign install after its
	// start prevents that pass from recording a catalog claim.
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	catalog, err := usagePricingIdentity(snapshot.PricingRows)
	require.NoError(t, err)
	var beforePricing string
	var beforeInstallRevision int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT change_pricing, change_install_revision FROM usage_rollup_timezones WHERE timezone_key=?`, identity.Key).Scan(&beforePricing, &beforeInstallRevision))
	require.NotEqual(t, zone.installSeq, beforeInstallRevision, "an old pass would move the claim boundary")
	var foreignInstalls int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_rollup_installs WHERE timezone_id=? AND install_revision>=? AND source_pricing<>?`, zone.id, zone.installSeq, catalog).Scan(&foreignInstalls))
	require.Positive(t, foreignInstalls)
	require.NoError(t, cache.claimUsageZone(t.Context(), identity, zone, snapshot.ChangeRevision, catalog, nil))
	var afterPricing string
	var afterInstallRevision int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT change_pricing, change_install_revision FROM usage_rollup_timezones WHERE timezone_key=?`, identity.Key).Scan(&afterPricing, &afterInstallRevision))
	assert.Equal(t, beforePricing, afterPricing)
	assert.Equal(t, beforeInstallRevision, afterInstallRevision)
	_, ok, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	assertJournaledUsageLegacy(t, database, filter)
}

func TestUsageJournaledTopSharesSummary(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		database := openDailyUsageFixtureDB(t)
		filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
		warmJournaledUsage(t, database, filter)
		var reads atomic.Int64
		database.usageCache.usageWindowObserver.afterRead = func(journaled bool) {
			assert.True(t, journaled, "unfiltered rankings must consume the journaled window")
			reads.Add(1)
		}
		want, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 3)
		require.NoError(t, err)
		got, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.EqualValues(t, 1, reads.Load())
	})
	for _, topFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(topFirst), func(t *testing.T) {
			database := openDailyUsageFixtureDB(t)
			filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", Agent: "claude"}
			warmJournaledUsage(t, database, filter)
			var prepares atomic.Int64
			database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
			ctx := WithUsageWindowMemo(t.Context())
			readTop := func() {
				want, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 3)
				require.NoError(t, err)
				got, err := database.GetTopSessionsByCost(ctx, filter, 3)
				require.NoError(t, err)
				assert.Equal(t, want, got)
			}
			if topFirst {
				readTop()
			}
			want, err := database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			got, err := database.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			readTop()
			assert.EqualValues(t, 1, prepares.Load())
		})
	}
}

func TestUsageJournaledDedupInvalidation(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "original", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	seedUsageSnapshotSession(t, database, "sibling", "project-b", "2026-08-10T10:00:00Z", 0, 20, "model-a")
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	var installed int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_rollup_installs WHERE session_id='original'`).Scan(&installed))
	require.Zero(t, installed)
	assertJournaledUsageLegacy(t, database, filter)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 20, got.Totals.OutputTokens)
}

func TestUsageJournaledDelayedBuildAfterPeerInvalidation(t *testing.T) {
	for _, consumer := range []string{"daily", "matching", "pass"} {
		t.Run(consumer, func(t *testing.T) {
			database := testDB(t)
			seedUsageSnapshotSession(t, database, "original", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
			require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "original", []UsageEvent{{Source: "session", Model: "unique-model", InputTokens: 100}}))
			filter := UsageFilter{From: "2026-08-11", To: "2026-08-11", Timezone: "UTC", SkipSessionCounts: true}
			cache := warmJournaledUsage(t, database, filter)
			// A request or pass retains this metadata while newer reads catch up.
			delayed, err := database.captureUsageQuery(t.Context(), UsageFilter{Timezone: "UTC"}, usageQueryKindToken)
			require.NoError(t, err)
			fills, err := cache.fill.Ensure(t.Context(), delayed.Versions, 0)
			require.NoError(t, err)
			resume := func() {
				_, _, err := cache.rollup.Ensure(t.Context(), delayed, fills, export.NewPricingResolver(delayed.PricingRows))
				require.NoError(t, err)
			}
			if consumer == "pass" {
				zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(delayed.location, nil).Key)
				require.NoError(t, err)
				_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='original'`)
				require.NoError(t, err)
				held, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				var once atomic.Bool
				cache.rollup.observer.beforeEnsure = func() {
					if once.CompareAndSwap(false, true) {
						close(held)
						<-release
					}
				}
				done := make(chan error, 1)
				t.Cleanup(func() { unblock(); <-done })
				go func() {
					done <- database.runUsageCacheBackfillPass(t.Context(), time.Now(), delayed, zone.installSeq)
					close(done)
				}()
				<-held
				resume = func() { unblock(); require.NoError(t, <-done) }
			}
			_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET started_at='2026-08-11T09:00:00Z' WHERE id='original'`)
			require.NoError(t, err)
			first, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Equal(t, 100, first.Totals.InputTokens)
			seedUsageSnapshotSession(t, database, "sibling", "project-b", "2026-08-10T10:00:00Z", 0, 20, "model-a")
			sibling, err := database.captureUsageQuery(t.Context(), UsageFilter{Timezone: "UTC"}, usageQueryKindToken)
			require.NoError(t, err)
			_, err = cache.fill.Ensure(t.Context(), sibling.Versions, 0)
			require.NoError(t, err)
			var remaining int
			require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_rollup_installs WHERE session_id='original'`).Scan(&remaining))
			require.Zero(t, remaining)
			resume()
			coverage, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
			require.NoError(t, err)
			require.True(t, coverage.revision.Valid)
			assert.LessOrEqual(t, coverage.revision.Int64, delayed.ChangeRevision, "replay changes after the delayed snapshot")
			// Each consumer must repair the date without another consumer warming it.
			for range 2 {
				switch consumer {
				case "daily", "pass":
					got, err := database.GetDailyUsage(t.Context(), filter)
					require.NoError(t, err)
					assert.Equal(t, 100, got.Totals.InputTokens)
				case "matching":
					prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, true, nil)
					require.NoError(t, err)
					require.True(t, prepared.journaled)
					facts, err := cache.usageRollupQuery(t.Context(), prepared, filter, export.NewPricingResolver(prepared.snapshot.PricingRows))
					require.NoError(t, err)
					require.Len(t, facts.Groups, 1)
					assert.EqualValues(t, 100, facts.Groups[0].InputTokens)
					got, err := database.GetUsageMatchingSessionCount(t.Context(), filter)
					require.NoError(t, err)
					assert.Equal(t, 1, got)
				}
			}
		})
	}
}

func TestUsageJournaledConcurrentWriter(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-11", Timezone: "UTC", SkipSessionCounts: true}
	cache := warmJournaledUsage(t, database, filter)
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	var journaled atomic.Int64
	database.usageCache.usageWindowObserver.afterRead = func(fast bool) {
		if fast {
			journaled.Add(1)
		}
	}
	writes, results, done := make(chan int), make(chan error), make(chan struct{})
	go func() {
		defer close(done)
		for ordinal := range writes {
			err := other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: ordinal, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":1}`)}})
			if err == nil {
				// Exercise the daemon's normal notification fill and rollup
				// refresh while the foreground request prepares and reads.
				cache.fill.processNotificationBatch(map[string]bool{"session": true}, false)
			}
			results <- err
		}
	}()
	t.Cleanup(func() { close(writes); <-done })
	for ordinal := 1; ordinal <= 100; ordinal++ {
		writes <- ordinal
		got, err := database.GetDailyUsage(t.Context(), filter)
		writeErr := <-results
		require.NoError(t, err)
		require.NoError(t, writeErr)
		assert.GreaterOrEqual(t, got.Totals.InputTokens, 99+ordinal)
		assert.LessOrEqual(t, got.Totals.InputTokens, 100+ordinal)
		before := journaled.Load()
		warm, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, 100+ordinal, warm.Totals.InputTokens)
		assert.Equal(t, before+1, journaled.Load(), "a completed notification must leave a journaled warm read")
	}
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 200, got.Totals.InputTokens)
}

func TestUsageJournaledConcurrentWriterInstallConflicts(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	for i := range 29 {
		seedJournaledUsage(t, database, fmt.Sprintf("session-%02d", i))
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-11", Timezone: "UTC", SkipSessionCounts: true}
	warmJournaledUsage(t, database, filter)
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	writes, results, done := make(chan int), make(chan error), make(chan struct{})
	go func() {
		defer close(done)
		for ordinal := range writes {
			err := other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: ordinal, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":1}`)}})
			if err == nil {
				_, err = other.GetDailyUsage(t.Context(), UsageFilter{Timezone: "UTC", SkipSessionCounts: true})
			}
			results <- err
		}
	}()
	t.Cleanup(func() { close(writes); <-done })
	readerErrors := make(chan error, 10) // Three peer writes and seven readers.
	var reads, conflicts atomic.Int64
	database.usageCache.usageWindowObserver.afterRead = func(journaled bool) {
		if !journaled {
			conflicts.Add(1)
		}
	}
	var readers sync.WaitGroup
	for reader := range 7 {
		filter := filter
		if reader < 3 {
			var once atomic.Bool
			filter.Progress = func(phase string) {
				if phase != "Reading cached daily totals" || !once.CompareAndSwap(false, true) {
					return
				}
				// Each of three distinct readers loses its first prepared install.
				ordinal := reads.Add(1)
				writes <- int(ordinal)
				readerErrors <- <-results
			}
		}
		readers.Go(func() {
			for range 10 {
				got, err := database.GetDailyUsage(t.Context(), filter)
				if err != nil {
					readerErrors <- err
					return
				}
				assert.GreaterOrEqual(t, got.Totals.InputTokens, 3000)
				assert.LessOrEqual(t, got.Totals.InputTokens, 3003)
			}
		})
	}
	readers.Wait()
	close(readerErrors)
	for err := range readerErrors {
		require.NoError(t, err)
	}
	assert.GreaterOrEqual(t, conflicts.Load(), int64(3), "each displaced journaled reader must recapture")
	filter.Progress = nil
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 3003, got.Totals.InputTokens)
}

func TestUsageJournaledUntrustedReadsSharePass(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	_, err := cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_timezones SET change_revision=NULL`)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var once atomic.Bool
	cache.rollup.observer.beforeEnsure = func() {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	var starts atomic.Int64
	database.SetUsageCacheBackfillStarted(func() { starts.Add(1) })
	t.Cleanup(func() { unblock(); database.StopUsageCacheBackfill() })
	_, trusted, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
	require.NoError(t, err)
	assert.False(t, trusted)
	require.EqualValues(t, 1, starts.Load())
	<-held
	for range 10 {
		_, trusted, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
		require.NoError(t, err)
		assert.False(t, trusted)
	}
	unblock()
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	for range 2 {
		prepared, trusted, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
		require.NoError(t, err)
		assert.True(t, trusted)
		assert.True(t, prepared.journaled)
	}
	assert.EqualValues(t, 1, starts.Load())
}

func TestUsageJournaledUnretainedZoneBoundsPasses(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	zones := []string{"Pacific/Honolulu", "America/Anchorage", "America/Los_Angeles", "America/Denver", "America/Chicago", "America/New_York", "Europe/London", "Europe/Paris", "Asia/Tokyo", "Australia/Sydney"}
	for _, zone := range zones {
		got, err := database.GetDailyUsage(t.Context(), UsageFilter{Timezone: zone})
		require.NoError(t, err)
		require.Equal(t, 100, got.Totals.InputTokens)
	}
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	t.Cleanup(database.StopUsageCacheBackfill)
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	var starts atomic.Int64
	database.SetUsageCacheBackfillStarted(func() { starts.Add(1) })
	// This zone is older than the eight explicit zones retained by a pass.
	// Preparation itself doesn't refresh recency; serving the fallback does.
	for range 3 {
		_, trusted, err := database.prepareJournaledUsageWindow(t.Context(), UsageFilter{Timezone: zones[0]}, usageQueryKindToken, false, func(string) {})
		require.NoError(t, err)
		assert.False(t, trusted)
		require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	}
	assert.Zero(t, starts.Load(), "an unretained zone cannot gain coverage from a pass")
}

func TestUsageJournaledWarmSevenDaysReuseInstalledTotals(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-05", To: "2026-08-11", Timezone: "UTC", SkipSessionCounts: true}
	cache := warmJournaledUsage(t, database, filter)
	want, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, 100, want.Totals.InputTokens)
	var fills, builds, reads atomic.Int64
	cache.fill.observer.beforeInstall = func([]usageSourceVersion) { fills.Add(1) }
	cache.rollup.observer.beforeEnsure = func() { builds.Add(1) }
	database.usageCache.usageWindowObserver.afterRead = func(journaled bool) {
		assert.True(t, journaled, "warm reads must skip archive discovery and fact checks")
		reads.Add(1)
	}
	holdUsageCacheWriter(t, cache)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for range 5 {
		got, err := database.GetDailyUsage(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	assert.EqualValues(t, 5, reads.Load(), "every warm report must read the journaled window")
	assert.Zero(t, fills.Load(), "warm reads must reuse cached facts")
	assert.Zero(t, builds.Load(), "warm reads must reuse installed daily totals")
}
