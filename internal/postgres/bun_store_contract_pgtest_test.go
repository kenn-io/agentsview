//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/storetest"
)

const bunCoreContractSchema = "agentsview_bun_core_contract"

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

func cleanupBunCoreContractSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	defer func() { require.NoError(t, pg.Close()) }()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + bunCoreContractSchema + ` CASCADE`)
	require.NoError(t, err)
}
