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
	_, store := prepareUsageSchema(t, schema)
	pg := store.DB()
	indexes := []string{"idx_sessions_data_version", "idx_sessions_parentless_workers", "idx_sessions_source_missing"}
	queries := []struct{ name, sql string }{
		{"version", maxDataVersionSQL},
		{"workers", repairParentlessWorkersSQL},
		{"source missing", repairLegacySourceMissingDeletionSQL},
	}
	for _, indexed := range []bool{false, true} {
		if !indexed {
			for _, index := range indexes {
				_, err := pg.ExecContext(t.Context(), `DROP INDEX `+index)
				require.NoError(t, err)
			}
		} else {
			require.NoError(t, (&Sync{pg: pg, schema: schema}).EnsureSchema(t.Context()))
		}
		for _, size := range []int{1000, 10000} {
			_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id, machine, project, agent, data_version)
				SELECT 'settled-'||n, 'machine', 'project', 'codex', $2 FROM generate_series(1,$1::int)n ON CONFLICT DO NOTHING`, size, db.CurrentDataVersion())
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), `VACUUM ANALYZE sessions`)
			require.NoError(t, err)
			for _, query := range queries {
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
				t.Logf("indexed=%t sessions=%d %s buffers=%d execution_ms=%.3f", indexed, size, query.name, buffers, plans[0].Time)
				if indexed {
					assert.LessOrEqual(t, buffers, 8, query.name)
				}
			}
			// Start each measurement from the requested cardinality.
			_, err = pg.ExecContext(t.Context(), `DELETE FROM sessions`)
			require.NoError(t, err)
		}
	}

	// Each missing index must independently trigger the ordinary push upgrade.
	for _, index := range indexes {
		t.Run(index, func(t *testing.T) {
			_, err := pg.ExecContext(t.Context(), `INSERT INTO sessions(id, machine, project, agent, session_kind, deleted_at, deletion_cause)
				VALUES ('late-worker', 'machine', 'project', 'codex', 'non-interactive', NULL, NULL),
				('late-source', 'machine', 'project', 'codex', '', NOW(), 'source_missing'),
				('user-trash', 'machine', 'project', 'codex', '', NOW(), 'user')`)
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), `DROP INDEX `+index)
			require.NoError(t, err)
			require.NoError(t, CheckSchemaCompat(t.Context(), pg), "read compatibility permits a missing preparation index")
			require.False(t, pushSchemaCurrent(t.Context(), pg))
			require.NoError(t, (&Sync{pg: pg, schema: schema}).EnsureSchema(t.Context()))
			require.True(t, pushSchemaCurrent(t.Context(), pg))
			var relationship string
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT relationship_type FROM sessions WHERE id='late-worker'`).Scan(&relationship))
			assert.Equal(t, "subagent", relationship)
			var restored, trashed bool
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT deleted_at IS NULL AND deletion_cause IS NULL FROM sessions WHERE id='late-source'`).Scan(&restored))
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT deleted_at IS NOT NULL AND deletion_cause='user' FROM sessions WHERE id='user-trash'`).Scan(&trashed))
			assert.True(t, restored)
			assert.True(t, trashed)
			_, err = pg.ExecContext(t.Context(), `DELETE FROM sessions`)
			require.NoError(t, err)
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
}
