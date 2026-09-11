//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawProjectionProofLookupsStayBounded(t *testing.T) {
	f := newProjectionFixture(t)
	m, receipt := f.accept(t, "device-a", "indexed-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("indexed")))
	identity, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	source := rawSourceID(m)
	for _, size := range []int{32, 8192} {
		seedRawUnrelatedProof(t, f, source, identity.SessionID, size)
		_, err = f.admin.ExecContext(t.Context(), `ANALYZE session_sources; ANALYZE sessions; ANALYZE raw_session_branches`)
		require.NoError(t, err)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			cases := []struct{ name, query, arg, column string }{
				{"source", rawSourceProofDeleteSQL, source, "source_id"},
				{"content", rawSourceProofAttachSQL, identity.SessionID, "session_id"},
				{"physical", rawSourceProofDetachSQL, identity.SessionID, "physical_session_id"},
				{"group", rawGroupContentsSQL, identity.GroupID, "raw_group_id"},
			}
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%d/%s/%s", size, mode, tc.name), func(t *testing.T) {
					plan := rawLookupPlan(t, f.runtime, mode, tc.query, tc.arg, size == 32)
					var indexed bool
					var visit func(map[string]any)
					visit = func(node map[string]any) {
						if cond, ok := node["Index Cond"].(string); ok && strings.Contains(cond, tc.column+" =") {
							indexed = true
						}
						if removed, ok := node["Rows Removed by Filter"].(float64); ok {
							assert.LessOrEqual(t, removed, float64(2), "unrelated proof scanned: %v", node)
						}
						if scans, ok := node["Plans"].([]any); ok {
							for _, v := range scans {
								visit(v.(map[string]any))
							}
						}
					}
					visit(plan)
					assert.True(t, indexed, "missing selective index predicate: %v", plan)
					// Heap/index work stays bounded even with 256x more unrelated proof.
					blocks, _ := plan["Shared Hit Blocks"].(float64)
					reads, _ := plan["Shared Read Blocks"].(float64)
					assert.Less(t, blocks+reads, float64(100), "changed-source lookup touched unrelated pages: %v", plan)
				})
			}
		}
		// Exercise the same predicates through equal receipts and actual removal /
		// reactivation while the unrelated population remains present.
		m, receipt = f.accept(t, "device-a", fmt.Sprintf("equal-%d", size), receipt.Receipt)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("indexed")))
		empty := projectionOutcome("unused")
		empty.Outcome.Results = nil
		m, receipt = f.accept(t, "device-a", fmt.Sprintf("remove-%d", size), receipt.Receipt)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, empty))
		m, receipt = f.accept(t, "device-a", fmt.Sprintf("return-%d", size), receipt.Receipt)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("indexed")))
		var count int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM session_sources`).Scan(&count))
		assert.Equal(t, size+1, count)
	}
}

func rawLookupPlan(t *testing.T, pg *sql.DB, mode, query, arg string, small bool) map[string]any {
	t.Helper()
	conn, err := pg.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), `SET LOCAL plan_cache_mode=`+mode)
	require.NoError(t, err)
	if small {
		// A sequential scan is reasonable on the tiny fixture. Prove an index
		// path exists there, but let the large fixture choose its normal plan.
		_, err = tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan=off`)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(t.Context(), `PREPARE raw_lookup(text) AS `+query)
	require.NoError(t, err)
	var data []byte
	require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE raw_lookup('`+strings.ReplaceAll(arg, "'", "''")+`')`).Scan(&data))
	_, err = tx.ExecContext(t.Context(), `DEALLOCATE raw_lookup`)
	require.NoError(t, err)
	var result []struct{ Plan map[string]any }
	require.NoError(t, json.Unmarshal(data, &result))
	require.Len(t, result, 1)
	return result[0].Plan
}

func seedRawUnrelatedProof(t *testing.T, f projectionFixture, source, id string, size int) {
	t.Helper()
	statements := []string{
		`INSERT INTO raw_source_projections(source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation,selected_job_id) SELECT 'filler-source-'||n,p.device_id,p.provider,p.configured_root_id,p.source_key_sha256,p.selected_manifest_id,p.processing_version,p.projection_generation,p.selected_job_id FROM raw_source_projections p CROSS JOIN generate_series(1,$3::int)n WHERE p.source_id=$1 ON CONFLICT DO NOTHING`,
		`INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias) SELECT 'filler-group-'||n,'codex','filler-key-'||n,'filler-alias-'||n FROM generate_series(1,$3::int)n ON CONFLICT DO NOTHING`,
		`INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload) SELECT 'filler-row-'||n,'filler-group-'||n,'filler-revision-'||n,p.payload FROM raw_content_revisions p CROSS JOIN generate_series(1,$3::int)n WHERE p.session_id=$2 ON CONFLICT DO NOTHING`,
		`INSERT INTO sessions(id,project,machine,agent,raw_group_id) SELECT 'filler-row-'||n,'synthetic','fixture','codex','filler-group-'||n FROM generate_series(1,$3::int)n ON CONFLICT DO NOTHING`,
		`INSERT INTO raw_session_branches(branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload) SELECT 'filler-branch-'||n,'filler-source-'||n,'filler-group-'||n,'filler-member-'||n,'filler-row-'||n,'filler-revision-'||n,p.selected_manifest_id,p.processing_version,p.projection_generation,true,''::bytea FROM raw_source_projections p CROSS JOIN generate_series(1,$3::int)n WHERE p.source_id=$1 ON CONFLICT DO NOTHING`,
		`INSERT INTO session_sources(branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation) SELECT branch_id,group_id,source_id,session_id,session_id,manifest_id,content_revision,processing_version,projection_generation FROM raw_session_branches WHERE branch_id LIKE 'filler-branch-%' ON CONFLICT DO NOTHING`,
	}
	for _, statement := range statements {
		// Explicit parameter casts keep unused positions well-typed in statements
		// which need only one of the fixture keys.
		statement = `WITH fixture_parameters AS (SELECT $1::text,$2::text,$3::int) ` + statement
		_, err := f.runtime.ExecContext(t.Context(), statement, source, id, size)
		require.NoError(t, err)
	}
}
