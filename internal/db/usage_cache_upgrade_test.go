//go:build fts5

package db

import (
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageCacheUpgradePreservesWarmFacts(t *testing.T) {
	path, old, id := copyUsageV23Fixture(t)
	oldDB := openRawUsageCacheTestDB(t, old)
	factsQuery := `SELECT 0, cached_session_id, fact_index, input_tokens, output_tokens FROM usage_facts ORDER BY cached_session_id, fact_index`
	revisionsQuery := `SELECT 0, session_id, install_revision FROM usage_cached_sessions ORDER BY session_id`
	facts := usageSpoolFactValues(t, oldDB, factsQuery)
	revisions := usageSpoolFactValues(t, oldDB, revisionsQuery)
	require.NotEmpty(t, facts)
	require.NoError(t, oldDB.Close())
	database, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	var extracted []usageSourceVersion
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) { extracted = append(extracted, versions...) }
	var installed int
	cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) { installed += len(builds) }
	freshPath, freshOld, _ := copyUsageV23Fixture(t)
	require.NoError(t, os.Remove(freshOld))
	fresh, err := Open(t.Context(), freshPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	compare := func() {
		t.Helper()
		want, err := fresh.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		got, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		wantTop, err := fresh.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		gotTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		assert.Equal(t, wantTop, gotTop)
		wantCounts, err := fresh.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		gotCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, wantCounts, gotCounts)
	}
	compare()
	assert.Empty(t, extracted)
	assert.Zero(t, installed)
	assert.Equal(t, facts, usageSpoolFactValues(t, cache.db, factsQuery))
	assert.Equal(t, revisions, usageSpoolFactValues(t, cache.db, revisionsQuery))
	probe := probeUsageCache(t.Context(), cache.path)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Compatible)
	var appID, vacuum int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA application_id`).Scan(&appID))
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA auto_vacuum`).Scan(&vacuum))
	assert.Equal(t, usageCacheApplicationID, appID)
	assert.Equal(t, 2, vacuum)
	assert.NoFileExists(t, old)
	freshCache, err := fresh.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	assertUsageCacheSchema(t, freshCache.db, cache.db)
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.InsertMessages(t.Context(), []Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:40:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
		require.NoError(t, db.SoftDeleteSession(t.Context(), "two"))
		seedUsageSnapshotSession(t, db, "new-peer", "project-c", "2026-08-10T11:30:00Z", 0, 30, "model-a")
		db.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 1000000}})
	}
	compare()
	var ids []string
	for _, version := range extracted {
		ids = append(ids, version.SessionID)
	}
	assert.ElementsMatch(t, []string{"one", "new-peer"}, ids)
}

func assertUsageCacheSchema(t *testing.T, fresh, upgraded *sql.DB) {
	t.Helper()
	objects := `SELECT 0, type, name, tbl_name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`
	freshObjects := usageSpoolFactValues(t, fresh, objects)
	assert.Equal(t, freshObjects, usageSpoolFactValues(t, upgraded, objects))
	for _, object := range freshObjects {
		kind, name := object[0].(string), object[1].(string)
		var queries []string
		switch kind {
		case "table":
			queries = []string{
				`SELECT 0, name, type, "notnull", CASE WHEN name='cost_status' THEN NULL ELSE dflt_value END, pk FROM pragma_table_info(?) ORDER BY name`,
				`SELECT 0, id, seq, "table", "from", "to", on_update, on_delete, match FROM pragma_foreign_key_list(?) ORDER BY id, seq`,
				`SELECT 0, name, "unique", origin, partial FROM pragma_index_list(?) ORDER BY name`,
			}
		}
		for _, query := range queries {
			assert.Equal(t, usageSpoolFactValues(t, fresh, query, name), usageSpoolFactValues(t, upgraded, query, name), name)
		}
	}
	indexesQuery := `SELECT 0, name FROM sqlite_schema WHERE type='index' ORDER BY name`
	indexes := usageSpoolFactValues(t, fresh, indexesQuery)
	assert.Equal(t, indexes, usageSpoolFactValues(t, upgraded, indexesQuery))
	for _, index := range indexes {
		query := `SELECT 0, seqno, name, "desc", coll, key FROM pragma_index_xinfo(?) ORDER BY seqno`
		name := index[0].(string)
		assert.Equal(t, usageSpoolFactValues(t, fresh, query, name), usageSpoolFactValues(t, upgraded, query, name), name)
		sqlQuery := `SELECT 0, sql FROM sqlite_schema WHERE type='index' AND name=?`
		assert.Equal(t, usageSpoolFactValues(t, fresh, sqlQuery, name), usageSpoolFactValues(t, upgraded, sqlQuery, name), name)
	}
}

func TestUsageCacheUpgradeRejectsInvalidCandidate(t *testing.T) {
	for _, variant := range []string{"source", "incomplete", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			archive, old, id := copyUsageV23Fixture(t)
			var err error
			db := openRawUsageCacheTestDB(t, old)
			switch variant {
			case "source":
				_, err = db.ExecContext(t.Context(), `UPDATE usage_cache_metadata SET value='other' WHERE key='source_database_id'`)
			case "incomplete":
				_, err = db.ExecContext(t.Context(), `DROP TABLE usage_facts`)
			}
			require.NoError(t, err)
			require.NoError(t, db.Close())
			ctx := t.Context()
			if variant == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			candidate := filepath.Join(filepath.Dir(archive), "candidate.db")
			seeded, err := seedUpgradedUsageCache(ctx, archive, candidate, id)
			assert.False(t, seeded)
			if variant == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
				assert.NoFileExists(t, candidate)
				_, _, err = publishUsageCache(ctx, archive, usageCacheGenerationPath(archive, 24, id), id)
				require.ErrorIs(t, err, context.Canceled)
				candidates, err := filepath.Glob(filepath.Join(filepath.Dir(archive), ".usage-cache-init-*"))
				require.NoError(t, err)
				assert.Empty(t, candidates)
			} else {
				require.NoError(t, err)
				manager := newUsageCacheManager(archive)
				t.Cleanup(func() { require.NoError(t, manager.Close()) })
				opened, err := manager.Generation(t.Context(), id)
				require.NoError(t, err)
				assert.False(t, opened.temporary)
				assert.True(t, probeUsageCache(t.Context(), opened.path).Compatible)
			}
		})
	}
}

func TestUsageCacheUpgradeConcurrentPublishAndLease(t *testing.T) {
	archive, old, id := copyUsageV23Fixture(t)
	lease, err := acquireUsageCacheLease(old)
	require.NoError(t, err)
	managers := []*usageCacheManager{newUsageCacheManager(archive), newUsageCacheManager(archive)}
	t.Cleanup(func() {
		for _, manager := range managers {
			require.NoError(t, manager.Close())
		}
	})
	errs := make(chan error, len(managers))
	paths := make(chan string, len(managers))
	for _, manager := range managers {
		go func() {
			opened, err := manager.Generation(t.Context(), id)
			if err == nil {
				paths <- opened.path
			}
			errs <- err
		}()
	}
	for range managers {
		require.NoError(t, <-errs)
	}
	first, second := <-paths, <-paths
	assert.Equal(t, first, second)
	assert.FileExists(t, old)
	require.NoError(t, lease.Close())
	retired, err := retireUsageCacheGeneration(t.Context(), archive, old)
	require.NoError(t, err)
	assert.True(t, retired)
}

func copyUsageV23Fixture(t *testing.T) (string, string, string) {
	t.Helper()
	return copyUsageFixture(t, 23)
}

func copyUsageFixture(t *testing.T, format int) (string, string, string) {
	t.Helper()
	return copyUsageFixtureNamed(t, format, "")
}

func copyUsageFixtureNamed(t *testing.T, format int, prefix string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"archive.db", "cache.db"} {
		input, err := os.Open(filepath.Join(fmt.Sprintf("testdata/usage-cache-v%d", format), prefix+name+".gz"))
		require.NoError(t, err)
		reader, err := gzip.NewReader(input)
		require.NoError(t, err)
		output, err := os.Create(filepath.Join(dir, name))
		require.NoError(t, err)
		_, err = io.Copy(output, reader)
		require.NoError(t, err)
		require.NoError(t, output.Close())
		require.NoError(t, reader.Close())
		require.NoError(t, input.Close())
	}
	archive := filepath.Join(dir, "archive.db")
	cache := filepath.Join(dir, "cache.db")
	db := openRawUsageCacheTestDB(t, cache)
	id := readUsageCacheMetadata(t, db)[usageCacheMetadataSourceDatabaseID]
	require.NotEmpty(t, id)
	require.NoError(t, db.Close())
	old := usageCacheGenerationPath(archive, format, id)
	require.NoError(t, os.Rename(cache, old))
	return archive, old, id
}

func TestUsageCacheUpgradeSkipsCorruptNewer(t *testing.T) {
	archive, old21, id := copyUsageFixture(t, 21)
	prior := openRawUsageCacheTestDB(t, old21)
	var facts int
	require.NoError(t, prior.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&facts))
	require.Positive(t, facts)
	require.NoError(t, prior.Close())

	_, old23, _ := copyUsageV23Fixture(t)
	newer := openRawUsageCacheTestDB(t, old23)
	_, err := newer.ExecContext(t.Context(), `UPDATE usage_cache_metadata SET value = ? WHERE key = ?`, id, usageCacheMetadataSourceDatabaseID)
	require.NoError(t, err)
	var root, pageSize int64
	require.NoError(t, newer.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE name='usage_facts'`).Scan(&root))
	require.NoError(t, newer.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize))
	require.NoError(t, newer.Close())
	newerPath := usageCacheGenerationPath(archive, 23, id)
	require.NoError(t, os.Rename(old23, newerPath))
	file, err := os.OpenFile(newerPath, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, (root-1)*pageSize)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	probe := probeUsageCache(t.Context(), newerPath)
	require.NoError(t, probe.Err)
	require.True(t, probe.Recognized)

	database, err := Open(t.Context(), archive)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, cache.temporary)
	var got int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&got))
	assert.Equal(t, facts, got)
	var extracted []usageSourceVersion
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) { extracted = append(extracted, versions...) }
	_, err = database.GetDailyUsage(t.Context(), UsageFilter{Timezone: "UTC"})
	require.NoError(t, err)
	assert.Empty(t, extracted)
}

func TestUsageCacheUpgradeCorruptFactsRebuilds(t *testing.T) {
	archive, old, id := copyUsageV23Fixture(t)
	db := openRawUsageCacheTestDB(t, old)
	var root, pageSize int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE name='usage_facts'`).Scan(&root))
	require.NoError(t, db.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize))
	require.NoError(t, db.Close())
	file, err := os.OpenFile(old, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, (root-1)*pageSize)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	probe := probeUsageCache(t.Context(), old)
	require.NoError(t, probe.Err)
	require.True(t, probe.Recognized)
	manager := newUsageCacheManager(archive)
	cache, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, cache.temporary)
	assert.NoFileExists(t, old)
	path := cache.path
	require.NoError(t, manager.Close())
	manager = newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	again, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, again.temporary)
	assert.Equal(t, path, again.path)
}

func TestUsageCacheUpgradeReleasedFacts(t *testing.T) {
	path, old, id := copyUsageFixture(t, 21)
	prior := openRawUsageCacheTestDB(t, old)
	var facts int
	require.NoError(t, prior.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&facts))
	require.Positive(t, facts)
	require.NoError(t, prior.Close())
	database, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	var got int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&got))
	assert.Equal(t, facts, got)
	var zones int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_rollup_timezones`).Scan(&zones))
	assert.Zero(t, zones)
	assert.Empty(t, readUsageCacheMetadata(t, cache.db)[usageCacheMetadataBackfillCompletedAt])
	var extracted []string
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		for _, version := range versions {
			extracted = append(extracted, version.SessionID)
		}
	}
	freshPath, freshOld, _ := copyUsageFixture(t, 21)
	require.NoError(t, os.Remove(freshOld))
	fresh, err := Open(t.Context(), freshPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	price := func(rate int64) {
		for _, db := range []*DB{database, fresh} {
			db.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: rate}})
		}
	}
	check := func(input, output int, cost int64, wantIDs ...string) {
		t.Helper()
		want, err := fresh.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		result, err := database.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, want, result)
		assert.Equal(t, input, result.Totals.InputTokens)
		assert.Equal(t, output, result.Totals.OutputTokens)
		assert.Equal(t, cost, result.Totals.TotalCost.Microdollars)
		wantTop, err := fresh.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		gotTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		assert.Equal(t, wantTop, gotTop)
		wantCounts, err := fresh.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		gotCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, wantCounts, gotCounts)
		assert.ElementsMatch(t, wantIDs, extracted)
		extracted = nil
	}
	price(1_000_000)
	check(330, 100, 300)
	var exceptions int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_rollup_exceptions WHERE cost_status='' AND request_scoped IN (0, 1)`).Scan(&exceptions))
	assert.Positive(t, exceptions)
	freshCache, err := fresh.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	assertUsageCacheSchema(t, freshCache.db, cache.db)
	price(2_000_000)
	check(330, 100, 600)
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.InsertMessages(t.Context(), []Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:40:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
	}
	check(380, 100, 700, "one")
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.SoftDeleteSession(t.Context(), "two"))
	}
	check(280, 80, 500)
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.ReplaceSessionMessages(t.Context(), "one", []Message{
			{SessionID: "one", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T23:30:00Z", Model: "unknown-recorded-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20,"recorded_cost_microdollars":7000}`)},
			{SessionID: "one", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:40:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)},
		}))
	}
	check(280, 80, 7300, "one")
	cost := money.Money{Microdollars: 3000}
	for _, db := range []*DB{database, fresh} {
		require.NoError(t, db.ReplaceSessionUsageEvents(t.Context(), "event-owner", []UsageEvent{{Source: "session", Model: "unknown-event-model", Cost: &cost, CostStatus: "recorded", OccurredAt: "2026-08-10T12:00:00Z", DedupKey: "event"}}))
	}
	check(280, 80, 10300, "event-owner")
	require.NoError(t, database.Close())
	database, err = Open(t.Context(), path)
	require.NoError(t, err)
	cache, err = database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		for _, version := range versions {
			extracted = append(extracted, version.SessionID)
		}
	}
	price(2_000_000)
	check(280, 80, 10300)
}

func TestUsageCacheUpgradeFormat22(t *testing.T) {
	archive, old, id := copyUsageFixture(t, 21)
	db := openRawUsageCacheTestDB(t, old)
	_, err := db.ExecContext(t.Context(), `DROP INDEX usage_facts_claude_identity; `+usageCacheClaudeIdentityIndexSQL+` UPDATE usage_cache_metadata SET value='22' WHERE key='format_version'`)
	require.NoError(t, err)
	var facts int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&facts))
	require.NoError(t, db.Close())
	old22 := usageCacheGenerationPath(archive, 22, id)
	require.NoError(t, os.Rename(old, old22))
	manager := newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	cache, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	var actual, zones int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_facts`).Scan(&actual))
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_rollup_timezones`).Scan(&zones))
	assert.Equal(t, facts, actual)
	assert.Zero(t, zones)
	assert.NoFileExists(t, old22)
}

func TestUsageCacheUpgradeRecordedCosts(t *testing.T) {
	for _, format := range []int{21, 22} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			path, old, id := copyUsageFixtureNamed(t, 21, "costs-")
			if format == 22 {
				db := openRawUsageCacheTestDB(t, old)
				_, err := db.ExecContext(t.Context(), `DROP INDEX usage_facts_claude_identity; `+usageCacheClaudeIdentityIndexSQL+` UPDATE usage_cache_metadata SET value='22' WHERE key='format_version'`)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				require.NoError(t, os.Rename(old, usageCacheGenerationPath(path, 22, id)))
			}
			freshPath, freshOld, _ := copyUsageFixtureNamed(t, 21, "costs-")
			require.NoError(t, os.Remove(freshOld))
			database, err := Open(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			fresh, err := Open(t.Context(), freshPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fresh.Close()) })
			for _, db := range []*DB{database, fresh} {
				db.SetCustomPricing(map[string]config.CustomModelRate{"test-priced-model": {InputMicrodollarsPerMTok: 1_000_000}})
			}
			cache, err := database.usageCache.Generation(t.Context(), id)
			require.NoError(t, err)
			var extracted []string
			cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
				for _, version := range versions {
					extracted = append(extracted, version.SessionID)
				}
			}
			filter := UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
			want, err := fresh.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.EqualValues(t, 19200, got.Totals.TotalCost.Microdollars)
			assert.ElementsMatch(t, []string{"recorded-event", "exact-event", "recorded-message", "unpriced-message"}, extracted)
			rows := usageSpoolFactValues(t, cache.db, `SELECT 0, s.session_id, f.cost_status FROM usage_facts f JOIN usage_cached_sessions s ON s.id=f.cached_session_id WHERE s.session_id IN ('recorded-event', 'exact-event', 'recorded-message', 'unpriced-message') ORDER BY s.session_id`)
			assert.Equal(t, [][]any{{"exact-event", "exact"}, {"recorded-event", "recorded"}, {"recorded-message", "recorded"}, {"unpriced-message", "recorded"}}, rows)
		})
	}
}
