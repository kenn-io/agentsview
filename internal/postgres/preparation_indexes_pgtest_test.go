//go:build pgtest

package postgres

import (
	"encoding/json/v2"
	"testing"

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
				require.NoError(t, CheckSchemaCompat(t.Context(), pg), "read compatibility permits a missing preparation index")
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
