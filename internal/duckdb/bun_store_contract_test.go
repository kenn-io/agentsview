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
