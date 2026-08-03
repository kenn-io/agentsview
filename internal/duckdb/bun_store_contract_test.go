//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
	"go.kenn.io/agentsview/internal/storetest"
)

func TestBunStoreCoreContract(t *testing.T) {
	storetest.RunCoreContract(t, storetest.Backend{
		Name: "duckdb",
		Open: func(t *testing.T) storetest.CoreStore {
			conn, err := Open(filepath.Join(t.TempDir(), "core-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			require.NoError(t, storetest.InsertBunCoreFixture(
				t.Context(), common, "duck-contract-archive", "duck-contract-generation",
			))
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore
		},
		Seed: func(*testing.T, storetest.CoreStore) storetest.Fixture {
			return storetest.CoreFixture()
		},
	})
}

func TestBunStoreIdentityContract(t *testing.T) {
	storetest.RunIdentityContract(t, storetest.IdentityBackend{
		Name: "duckdb",
		Open: func(t *testing.T) (storetest.IdentityStore, storetest.IdentityFixture) {
			conn, err := Open(filepath.Join(t.TempDir(), "identity-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			fixture, err := storetest.InsertBunIdentityFixture(
				t.Context(), common, "bun-identity-archive-a", "bun-identity-salt-a",
			)
			require.NoError(t, err)
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store, fixture
		},
	})
}

func TestBunStoreDataContract(t *testing.T) {
	storetest.RunDataContract(t, storetest.DataBackend{
		Name: "duckdb",
		Open: func(t *testing.T) (storetest.DataStore, storetest.IdentityFixture) {
			conn, err := Open(filepath.Join(t.TempDir(), "data-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			fixture, err := storetest.InsertBunIdentityFixture(
				t.Context(), common, "bun-identity-archive-a", "bun-identity-salt-a",
			)
			require.NoError(t, err)
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore, fixture
		},
	})
}
