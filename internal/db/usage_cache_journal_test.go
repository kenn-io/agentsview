//go:build fts5

package db

import (
	"context"
	"fmt"
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
	t.Run("missing timezone", func(t *testing.T) {
		var missingTimezoneID int64
		require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT MAX(id)+1 FROM usage_rollup_timezones`).Scan(&missingTimezoneID))
		got, err := usageRollupWindowSessions(t.Context(), cache.db, missingTimezoneID, UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"})
		require.NoError(t, err)
		assert.Empty(t, got)
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
	wantTop, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 10)
	require.NoError(t, err)
	gotTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	assert.Equal(t, wantTop, gotTop)
	wantCounts, err := database.getUsageSessionCountsLegacy(t.Context(), filter)
	require.NoError(t, err)
	gotCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, wantCounts, gotCounts)
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
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
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
			prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindToken, false, nil)
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

func TestUsageJournaledOlderRollupCannotReplaceNewer(t *testing.T) {
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
	// A second handle owns its own coordinator, so its newer build can finish.
	_, err = other.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	require.NoError(t, other.Close())
	var newerRevision int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT source_change_revision FROM usage_rollup_installs WHERE session_id='session'`).Scan(&newerRevision))
	unblock()
	buildErr := <-done
	var installedRevision int64
	var agent string
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT source_change_revision, baked_agent FROM usage_rollup_installs WHERE session_id='session'`).Scan(&installedRevision, &agent))
	assert.Equal(t, newerRevision, installedRevision)
	assert.Equal(t, "claude", agent)
	require.ErrorIs(t, buildErr, errUsageCacheSourceChanged)
	var rebuilt atomic.Int64
	cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) { rebuilt.Add(int64(len(builds))) }
	assertJournaledUsageLegacy(t, database, filter)
	assert.Zero(t, rebuilt.Load())
}

func TestUsageJournaledDelayedFillPreservesCurrentRevision(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	old, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "session", Ordinal: 1, Role: "assistant", Model: "journal-model",
		Timestamp: "2026-08-11T00:30:00Z", TokenUsage: []byte(`{"input_tokens":7}`),
	}}))
	current, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), current.Versions, 0)
	require.NoError(t, err)
	require.NotEqual(t, old.Versions[0].TranscriptRevision, current.Versions[0].TranscriptRevision)
	delayed, err := cache.fill.FillBackground(t.Context(), old.Versions, 0)
	require.NoError(t, err)
	assert.Equal(t, fills["session"], delayed["session"], "extracting the current source again must preserve its installed revision")
}

func TestUsageJournaledSupersededBackgroundBuild(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	seedJournaledUsage(t, database, "peer")
	filter := UsageFilter{Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	old, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), old.Versions, 0)
	require.NoError(t, err)
	session, err := database.GetSession(t.Context(), "session")
	require.NoError(t, err)
	require.NotNil(t, session)
	session.Agent = "claude"
	require.NoError(t, database.UpsertSession(t.Context(), *session))
	_, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	var newerRevision int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT source_change_revision FROM usage_rollup_installs WHERE session_id='session'`).Scan(&newerRevision))
	require.Greater(t, newerRevision, old.ChangeRevision)
	// The same batch must still install a peer that needs work.
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='peer'`)
	require.NoError(t, err)
	_, err = ensureUsageRollupsAcross(t.Context(), cache, old, fills,
		export.NewPricingResolver(old.PricingRows), []*time.Location{time.UTC})
	require.NoError(t, err)
	var installedRevision int64
	var agent string
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT source_change_revision, baked_agent FROM usage_rollup_installs WHERE session_id='session'`).Scan(&installedRevision, &agent))
	assert.Equal(t, newerRevision, installedRevision)
	assert.Equal(t, "claude", agent)
	var peerTokens int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT SUM(d.input_tokens)
		FROM usage_daily_rollups d JOIN usage_rollup_installs i ON i.id=d.rollup_install_id
		JOIN usage_rollup_timezones z ON z.id=i.timezone_id
		WHERE i.session_id='peer' AND z.timezone_key=?`, usageTimezoneIdentityFor(time.UTC, nil).Key).Scan(&peerTokens))
	assert.EqualValues(t, 100, peerTokens)
}

func TestUsageJournaledPassUsesCapturedPrices(t *testing.T) {
	for _, revert := range []bool{false, true} {
		t.Run(strconv.FormatBool(revert), func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			require.NoError(t, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}}}))
			filter := UsageFilter{Timezone: "UTC"}
			cache := warmJournaledUsage(t, database, filter)
			_, err := cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
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
			assert.Equal(t, revert, ok, "only the reverted catalog can use the journaled result")
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
	require.NoError(t, cache.claimUsageZone(t.Context(), identity, zone, snapshot.ChangeRevision, catalog))
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
		want, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 3)
		require.NoError(t, err)
		got, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
		require.NoError(t, err)
		assert.Equal(t, want, got)
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
	for _, consumer := range []string{"daily", "ranking", "counts", "matching", "pass"} {
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
				case "ranking":
					got, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
					require.NoError(t, err)
					assert.Len(t, got, 1)
				case "counts":
					got, err := database.GetUsageSessionCounts(t.Context(), filter)
					require.NoError(t, err)
					want, err := database.getUsageSessionCountsLegacy(t.Context(), filter)
					require.NoError(t, err)
					assert.Equal(t, want, got)
				case "matching":
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
	filter.Progress = func(phase string) {
		if phase != "Reading cached daily totals" {
			return
		}
		ordinal := reads.Add(1)
		if ordinal > 3 {
			return
		}
		// Hold prepared readers until the peer has replaced their installs.
		writes <- int(ordinal)
		readerErrors <- <-results
		conflicts.Add(1)
	}
	var readers sync.WaitGroup
	for range 7 {
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
	assert.EqualValues(t, 3, conflicts.Load())
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
