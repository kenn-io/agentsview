//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
	"go.kenn.io/agentsview/internal/duckdb/bundialect"
	"go.kenn.io/agentsview/internal/storetest"
)

func TestStableDuckDBViewRetriesChangedRemoteGeneration(t *testing.T) {
	tokens := []string{"generation-a", "generation-b", "generation-b", "generation-b"}
	tokenIndex := 0
	callbackCalls := 0
	err := stableDuckDBView(
		t.Context(),
		func(context.Context) (string, error) {
			token := tokens[tokenIndex]
			tokenIndex++
			return token, nil
		},
		func() error {
			callbackCalls++
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, callbackCalls)
	assert.Equal(t, len(tokens), tokenIndex)
}

func TestDuckDBMirrorReadTokenUsesOpaqueGeneration(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "mirror-token.duckdb"))
	require.NoError(t, err)
	_, err = conn.Exec(`
		CREATE TABLE sync_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO sync_metadata (key, value) VALUES
			('agentsview_mirror_generation', 'opaque-generation'),
			('z-key', 'second'),
			('a-key', 'first')`)
	require.NoError(t, err)
	store := NewStoreFromDB(conn)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	token, err := store.mirrorReadToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "opaque-generation", token)
}

func TestBunStoreConsistentViewSnapshotsMutableDirectConnection(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "mutable-snapshot.duckdb"))
	require.NoError(t, err)
	conn.SetMaxOpenConns(4)
	common := bun.NewDB(conn, bundialect.New())
	require.NoError(t, db.CreateCommonSchema(t.Context(), common))
	_, err = common.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "snapshot-archive", SourceArchiveSalt: "snapshot-salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC))
	_, err = common.NewInsert().Model(&bunmodel.Session{
		ID: "before", Project: "snapshot", Machine: "host", Agent: "codex",
		CreatedAt: created, SourceArchiveID: "snapshot-archive",
		SourceDatabaseGeneration: "snapshot-generation",
	}).Exec(t.Context())
	require.NoError(t, err)

	store := NewStoreFromDB(conn)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	committed := make(chan error, 1)
	err = (&duckBunBackend{store: store}).ConsistentView(
		t.Context(), func(snapshot bun.IDB) error {
			var before int
			if err := snapshot.NewSelect().Table("sessions").ColumnExpr("COUNT(*)").
				Scan(t.Context(), &before); err != nil {
				return err
			}
			go func() {
				_, insertErr := conn.ExecContext(t.Context(), `
					INSERT INTO sessions (
						id, project, machine, agent, created_at,
						source_archive_id, source_database_generation
					) VALUES (?, 'snapshot', 'host', 'codex', ?, ?, ?)`,
					"after", created, "snapshot-archive", "snapshot-generation",
				)
				committed <- insertErr
			}()
			if insertErr := <-committed; insertErr != nil {
				return fmt.Errorf("committing concurrent DuckDB insert: %w", insertErr)
			}
			var after int
			if err := snapshot.NewSelect().Table("sessions").ColumnExpr("COUNT(*)").
				Scan(t.Context(), &after); err != nil {
				return err
			}
			assert.Equal(t, before, after)
			return nil
		},
	)
	require.NoError(t, err)
}

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

func TestCanonicalIdentityWriteContract(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "identity-write-contract.duckdb"))
	require.NoError(t, err)
	common := bun.NewDB(conn, bundialect.New())
	t.Cleanup(func() { require.NoError(t, common.Close()) })
	require.NoError(t, db.CreateCommonSchema(t.Context(), common))
	storetest.RunCanonicalIdentityWriteContract(t, "duckdb", common)
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

func TestBunStoreCurationContract(t *testing.T) {
	storetest.RunCurationContract(t, storetest.CurationBackend{
		Name: "duckdb", Writable: false,
		Open: func(t *testing.T) (storetest.CurationStore, storetest.CurationFixture) {
			conn, err := Open(filepath.Join(t.TempDir(), "curation-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			fixture, err := storetest.InsertBunCurationFixture(
				t.Context(), common, "bun-curation-archive", "bun-curation-generation",
			)
			require.NoError(t, err)
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore, fixture
		},
	})
}

func TestBunStoreInsightContract(t *testing.T) {
	storetest.RunInsightContract(t, storetest.InsightBackend{
		Name: "duckdb", Writable: false,
		Open: func(t *testing.T) (storetest.InsightStore, storetest.InsightFixture) {
			conn, err := Open(filepath.Join(t.TempDir(), "insight-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			fixture, err := storetest.InsertBunInsightFixture(t.Context(), common)
			require.NoError(t, err)
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore, fixture
		},
	})
}

func TestBunStoreMutationContract(t *testing.T) {
	storetest.RunMutationContract(t, storetest.MutationBackend{
		Name: "duckdb", Writable: false,
		Open: func(t *testing.T, extraTrashRows int) storetest.MutationHarness {
			conn, err := Open(filepath.Join(t.TempDir(), "mutation-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			fixture, err := storetest.InsertBunMutationFixture(
				t.Context(), common, "bun-mutation-archive",
				"bun-mutation-generation", extraTrashRows,
			)
			require.NoError(t, err)
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return storetest.MutationHarness{
				Store: store.BunStore,
				Rows:  fixture,
				IsExcluded: func(t *testing.T, id string) bool {
					t.Helper()
					count, countErr := common.NewSelect().
						Table("excluded_sessions").Where("id = ?", id).Count(t.Context())
					require.NoError(t, countErr)
					return count > 0
				},
			}
		},
	})
}

func TestBunStoreRecallContract(t *testing.T) {
	storetest.RunRecallContract(t, storetest.RecallBackend{
		Name: "duckdb", Writable: false,
		Open: func(t *testing.T) storetest.RecallStore {
			conn, err := Open(filepath.Join(t.TempDir(), "recall-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore
		},
	})
}

func TestBunStoreUsageContract(t *testing.T) {
	storetest.RunUsageContract(t, storetest.UsageBackend{
		Name: "duckdb",
		Open: func(t *testing.T) storetest.UsageStore {
			conn, err := Open(filepath.Join(t.TempDir(), "usage-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			require.NoError(t, storetest.InsertBunUsageFixture(
				t.Context(), common, "bun-usage-archive", "bun-usage-generation",
			))
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore
		},
	})
}

func TestCanonicalPricingWriteContract(t *testing.T) {
	conn, err := Open(filepath.Join(t.TempDir(), "pricing-write-contract.duckdb"))
	require.NoError(t, err)
	common := bun.NewDB(conn, bundialect.New())
	require.NoError(t, db.CreateCommonSchema(t.Context(), common))
	t.Cleanup(func() { require.NoError(t, common.Close()) })
	storetest.RunPricingWriteContract(t, "duckdb", common)
	storetest.RunCursorUsageWriteContract(t, "duckdb", common)
}

func TestBunStoreAnalyticsContract(t *testing.T) {
	storetest.RunAnalyticsContract(t, storetest.AnalyticsBackend{
		Name: "duckdb",
		Open: func(t *testing.T) storetest.AnalyticsStore {
			conn, err := Open(filepath.Join(t.TempDir(), "analytics-contract.duckdb"))
			require.NoError(t, err)
			common := bun.NewDB(conn, bundialect.New())
			require.NoError(t, db.CreateCommonSchema(t.Context(), common))
			require.NoError(t, storetest.InsertBunAnalyticsFixture(
				t.Context(), common, "bun-analytics-archive", "bun-analytics-generation",
			))
			store := NewStoreFromDB(conn)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store.BunStore
		},
	})
}
