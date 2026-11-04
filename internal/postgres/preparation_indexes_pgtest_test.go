//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGPreparationIndexesUpgradeAndBoundEmptyChecks(t *testing.T) {
	const schema = "agentsview_preparation_index_test"
	t.Run("bounded empty checks", func(t *testing.T) {
		_, store := prepareUsageSchema(t, schema)
		pg := store.DB()
		_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id, machine, project, agent, data_version)
			SELECT 'settled-'||n, 'machine', 'project', 'codex', $1 FROM generate_series(1,10000)n`, db.CurrentDataVersion())
		require.NoError(t, err)
		_, err = pg.ExecContext(t.Context(), `VACUUM ANALYZE sessions`)
		require.NoError(t, err)
		for _, query := range []struct{ name, sql string }{
			{"version", maxDataVersionSQL},
			{"workers", repairParentlessWorkersSQL},
			{"source missing", repairLegacySourceMissingDeletionSQL},
		} {
			var encoded []byte
			require.NoError(t, pg.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+query.sql).Scan(&encoded))
			var plans []struct {
				Plan struct {
					Hit  int `json:"Shared Hit Blocks"`
					Read int `json:"Shared Read Blocks"`
				}
				Time float64 `json:"Execution Time"`
			}
			require.NoError(t, json.Unmarshal(encoded, &plans))
			require.Len(t, plans, 1)
			buffers := plans[0].Plan.Hit + plans[0].Plan.Read
			t.Logf("sessions=10000 %s buffers=%d execution_ms=%.3f", query.name, buffers, plans[0].Time)
			assert.LessOrEqual(t, buffers, 8, query.name)
		}
	})

	t.Run("missing index upgrade", func(t *testing.T) {
		_, store := prepareUsageSchema(t, schema)
		pg := store.DB()

		// Each missing index must independently trigger the ordinary push upgrade.
		for _, index := range []string{"idx_sessions_data_version", "idx_sessions_parentless_workers", "idx_sessions_source_missing"} {
			t.Run(index, func(t *testing.T) {
				_, err := pg.ExecContext(t.Context(), `DROP INDEX `+index)
				require.NoError(t, err)
				require.True(t, pushSchemaCurrent(t.Context(), pg), "missing performance indexes preserve schema compatibility")
				require.NoError(t, (&Sync{pg: pg, schema: schema}).EnsureSchema(t.Context()))
				require.True(t, pgHasIndex(t.Context(), pg, index), "owner preparation installs the missing index")
			})
		}

		_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id, machine, project, agent, data_version)
		VALUES ('newer-parser', 'machine', 'project', 'codex', $1)`, db.CurrentDataVersion()+1)
		require.NoError(t, err)
		_, err = pg.ExecContext(t.Context(), `DROP INDEX idx_sessions_data_version`)
		require.NoError(t, err)
		err = (&Sync{pg: pg, schema: schema}).EnsureSchema(t.Context())
		require.True(t, db.IsDataVersionTooNew(err))
		assert.False(t, pgHasIndex(t.Context(), pg, "idx_sessions_data_version"), "newer data must be refused before schema writes")
	})
}

func TestPGPreparationIndexesConcurrentInstallersBothSucceed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strict bool
	}{
		{"both compatible", false},
		{"strict plus compatible", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const schema = "agentsview_preparation_concurrent_test"
			pgURL, store := prepareUsageSchema(t, schema)
			pg := store.DB()
			_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id,machine,project,agent) VALUES('existing','machine','project','codex')`)
			require.NoError(t, err)
			for _, index := range []string{"idx_sessions_data_version", "idx_sessions_parentless_workers", "idx_sessions_source_missing"} {
				_, err := pg.ExecContext(t.Context(), `DROP INDEX `+index)
				require.NoError(t, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			blocker, err := pg.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback() }()
			// Block the shared installer gate, so strict setup can finish its earlier DDL.
			require.NoError(t, lockSyncMetadataRow(ctx, blocker, "preparation_indexes_lock"))
			results := make(chan error, 2)
			for _, name := range []string{"preparation-index-a", "preparation-index-b"} {
				dsn, err := appendConnParams(pgURL, map[string]string{"application_name": name})
				require.NoError(t, err)
				conn, err := Open(ctx, dsn, schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
				go func() {
					if tc.strict && name == "preparation-index-b" {
						results <- EnsureSchema(ctx, conn, schema)
					} else {
						results <- (&Sync{pg: conn, schema: schema}).EnsureSchema(ctx)
					}
				}()
			}
			require.Eventually(t, func() bool {
				var waiting int
				err := pg.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_stat_activity
					WHERE datname=current_database() AND application_name IN ('preparation-index-a','preparation-index-b')
					AND wait_event_type='Lock' AND query LIKE '%sync_metadata%FOR UPDATE'`).Scan(&waiting)
				return err == nil && waiting == 2
			}, 20*time.Second, 10*time.Millisecond, "both public entry points must reach the shared installer lock")
			require.NoError(t, blocker.Commit())
			require.NoError(t, <-results, "first publisher upgrades the shared archive")
			require.NoError(t, <-results, "second publisher upgrades the shared archive")
			for _, index := range []string{"idx_sessions_data_version", "idx_sessions_parentless_workers", "idx_sessions_source_missing"} {
				assert.True(t, pgHasIndex(t.Context(), pg, index), index)
			}
		})
	}
}
