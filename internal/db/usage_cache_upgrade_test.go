//go:build fts5

package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func makeFormat23Cache(t *testing.T, path string) {
	t.Helper()
	db := openRawUsageCacheTestDB(t, path)
	for _, query := range []string{
		`ALTER TABLE usage_facts DROP COLUMN ranking_output_tokens`,
		`ALTER TABLE usage_rollup_exceptions DROP COLUMN ranking_output_tokens`,
		`DROP TABLE usage_rollup_account_totals`,
		`DELETE FROM usage_cache_metadata WHERE key IN ('reset_epoch', 'source_mutation_revision')`,
		`UPDATE usage_cache_metadata SET value='23' WHERE key='format_version'`,
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	// Compare against the schema shipped by the format 23 writer, not the
	// current schema from which this populated fixture was derived.
	schema, err := os.ReadFile("testdata/usage-cache-v23-schema.sql")
	require.NoError(t, err)
	reference := openRawUsageCacheTestDB(t, filepath.Join(t.TempDir(), "v23.db"))
	_, err = reference.ExecContext(t.Context(), string(schema))
	require.NoError(t, err)
	objects := `SELECT type, name, tbl_name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' AND name != 'padding' ORDER BY type, name`
	assert.Equal(t, usageSpoolFactValues(t, reference, objects), usageSpoolFactValues(t, db, objects))
	rows, err := reference.QueryContext(t.Context(), `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	require.NoError(t, err)
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	for _, table := range tables {
		query := `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`
		assert.Equal(t, usageSpoolFactValues(t, reference, query, table), usageSpoolFactValues(t, db, query, table), table)
	}
	require.NoError(t, reference.Close())
	require.NoError(t, db.Close())
}

func TestUsageCacheUpgradePreservesWarmFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := openCopiedTestDB(t, path)
	require.NoError(t, err)
	seedJournaledUsage(t, database, "one")
	seedJournaledUsage(t, database, "two")
	seedUsageSnapshotSession(t, database, "earlier", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, database, "later", "project-b", "2026-08-10T10:00:00Z", 0, 20, "model-a")
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T11:00:00Z", Model: "cursor-model", InputTokens: 30, DedupKey: "cursor"}}))
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true, PairwiseSides: true}
	cache := warmJournaledUsage(t, database, filter)
	id, current := cache.databaseID, cache.path
	before, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	top, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	counts, err := database.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	facts := usageSpoolFactValues(t, cache.db, `SELECT cached_session_id, fact_index, input_tokens, output_tokens FROM usage_facts ORDER BY cached_session_id, fact_index`)
	revisions := usageSpoolFactValues(t, cache.db, `SELECT session_id, install_revision FROM usage_cached_sessions ORDER BY session_id`)
	require.NotEmpty(t, facts)
	require.NoError(t, database.Close())
	makeFormat23Cache(t, current)
	old := usageCacheGenerationPath(path, 23, id)
	require.NoError(t, os.Rename(current, old))
	database, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	cache, err = database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	var extracted []usageSourceVersion
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) { extracted = append(extracted, versions...) }
	after, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	afterTop, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	assert.Equal(t, top, afterTop)
	afterCounts, err := database.GetUsageSessionCounts(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, counts, afterCounts)
	assert.Empty(t, extracted)
	assert.Equal(t, facts, usageSpoolFactValues(t, cache.db, `SELECT cached_session_id, fact_index, input_tokens, output_tokens FROM usage_facts ORDER BY cached_session_id, fact_index`))
	assert.Equal(t, revisions, usageSpoolFactValues(t, cache.db, `SELECT session_id, install_revision FROM usage_cached_sessions ORDER BY session_id`))
	probe := probeUsageCache(t.Context(), cache.path)
	require.NoError(t, probe.Err)
	assert.True(t, probe.Compatible)
	var appID, vacuum int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA application_id`).Scan(&appID))
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `PRAGMA auto_vacuum`).Scan(&vacuum))
	assert.Equal(t, usageCacheApplicationID, appID)
	assert.Equal(t, 2, vacuum)
	assert.NoFileExists(t, old)
	filter.PairwiseSides = false
	assertJournaledUsageLegacy(t, database, filter)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "one", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:40:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
	require.NoError(t, database.SoftDeleteSession(t.Context(), "two"))
	seedUsageSnapshotSession(t, database, "new-peer", "project-c", "2026-08-10T11:30:00Z", 0, 30, "model-a")
	database.SetCustomPricing(map[string]config.CustomModelRate{"journal-model": {InputMicrodollarsPerMTok: 1000000}})
	assertJournaledUsageLegacy(t, database, filter)
}

func TestUsageCacheUpgradeRejectsInvalidCandidate(t *testing.T) {
	for _, variant := range []string{"source", "incomplete", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "archive.db")
			old := usageCacheGenerationPath(archive, 23, "id")
			cache, err := initializeUsageCache(t.Context(), old, "id", false)
			require.NoError(t, err)
			require.NoError(t, cache.db.Close())
			makeFormat23Cache(t, old)
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
			seeded, err := seedUpgradedUsageCache(ctx, archive, candidate, "id")
			assert.False(t, seeded)
			require.Error(t, err)
			if variant == "cancelled" {
				assert.ErrorIs(t, err, context.Canceled)
			} else {
				manager := newUsageCacheManager(archive)
				t.Cleanup(func() { require.NoError(t, manager.Close()) })
				opened, err := manager.Generation(t.Context(), "id")
				require.NoError(t, err)
				assert.False(t, opened.temporary)
				assert.True(t, probeUsageCache(t.Context(), opened.path).Compatible)
			}
		})
	}
}

func TestUsageCacheUpgradeConcurrentPublishAndLease(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive.db")
	old := usageCacheGenerationPath(archive, 23, "id")
	cache, err := initializeUsageCache(t.Context(), old, "id", false)
	require.NoError(t, err)
	require.NoError(t, cache.db.Close())
	makeFormat23Cache(t, old)
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
			opened, err := manager.Generation(t.Context(), "id")
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
