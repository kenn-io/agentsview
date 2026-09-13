//go:build pgtest

package postgres

import (
	"database/sql"
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are preservation inventories, not fabricated parser success. One source
// and physical member use normal publication. Remaining retained heads have no
// projection, and additional raw members lack physical proof and stay ambiguous.
// Wide keys make retaining all caller pages distinguishable from one live page.
func seedParityCensusAcceptance(t *testing.T, scenario parityCaptureScenario, size int) {
	t.Helper()
	for _, database := range []*sql.DB{scenario.runtime.runtime, scenario.baseline.runtime} {
		_, err := database.ExecContext(t.Context(), `INSERT INTO raw_source_heads(tenant_id,device_id,provider,configured_root_id,source_key,source_key_sha256,generation)
 SELECT current_setting('agentsview.tenant_id'),'device-a','codex','root-a',k,encode(sha256(convert_to(k,'UTF8')),'hex'),0
 FROM (SELECT 'census-'||lpad(n::text,6,'0')||repeat('x',8192) k FROM generate_series(1,$1)n) keys`, size-1)
		require.NoError(t, err)
		_, err = database.ExecContext(t.Context(), `INSERT INTO raw_manifests(tenant_id,manifest_id,device_id,provider,configured_root_id,source_key,source_key_sha256,capture_id,parent_receipt,receipt,generation,kind,captured_at,canonical_json)
 SELECT current_setting('agentsview.tenant_id'),repeat(md5('manifest-'||source_key_sha256),2),device_id,provider,configured_root_id,source_key,source_key_sha256,'capture-'||source_key_sha256,'',repeat(md5('receipt-'||source_key_sha256),2),1,'snapshot',clock_timestamp(),convert_to('{}','UTF8')
 FROM raw_source_heads WHERE source_key LIKE 'census-%'`)
		require.NoError(t, err)
		_, err = database.ExecContext(t.Context(), `UPDATE raw_source_heads h SET manifest_id=m.manifest_id,receipt=m.receipt,generation=1 FROM raw_manifests m WHERE h.source_key_sha256=m.source_key_sha256 AND h.source_key LIKE 'census-%'`)
		require.NoError(t, err)
	}
	_, err := scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_groups(group_id,provider,logical_key,base_alias)
 SELECT 'census-group-'||lpad(n::text,6,'0'),'codex','logical-'||n||repeat('y',8192),'census-alias-'||n FROM generate_series(1,$1)n;
 `, size-1)
	require.NoError(t, err)
	_, err = scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_content_revisions(session_id,group_id,content_revision,payload)
 SELECT 'census-session-'||n,'census-group-'||lpad(n::text,6,'0'),'revision',decode('','hex') FROM generate_series(1,$1)n`, size-1)
	require.NoError(t, err)
	_, err = scenario.baseline.runtime.ExecContext(t.Context(), `INSERT INTO raw_session_branches(branch_id,source_id,group_id,member_id,session_id,content_revision,manifest_id,processing_version,projection_generation,active,prior_payload)
 SELECT 'census-branch-'||n,p.source_id,'census-group-'||lpad(n::text,6,'0'),'member-'||n,'census-session-'||n,'revision',p.selected_manifest_id,p.processing_version,p.projection_generation,TRUE,decode('','hex') FROM raw_source_projections p CROSS JOIN generate_series(1,$1)n WHERE p.source_id=$2`, size-1, scenario.sourceID)
	require.NoError(t, err)
}

type parityCensusMeasurement struct {
	pages, rows, maxRows int
	peakLive             uint64
}

func TestMigrationParityAcceptanceFullCensusRetainsOnlyBoundedPages(t *testing.T) {
	// Each phase's large corpus can retain at most a few pages plus driver state.
	// A single full 10,000-key census is >78MiB, far beyond this allowance.
	const allowance = 8 << 20
	smallPeaks := make(map[string]uint64)
	for _, size := range []int{10, 10000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			scenario := newParityCaptureScenario(t, "00000000-0000-4000-8000-000000000154")
			seedParityCensusAcceptance(t, scenario, size)
			measurements := make(map[string]*parityCensusMeasurement)
			for _, phase := range []string{"capture_runtime_sources", "capture_baseline_sources", "capture_members", "validation_runtime_sources", "validation_baseline_sources", "validation_members"} {
				measurements[phase] = &parityCensusMeasurement{}
			}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			scenario.store.parityCensusPageHook = func(phase string, rows int) {
				metric, ok := measurements[phase]
				require.True(t, ok, "unrecognized census phase")
				metric.pages++
				metric.rows += rows
				metric.maxRows = max(metric.maxRows, rows)
				// Measure live retained heap, not cumulative allocations: collection must
				// reclaim previous pages while the caller's current page is still in use.
				runtime.GC()
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				live := uint64(0)
				if current.HeapAlloc > before.HeapAlloc {
					live = current.HeapAlloc - before.HeapAlloc
				}
				metric.peakLive = max(metric.peakLive, live)
				require.LessOrEqual(t, rows, 128)
				if size == 10000 {
					require.LessOrEqual(t, live, smallPeaks[phase]+allowance, "%s retained prior census pages", phase)
				}
			}
			require.NoError(t, scenario.store.CaptureParityBaseline(t.Context(), scenario.lease, scenario.reader, scenario.binding))
			require.NoError(t, scenario.store.ValidateParityEvidence(t.Context(), scenario.lease, scenario.reader, scenario.binding, 128))
			// Keep measurement state alive until both actual caller operations finish.
			runtime.KeepAlive(scenario)
			for phase, metric := range measurements {
				assert.Equal(t, size, metric.rows, phase)
				assert.Equal(t, (size+127)/128, metric.pages, phase)
				assert.Equal(t, min(size, 128), metric.maxRows, phase)
				if size == 10 {
					smallPeaks[phase] = metric.peakLive
				}
				t.Logf("cohort=%d phase=%s delivered=%d pages=%d peakRows=%d peakLiveBytes=%d", size, phase, metric.rows, metric.pages, metric.maxRows, metric.peakLive)
			}
			var sources, members, ambiguous, observedMembers int
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1 AND inventory_kind='baseline'`, scenario.lease.RunID).Scan(&sources))
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER(WHERE verdict='ambiguous') FROM migration_parity_members WHERE run_id=$1 AND inventory_kind='baseline'`, scenario.lease.RunID).Scan(&members, &ambiguous))
			require.NoError(t, scenario.runtime.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_dependencies d JOIN migration_parity_runs r USING(run_id,init_epoch) WHERE d.run_id=$1 AND d.kind='member' AND d.validation_epoch=r.validation_epoch AND d.observed=d.expected`, scenario.lease.RunID).Scan(&observedMembers))
			assert.Equal(t, size, sources)
			assert.Equal(t, size, members)
			assert.Equal(t, size-1, ambiguous)
			assert.Equal(t, size, observedMembers)
			report, err := scenario.store.ReadParityReport(t.Context(), scenario.lease.RunID)
			require.NoError(t, err)
			assert.True(t, report.BaselineSealed)
			assert.False(t, report.Passing, "unreconstructable sources and ambiguous members must prevent pass")
		})
	}
}
