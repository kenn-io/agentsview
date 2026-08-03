package db_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storetest"
)

func TestBunStoreCoreContract(t *testing.T) {
	storetest.RunCoreContract(t, storetest.Backend{
		Name: "sqlite",
		Open: func(t *testing.T) storetest.CoreStore {
			database, err := db.Open(filepath.Join(t.TempDir(), "core-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			archiveID, err := database.GetArchiveID(t.Context())
			require.NoError(t, err)
			generation, err := database.GetDatabaseID(t.Context())
			require.NoError(t, err)
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				return storetest.InsertSQLiteCoreFixture(
					t.Context(), tx, archiveID, generation,
				)
			}))
			return database.BunStore
		},
		Seed: func(*testing.T, storetest.CoreStore) storetest.Fixture {
			return storetest.CoreFixture()
		},
	})
}

func TestBunStoreIdentityContract(t *testing.T) {
	storetest.RunIdentityContract(t, storetest.IdentityBackend{
		Name: "sqlite",
		Open: func(t *testing.T) (storetest.IdentityStore, storetest.IdentityFixture) {
			database, err := db.Open(filepath.Join(t.TempDir(), "identity-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			archiveID, err := database.GetArchiveID(t.Context())
			require.NoError(t, err)
			archiveSalt, err := database.GetArchiveSalt(t.Context())
			require.NoError(t, err)
			var fixture storetest.IdentityFixture
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				var insertErr error
				fixture, insertErr = storetest.InsertSQLiteIdentityFixture(
					t.Context(), tx, archiveID, archiveSalt,
				)
				return insertErr
			}))
			return database, fixture
		},
	})
}
