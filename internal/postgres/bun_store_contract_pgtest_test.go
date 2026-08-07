//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storetest"
)

const bunCoreContractSchema = "agentsview_bun_core_contract"
const bunIdentityContractSchema = "agentsview_bun_identity_contract"
const bunDataContractSchema = "agentsview_bun_data_contract"
const bunCurationContractSchema = "agentsview_bun_curation_contract"
const bunInsightContractSchema = "agentsview_bun_insight_contract"
const bunMutationContractSchema = "agentsview_bun_mutation_contract"
const bunRecallContractSchema = "agentsview_bun_recall_contract"
const bunUsageContractSchema = "agentsview_bun_usage_contract"
const bunOptionalUsageContractSchema = "agentsview_bun_optional_usage_contract"
const bunPricingWriteContractSchema = "agentsview_bun_pricing_write_contract"
const bunAnalyticsContractSchema = "agentsview_bun_analytics_contract"

func TestBunStoreCoreContract(t *testing.T) {
	storetest.RunCoreContract(t, storetest.Backend{
		Name: "postgres",
		Open: func(t *testing.T) storetest.CoreStore {
			pgURL := testPGURL(t)
			cleanupBunCoreContractSchema(t, pgURL)
			t.Cleanup(func() { cleanupBunCoreContractSchema(t, pgURL) })
			pg, err := Open(pgURL, bunCoreContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunCoreContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			require.NoError(t, storetest.InsertBunCoreFixture(
				t.Context(), store.bun, "pg-contract-archive", "pg-contract-generation",
			))
			return store.BunStore
		},
		Seed: func(*testing.T, storetest.CoreStore) storetest.Fixture {
			return storetest.CoreFixture()
		},
	})
}

func TestBunStoreIdentityContract(t *testing.T) {
	storetest.RunIdentityContract(t, storetest.IdentityBackend{
		Name: "postgres",
		Open: func(t *testing.T) (storetest.IdentityStore, storetest.IdentityFixture) {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunIdentityContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunIdentityContractSchema)
			})
			pg, err := Open(pgURL, bunIdentityContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunIdentityContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture, err := storetest.InsertBunIdentityFixture(
				t.Context(), store.bun, "bun-identity-archive-a", "bun-identity-salt-a",
			)
			require.NoError(t, err)
			return store, fixture
		},
	})
}

func TestBunStoreDataContract(t *testing.T) {
	storetest.RunDataContract(t, storetest.DataBackend{
		Name: "postgres",
		Open: func(t *testing.T) (storetest.DataStore, storetest.IdentityFixture) {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunDataContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunDataContractSchema)
			})
			pg, err := Open(pgURL, bunDataContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunDataContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture, err := storetest.InsertBunIdentityFixture(
				t.Context(), store.bun, "bun-identity-archive-a", "bun-identity-salt-a",
			)
			require.NoError(t, err)
			return store.BunStore, fixture
		},
	})
}

func TestBunStoreCurationContract(t *testing.T) {
	storetest.RunCurationContract(t, storetest.CurationBackend{
		Name: "postgres", Writable: true,
		Open: func(t *testing.T) (storetest.CurationStore, storetest.CurationFixture) {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunCurationContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunCurationContractSchema)
			})
			pg, err := Open(pgURL, bunCurationContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunCurationContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture, err := storetest.InsertBunCurationFixture(
				t.Context(), store.bun, "bun-curation-archive", "bun-curation-generation",
			)
			require.NoError(t, err)
			return store.BunStore, fixture
		},
	})
}

func TestBunStoreInsightContract(t *testing.T) {
	storetest.RunInsightContract(t, storetest.InsightBackend{
		Name: "postgres", Writable: true,
		Open: func(t *testing.T) (storetest.InsightStore, storetest.InsightFixture) {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunInsightContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunInsightContractSchema)
			})
			pg, err := Open(pgURL, bunInsightContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunInsightContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture, err := storetest.InsertBunInsightFixture(t.Context(), store.bun)
			require.NoError(t, err)
			require.NoError(t, store.DetectInsightGenerationAvailability(t.Context()))
			return store.BunStore, fixture
		},
	})
}

func TestBunStoreMutationContract(t *testing.T) {
	storetest.RunMutationContract(t, storetest.MutationBackend{
		Name: "postgres", Writable: true,
		Open: func(t *testing.T, extraTrashRows int) storetest.MutationHarness {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunMutationContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunMutationContractSchema)
			})
			pg, err := Open(pgURL, bunMutationContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunMutationContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture, err := storetest.InsertBunMutationFixture(
				t.Context(), store.bun, "bun-mutation-archive",
				"bun-mutation-generation", extraTrashRows,
			)
			require.NoError(t, err)
			futureRevision := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
			_, err = store.bun.NewUpdate().Table("sessions").
				Set("updated_at = ?", futureRevision).
				Where("1 = 1").
				Exec(t.Context())
			require.NoError(t, err)
			return storetest.MutationHarness{
				Store: store.BunStore,
				Rows:  fixture,
				IsExcluded: func(t *testing.T, id string) bool {
					t.Helper()
					count, countErr := store.bun.NewSelect().
						Table("excluded_sessions").Where("id = ?", id).Count(t.Context())
					require.NoError(t, countErr)
					return count > 0
				},
				OperationalTouchAfter: func(t *testing.T, id string) bool {
					t.Helper()
					var updatedAt time.Time
					require.NoError(t, store.bun.NewSelect().Table("sessions").
						Column("updated_at").Where("id = ?", id).Scan(t.Context(), &updatedAt))
					return updatedAt.After(futureRevision)
				},
			}
		},
	})
}

func TestBunStoreRecallContract(t *testing.T) {
	storetest.RunRecallContract(t, storetest.RecallBackend{
		Name: "postgres", Writable: false,
		Open: func(t *testing.T) storetest.RecallStore {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunRecallContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunRecallContractSchema)
			})
			pg, err := Open(pgURL, bunRecallContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunRecallContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore
		},
	})
}

func TestBunStoreUsageContract(t *testing.T) {
	storetest.RunUsageContract(t, storetest.UsageBackend{
		Name: "postgres",
		Open: func(t *testing.T) storetest.UsageStore {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunUsageContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunUsageContractSchema)
			})
			pg, err := Open(pgURL, bunUsageContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunUsageContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			require.NoError(t, storetest.InsertBunUsageFixture(
				t.Context(), store.bun, "bun-usage-archive", "bun-usage-generation",
			))
			return store.BunStore
		},
	})
}

func TestBunStoreUsageAllowsCompatibleMissingOptionalTables(t *testing.T) {
	pgURL := testPGURL(t)
	cleanupBunContractSchema(t, pgURL, bunOptionalUsageContractSchema)
	t.Cleanup(func() {
		cleanupBunContractSchema(t, pgURL, bunOptionalUsageContractSchema)
	})
	pg, err := Open(pgURL, bunOptionalUsageContractSchema, true)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(t.Context(), pg, bunOptionalUsageContractSchema))
	store := newStore(pg)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, storetest.InsertBunUsageFixture(
		t.Context(), store.bun, "bun-optional-usage-archive", "bun-optional-usage-generation",
	))
	_, err = pg.ExecContext(t.Context(), `
		DROP TABLE model_pricing_bands;
		DROP TABLE model_pricing;
		DROP TABLE cursor_usage_events`)
	require.NoError(t, err)
	require.NoError(t, CheckSchemaCompat(t.Context(), pg))

	store.SetCustomPricing(map[string]config.CustomModelRate{
		"contract-model": {
			InputMicrodollarsPerMTok:  2_000_000,
			OutputMicrodollarsPerMTok: 3_000_000,
		},
	})
	result, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2026-08-02", To: "2026-08-02", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(80), result.Totals.TotalCost.Microdollars)
	assert.Equal(t, []string{"contract-model"}, result.Daily[0].ModelsUsed)
}

func TestCanonicalPricingWriteContract(t *testing.T) {
	pgURL := testPGURL(t)
	cleanupBunContractSchema(t, pgURL, bunPricingWriteContractSchema)
	t.Cleanup(func() {
		cleanupBunContractSchema(t, pgURL, bunPricingWriteContractSchema)
	})
	pg, err := Open(pgURL, bunPricingWriteContractSchema, true)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(t.Context(), pg, bunPricingWriteContractSchema))
	store := newStore(pg)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	storetest.RunPricingWriteContract(t, "postgres", store.bun)
	storetest.RunCursorUsageWriteContract(t, "postgres", store.bun)
}

func TestBunStoreAnalyticsContract(t *testing.T) {
	storetest.RunAnalyticsContract(t, storetest.AnalyticsBackend{
		Name: "postgres",
		Open: func(t *testing.T) storetest.AnalyticsStore {
			pgURL := testPGURL(t)
			cleanupBunContractSchema(t, pgURL, bunAnalyticsContractSchema)
			t.Cleanup(func() {
				cleanupBunContractSchema(t, pgURL, bunAnalyticsContractSchema)
			})
			pg, err := Open(pgURL, bunAnalyticsContractSchema, true)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(t.Context(), pg, bunAnalyticsContractSchema))
			store := newStore(pg)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			require.NoError(t, storetest.InsertBunAnalyticsFixture(
				t.Context(), store.bun, "bun-analytics-archive", "bun-analytics-generation",
			))
			return store.BunStore
		},
	})
}

func cleanupBunCoreContractSchema(t *testing.T, pgURL string) {
	cleanupBunContractSchema(t, pgURL, bunCoreContractSchema)
}

func cleanupBunContractSchema(t *testing.T, pgURL string, schema string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	defer func() { require.NoError(t, pg.Close()) }()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
}
