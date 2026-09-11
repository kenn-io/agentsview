//go:build pgtest

package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedRuntimeIdleAndMaintenanceQueriesStayBounded(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "current", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("indexed")))
	jobs, err := NewHostedRawIngestStore(f.runtime, f.tenant, "parser-1")
	require.NoError(t, err)
	small := map[string]float64{}
	for _, size := range []int{100, 10000} {
		_, err = f.runtime.Exec(`INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256) SELECT tenant_id,device_id,provider,configured_root_id,'filler-'||n,repeat(md5('filler-'||n),2) FROM raw_manifests CROSS JOIN generate_series(1,$2::int)n WHERE manifest_id=$1 ON CONFLICT DO NOTHING`, m.ManifestID, size)
		require.NoError(t, err)

		_, err = f.runtime.Exec(`INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,canonical_json)
 SELECT tenant_id,repeat(md5('filler-'||n),2),device_id,provider,configured_root_id,'filler-'||n,repeat(md5('filler-'||n),2),'capture','',repeat(md5('filler-'||n),2),1,'snapshot',captured_at,canonical_json FROM raw_manifests CROSS JOIN generate_series(1,$2::int)n WHERE manifest_id=$1 ON CONFLICT DO NOTHING`, m.ManifestID, size)
		require.NoError(t, err)
		_, err = f.runtime.Exec(`INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,manifest_id,receipt,generation)
 SELECT tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,manifest_id,receipt,generation FROM raw_manifests ON CONFLICT(tenant_id,device_id,provider,configured_root_id,source_key_sha256) DO UPDATE SET manifest_id=EXCLUDED.manifest_id,receipt=EXCLUDED.receipt,generation=EXCLUDED.generation;
 INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version,state,projection_generation,available_at)
 SELECT tenant_id,manifest_id,'parse','parser-1',CASE WHEN source_key LIKE 'filler-%' THEN 'retrying' ELSE 'complete' END,1,now()+interval '1 day' FROM raw_manifests ON CONFLICT DO NOTHING;
 INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version,state,projection_generation)
 SELECT tenant_id,manifest_id,'parse','historical','complete',1 FROM raw_manifests ON CONFLICT DO NOTHING;`)
		require.NoError(t, err)
		_, err = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,ended_at) SELECT 'settled-'||n,'synthetic','fixture','codex',now()-interval '1 day' FROM generate_series(1,$1::int)n ON CONFLICT DO NOTHING`, size)
		require.NoError(t, err)
		_, err = f.admin.Exec(`ANALYZE sessions;ANALYZE raw_ingest_jobs;ANALYZE raw_manifests;ANALYZE raw_source_heads;ANALYZE raw_source_projections`)
		require.NoError(t, err)
		query, args := jobs.rawParseClaimStatement("idle", 1, time.Minute)
		for _, tc := range []struct {
			name, query string
			args        []any
		}{
			{"claim", query, args},
			{"pending", pendingRawSignalsSQL, []any{f.tenant, time.Now(), 1}},
			{"rollout", rawRolloutHeadsSQL, []any{f.tenant, "", "", "", "", 1}},
		} {
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				tx, err := f.runtime.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				_, err = tx.Exec(`SET LOCAL plan_cache_mode=` + mode)
				require.NoError(t, err)
				// A tiny table may reasonably use a sequential scan. Show the indexed
				// path there; the large fixture must choose its normal bounded plan.
				if size == 100 {
					_, err = tx.Exec(`SET LOCAL enable_seqscan=off`)
					require.NoError(t, err)
				}
				var encoded []byte
				err = tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+tc.query, tc.args...).Scan(&encoded)
				require.NoError(t, err)
				require.NoError(t, tx.Rollback())
				var plans []struct{ Plan map[string]any }
				require.NoError(t, json.Unmarshal(encoded, &plans))
				require.Len(t, plans, 1)
				plan := plans[0].Plan
				buffers := plan["Shared Hit Blocks"].(float64) + plan["Shared Read Blocks"].(float64)
				name := tc.name + "/" + mode
				if size == 100 {
					small[name] = buffers
				} else {
					assert.LessOrEqual(t, buffers, small[name]+30, name)
					assert.Less(t, buffers, float64(150), name)
				}
				var visit func(map[string]any)
				visit = func(node map[string]any) {
					if removed, ok := node["Rows Removed by Filter"].(float64); ok {
						assert.LessOrEqual(t, removed, float64(3), name)
					}
					if children, ok := node["Plans"].([]any); ok {
						for _, child := range children {
							visit(child.(map[string]any))
						}
					}
				}
				visit(plan)
				t.Logf("size=%d %s shared buffers=%.0f", size, name, buffers)
			}
		}
		leases, err := jobs.ClaimRawParseJobs(t.Context(), "idle", 1, time.Minute)
		require.NoError(t, err)
		assert.Empty(t, leases)
		var generation int
		require.NoError(t, f.runtime.QueryRow(`SELECT projection_generation FROM raw_source_projections WHERE source_id=$1`, rawSourceID(m)).Scan(&generation))
		assert.Equal(t, 1, generation)
	}
}
