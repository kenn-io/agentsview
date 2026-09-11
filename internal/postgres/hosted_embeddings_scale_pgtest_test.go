//go:build pgtest

package postgres

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type embeddingScalePlan struct {
	Plan struct {
		SharedHitBlocks  float64 `json:"Shared Hit Blocks"`
		SharedReadBlocks float64 `json:"Shared Read Blocks"`
	} `json:"Plan"`
}

func embeddingScaleExplain(t *testing.T, f hostedFixture, query string, forceIndex bool, args ...any) (float64, float64) {
	t.Helper()
	tx, err := f.runtime.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	if forceIndex {
		_, err = tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan=off`)
		require.NoError(t, err)
	}
	var raw []byte
	require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+query, args...).Scan(&raw))
	var outer []embeddingScalePlan
	require.NoError(t, json.Unmarshal(raw, &outer))
	require.Len(t, outer, 1)
	var detailed []struct {
		Plan embeddingExplainPlan `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal(raw, &detailed))
	work, _ := embeddingPlanWork(detailed[0].Plan)
	buffers := outer[0].Plan.SharedHitBlocks + outer[0].Plan.SharedReadBlocks
	return work, buffers
}

func settleEmbeddingScaleCorpus(t *testing.T, store *HostedEmbeddingStore) {
	t.Helper()
	for round := 0; round < 80; round++ {
		result, err := store.Reconcile(t.Context(), 512)
		require.NoError(t, err)
		if result.Outbox == 0 && result.Dirty == 0 && result.Backfill == 0 && result.BackfillFinished {
			return
		}
	}
	t.Fatal("embedding scale corpus did not settle")
}

func embeddingOperationMemory(t *testing.T, runs int, operation func()) (float64, uint64, uint64) {
	t.Helper()
	operation()
	allocs := testing.AllocsPerRun(runs, operation)
	runtime.GC()
	var before, afterOperation, afterGC runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		operation()
	}
	runtime.ReadMemStats(&afterOperation)
	allocated := (afterOperation.TotalAlloc - before.TotalAlloc) / uint64(runs)
	runtime.GC()
	runtime.ReadMemStats(&afterGC)
	if afterGC.HeapAlloc <= before.HeapAlloc {
		return allocs, allocated, 0
	}
	return allocs, allocated, afterGC.HeapAlloc - before.HeapAlloc
}

// Replacing the partial dirty index with a corpus scan, retaining result rows,
// or letting one source change fan out into an unbounded retry queue fails as
// the unrelated corpus grows from 100 to 10,000 sessions.
func TestHostedEmbeddingReconcileDeltaStaysBoundedAtCorpusScale(t *testing.T) {
	type measurement struct {
		idleWork, idleBuffers   float64
		deltaWork, deltaBuffers float64
		idleAllocs, deltaAllocs float64
		idleBytes, deltaBytes   uint64
		idleRetained            uint64
		deltaRetained           uint64
	}
	results := map[int]measurement{}
	for _, count := range []int{100, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f, store, _ := embeddingFixture(t)
			_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,prompt_evidence_discarded)
 SELECT 'scale-'||lpad(n::text,5,'0'),'synthetic','fixture','codex',true FROM generate_series(1,$1::int)n`, count)
			require.NoError(t, err)
			settleEmbeddingScaleCorpus(t, store)
			// Measure settled indexed work after PostgreSQL has reclaimed the
			// partial-index entries removed by the one-time corpus backfill.
			_, err = f.admin.ExecContext(t.Context(), `VACUUM ANALYZE hosted_embedding_sources`)
			require.NoError(t, err)
			_, err = f.admin.ExecContext(t.Context(), `VACUUM ANALYZE hosted_embedding_requirements`)
			require.NoError(t, err)

			idleWork, idleBuffers := embeddingScaleExplain(t, f, hostedEmbeddingDirtySQL, count == 100, f.tenant, 1)
			idleAllocs, idleBytes, idleRetained := embeddingOperationMemory(t, 10, func() {
				result, reconcileErr := store.Reconcile(t.Context(), 1)
				if reconcileErr != nil || result.Outbox != 0 || result.Dirty != 0 || result.Backfill != 0 {
					t.Fatalf("idle reconcile returned result=%+v err=%v", result, reconcileErr)
				}
			})
			measureEligible := false
			deltaAllocs, deltaBytes, deltaRetained := embeddingOperationMemory(t, 10, func() {
				measureEligible = !measureEligible
				_, updateErr := f.runtime.ExecContext(t.Context(), `UPDATE sessions SET prompt_evidence_discarded=$1 WHERE id='scale-00002'`, !measureEligible)
				if updateErr != nil {
					t.Fatal(updateErr)
				}
				result, reconcileErr := store.Reconcile(t.Context(), 1)
				if reconcileErr != nil || result.Dirty != 1 {
					t.Fatalf("delta reconcile returned result=%+v err=%v", result, reconcileErr)
				}
			})
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET prompt_evidence_discarded=true WHERE id='scale-00002'`)
			require.NoError(t, err)
			_, err = store.Reconcile(t.Context(), 1)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET prompt_evidence_discarded=false WHERE id='scale-00001'`)
			require.NoError(t, err)
			deltaWork, deltaBuffers := embeddingScaleExplain(t, f, hostedEmbeddingDirtySQL, count == 100, f.tenant, 1)
			delta, err := store.Reconcile(t.Context(), 1)
			require.NoError(t, err)
			assert.Equal(t, 1, delta.Dirty)
			status, err := store.Status(t.Context())
			require.NoError(t, err)
			require.NotNil(t, status.Desired)
			assert.Equal(t, int64(1), status.Desired.Ready)
			leases, err := store.Claim(t.Context(), "scale-worker", 1, time.Minute)
			require.NoError(t, err)
			require.Len(t, leases, 1)
			require.NoError(t, store.Fail(t.Context(), leases[0], "encoder_unavailable", true))
			status, err = store.Status(t.Context())
			require.NoError(t, err)
			assert.Equal(t, int64(1), status.Desired.Retry)

			results[count] = measurement{
				idleWork: idleWork, idleBuffers: idleBuffers, deltaWork: deltaWork, deltaBuffers: deltaBuffers,
				idleAllocs: idleAllocs, deltaAllocs: deltaAllocs, idleBytes: idleBytes, deltaBytes: deltaBytes,
				idleRetained: idleRetained, deltaRetained: deltaRetained,
			}
			t.Logf("corpus=%d idle leaf tuples=%.0f buffers=%.0f allocs=%.0f bytes/op=%d retained=%d; delta leaf tuples=%.0f buffers=%.0f allocs=%.0f bytes/op=%d retained=%d", count, idleWork, idleBuffers, idleAllocs, idleBytes, idleRetained, deltaWork, deltaBuffers, deltaAllocs, deltaBytes, deltaRetained)
			assert.Zero(t, idleWork)
			assert.LessOrEqual(t, deltaWork, float64(2))
			assert.Less(t, idleBuffers, float64(64))
			assert.Less(t, deltaBuffers, float64(64))
			assert.Less(t, idleBytes, uint64(1<<20))
			assert.Less(t, deltaBytes, uint64(2<<20))
		})
	}
	small, large := results[100], results[10000]
	assert.LessOrEqual(t, large.idleBuffers, small.idleBuffers+16)
	assert.LessOrEqual(t, large.deltaBuffers, small.deltaBuffers+16)
	assert.LessOrEqual(t, large.idleAllocs, small.idleAllocs+64)
	assert.LessOrEqual(t, large.deltaAllocs, small.deltaAllocs+64)
	assert.LessOrEqual(t, large.idleBytes, small.idleBytes+(64<<10))
	assert.LessOrEqual(t, large.deltaBytes, small.deltaBytes+(64<<10))
	assert.LessOrEqual(t, large.idleRetained, small.idleRetained+(2<<20))
	assert.LessOrEqual(t, large.deltaRetained, small.deltaRetained+(2<<20))
}
