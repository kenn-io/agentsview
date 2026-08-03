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

func TestBunStoreDataContract(t *testing.T) {
	storetest.RunDataContract(t, storetest.DataBackend{
		Name: "sqlite",
		Open: func(t *testing.T) (storetest.DataStore, storetest.IdentityFixture) {
			database, err := db.Open(filepath.Join(t.TempDir(), "data-contract.db"))
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
			return database.BunStore, fixture
		},
	})
}

func TestBunStoreCurationContract(t *testing.T) {
	storetest.RunCurationContract(t, storetest.CurationBackend{
		Name: "sqlite", Writable: true,
		Open: func(t *testing.T) (storetest.CurationStore, storetest.CurationFixture) {
			database, err := db.Open(filepath.Join(t.TempDir(), "curation-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			archiveID, err := database.GetArchiveID(t.Context())
			require.NoError(t, err)
			generation, err := database.GetDatabaseID(t.Context())
			require.NoError(t, err)
			var fixture storetest.CurationFixture
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				var insertErr error
				fixture, insertErr = storetest.InsertSQLiteCurationFixture(
					t.Context(), tx, archiveID, generation,
				)
				return insertErr
			}))
			return database.BunStore, fixture
		},
	})
}

func TestBunStoreInsightContract(t *testing.T) {
	storetest.RunInsightContract(t, storetest.InsightBackend{
		Name: "sqlite", Writable: true,
		Open: func(t *testing.T) (storetest.InsightStore, storetest.InsightFixture) {
			database, err := db.Open(filepath.Join(t.TempDir(), "insight-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			var fixture storetest.InsightFixture
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				var insertErr error
				fixture, insertErr = storetest.InsertSQLiteInsightFixture(t.Context(), tx)
				return insertErr
			}))
			return database.BunStore, fixture
		},
	})
}

func TestBunStoreMutationContract(t *testing.T) {
	storetest.RunMutationContract(t, storetest.MutationBackend{
		Name: "sqlite", Writable: true,
		Open: func(t *testing.T, extraTrashRows int) storetest.MutationHarness {
			database, err := db.Open(filepath.Join(t.TempDir(), "mutation-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			archiveID, err := database.GetArchiveID(t.Context())
			require.NoError(t, err)
			generation, err := database.GetDatabaseID(t.Context())
			require.NoError(t, err)
			var fixture storetest.MutationFixture
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				var insertErr error
				fixture, insertErr = storetest.InsertSQLiteMutationFixture(
					t.Context(), tx, archiveID, generation, extraTrashRows,
				)
				return insertErr
			}))
			return storetest.MutationHarness{
				Store: database.BunStore,
				Rows:  fixture,
				IsExcluded: func(t *testing.T, id string) bool {
					t.Helper()
					return database.IsSessionExcluded(id)
				},
				RestoreBaselinePresent: func(t *testing.T, id string) bool {
					t.Helper()
					var count int
					require.NoError(t, database.Reader().QueryRowContext(t.Context(), `
						SELECT count(*) FROM local_session_source_baselines
						WHERE session_id = ?`, id).Scan(&count))
					return count > 0
				},
			}
		},
	})
}

func TestBunStoreRecallContract(t *testing.T) {
	storetest.RunRecallContract(t, storetest.RecallBackend{
		Name: "sqlite", Readable: true, Writable: true,
		Open: func(t *testing.T) storetest.RecallStore {
			database, err := db.Open(filepath.Join(t.TempDir(), "recall-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "bun-recall-source", Project: "recall-contract",
				Machine: "contract-machine", Agent: "codex",
			}))
			return database.BunStore
		},
	})
}

func TestBunStoreReadOnlyRecallContract(t *testing.T) {
	storetest.RunRecallContract(t, storetest.RecallBackend{
		Name: "sqlite-read-only", Readable: true,
		Open: func(t *testing.T) storetest.RecallStore {
			path := filepath.Join(t.TempDir(), "read-only-recall-contract.db")
			database, err := db.Open(path)
			require.NoError(t, err)
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "bun-recall-source", Project: "recall-contract",
				Machine: "contract-machine", Agent: "codex",
			}))
			_, err = database.InsertRecallEntry(db.RecallEntry{
				ID: "bun-recall-entry", Type: "fact", Scope: "project",
				Title:   "Canonical Recall entry",
				Body:    "Recall reads remain available from a read-only archive.",
				Project: "recall-contract", SourceSessionID: "bun-recall-source",
				Transferable: true, ProvenanceOK: true,
			})
			require.NoError(t, err)
			require.NoError(t, database.Close())

			readOnly, err := db.OpenReadOnly(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, readOnly.Close()) })
			return readOnly.BunStore
		},
	})
}

func TestBunStoreUsageContract(t *testing.T) {
	storetest.RunUsageContract(t, storetest.UsageBackend{
		Name: "sqlite",
		Open: func(t *testing.T) storetest.UsageStore {
			database, err := db.Open(filepath.Join(t.TempDir(), "usage-contract.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			archiveID, err := database.GetArchiveID(t.Context())
			require.NoError(t, err)
			generation, err := database.GetDatabaseID(t.Context())
			require.NoError(t, err)
			require.NoError(t, database.Update(func(tx *sql.Tx) error {
				return storetest.InsertSQLiteUsageFixture(
					t.Context(), tx, archiveID, generation,
				)
			}))
			return database.BunStore
		},
	})
}
