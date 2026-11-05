//go:build fts5

package db

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

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
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	cache, err := database.usageCache.Generation(t.Context(), prepared.snapshot.DatabaseID)
	require.NoError(t, err)
	return cache
}

func assertJournaledUsageLegacy(t *testing.T, database *DB, filter UsageFilter) {
	t.Helper()
	want, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestUsageJournaledFreshness(t *testing.T) {
	for _, name := range []string{"append", "soft delete", "hard delete", "restore", "start date", "message date", "agent", "price", "second handle"} {
		t.Run(name, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-11", Timezone: "UTC"}
			if name == "agent" {
				filter.Agent = "claude"
			}
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
			if name == "start date" {
				require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", nil))
				require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "session", []UsageEvent{{Source: "session", Model: "journal-model", InputTokens: 100}}))
			}
			if name == "restore" {
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
			case "price":
				require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 2_000_000}}}))
			case "second handle":
				other, err := OpenIsolatedContext(t.Context(), database.Path())
				require.NoError(t, err)
				require.NoError(t, other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T01:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
				require.NoError(t, other.Close())
			}
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledTrustBoundaries(t *testing.T) {
	for _, name := range []string{"no pass", "new generation", "revision above archive", "custom pricing"} {
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
			case "no pass":
				_, err := cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_timezones SET change_revision=NULL`)
				require.NoError(t, err)
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
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, nil)
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
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) { extracted.Add(int64(len(versions))) }
	assertJournaledUsageLegacy(t, database, filter)
	assert.Zero(t, extracted.Load())
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "session-000", Ordinal: 1, Role: "assistant", Model: "journal-model", Timestamp: "2026-08-10T23:31:00Z", TokenUsage: []byte(`{"input_tokens":9}`)}}))
	assertJournaledUsageLegacy(t, database, filter)
	assert.EqualValues(t, 1, extracted.Load())
}

func TestUsageJournaledForeignInstall(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			if empty {
				filter.From = "2026-08-11"
				filter.To = filter.From
			}
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, nil)
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
			_, err = cache.usageRollupActivityQuery(t.Context(), prepared, filter)
			require.Error(t, err)
			assert.True(t, usageCacheReadShouldRecapture(err))
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledOlderInstallRepairs(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	old, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), old.Versions, 0)
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
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
	go func() {
		_, _, err := cache.rollup.Ensure(t.Context(), old, fills, export.NewPricingResolver(old.PricingRows))
		done <- err
	}()
	<-held
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	_, err = other.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='session'`)
	require.NoError(t, err)
	// A second handle owns its own coordinator, so its newer build can finish.
	_, err = other.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NoError(t, other.Close())
	close(release)
	require.NoError(t, <-done)
	assertJournaledUsageLegacy(t, database, filter)
}

func TestUsageJournaledPassCannotVouchForPrices(t *testing.T) {
	for _, revert := range []bool{false, true} {
		t.Run(fmt.Sprint(revert), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			_, err := cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
			require.NoError(t, err)
			snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
			require.NoError(t, err)
			var once atomic.Bool
			cache.rollup.observer.beforeInstall = func([]usageRollupBuild) {
				if once.CompareAndSwap(false, true) {
					require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 2_000_000}}}))
					if revert {
						require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
					}
				}
			}
			zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
			require.NoError(t, err)
			require.NoError(t, database.runUsageCacheBackfillPass(t.Context(), time.Now(), snapshot, zone.installSeq))
			prepared, ok, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
			require.NoError(t, err)
			assert.False(t, ok)
			assert.False(t, prepared.journaled)
			require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
			assertJournaledUsageLegacy(t, database, filter)
		})
	}
}

func TestUsageJournaledBatchesCheckCursorOnce(t *testing.T) {
	database := testDB(t)
	for i := range 260 {
		seedJournaledUsage(t, database, fmt.Sprintf("session-%03d", i))
	}
	filter := UsageFilter{Timezone: "UTC"}
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "journal-model", InputTokens: 7, DedupKey: "cursor"}}))
	cache := warmJournaledUsage(t, database, filter)
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude'`)
	require.NoError(t, err)
	var checks atomic.Int64
	cache.fill.observer.beforeCursorCheck = func(int64) { checks.Add(1) }
	var sizes []int
	cache.rollup.observer.beforeAttempt = func(fills map[string]usageFillResult) { sizes = append(sizes, len(fills)) }
	assertJournaledUsageLegacy(t, database, filter)
	assert.EqualValues(t, 1, checks.Load())
	assert.Equal(t, []int{256, 4}, sizes)
}

func TestUsageJournaledPassRejectsForeignCatalog(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(filter.Location(), nil).Key)
	require.NoError(t, err)
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	other.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 3_000_000}})
	_, err = other.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NoError(t, other.Close())
	// The old pass reuses its captured catalog; a foreign install after its
	// start prevents that pass from recording a catalog claim.
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	catalog, err := usagePricingIdentity(snapshot.PricingRows)
	require.NoError(t, err)
	require.NoError(t, cache.claimUsageZone(t.Context(), identity, zone, snapshot.ChangeRevision, catalog))
	prepared, ok, err := database.prepareJournaledUsageWindow(t.Context(), filter, usageQueryKindToken, false, func(string) {})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.False(t, prepared.journaled)
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	assertJournaledUsageLegacy(t, database, filter)
}

func TestUsageJournaledTopMetadata(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	want, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 3)
	require.NoError(t, err)
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
