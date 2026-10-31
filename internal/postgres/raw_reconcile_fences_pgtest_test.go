//go:build pgtest

package postgres

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// Each surviving fence can independently dominate the job/projection tuple.
// MaxInt64 is an exhausted fence; it must remain unchanged rather than wrap.
func TestRawReconcileSurvivingGenerationFences(t *testing.T) {
	for _, table := range []string{"raw_ingest_jobs", "raw_source_projections", "raw_projection_generations", "raw_session_branches", "session_sources", "raw_source_contributions"} {
		for _, high := range []int64{37, math.MaxInt64 - 1, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s/%d", table, high), func(t *testing.T) {
				f := newProjectionFixture(t)
				m, _ := f.accept(t, "device", "fence", "")
				require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("fence")))
				var query string
				if table == "raw_projection_generations" {
					query = `INSERT INTO raw_projection_generations(source_id,generation,manifest_id,processing_version) SELECT source_id,$1,selected_manifest_id,processing_version FROM raw_source_projections`
				} else {
					query = `UPDATE ` + table + ` SET projection_generation=$1`
				}
				_, err := f.runtime.ExecContext(t.Context(), query, high)
				require.NoError(t, err)
				_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET projection_selected=false`)
				require.NoError(t, err)
				before := rawAuthoritativeSnapshot(t, f)
				dry, err := f.sink.ReconcileRawSources(t.Context(), false)
				require.NoError(t, err)
				require.Equal(t, map[bool]int{true: 1, false: 0}[high == math.MaxInt64], dry.Unrecoverable)
				require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
				got, err := f.sink.ReconcileRawSources(t.Context(), true)
				if high == math.MaxInt64 {
					require.NoError(t, err)
					require.Equal(t, 1, got.Unrecoverable)
					require.Equal(t, before, rawAuthoritativeSnapshot(t, f))
					return
				}
				require.NoError(t, err)
				require.Equal(t, 1, got.Repaired)
				var generation int64
				require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT projection_generation FROM raw_source_projections WHERE source_id=$1`, rawSourceID(m)).Scan(&generation))
				require.Equal(t, high+1, generation)
			})
		}
	}
}

func TestRawReconcileRepairsAgreeingTupleBelowRetainedGeneration(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "retained-fence", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("retained")))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO raw_projection_generations(source_id,generation,manifest_id,processing_version) SELECT source_id,37,selected_manifest_id,processing_version FROM raw_source_projections`)
	require.NoError(t, err)
	before := rawAuthoritativeSnapshot(t, f)
	dry, err := f.sink.ReconcileRawSources(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, 1, dry.Found)
	require.Zero(t, dry.Repaired)
	require.Zero(t, dry.Unrecoverable)
	require.Equal(t, before, rawAuthoritativeSnapshot(t, f))

	got, err := f.sink.ReconcileRawSources(t.Context(), true)
	require.NoError(t, err)
	require.Equal(t, 1, got.Repaired)
	require.Zero(t, got.Unrecoverable)
	var generation int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT projection_generation FROM raw_source_projections WHERE source_id=$1`, rawSourceID(m)).Scan(&generation))
	require.Equal(t, int64(38), generation)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT generation FROM raw_projection_generations WHERE source_id=$1 AND generation=38`, rawSourceID(m)).Scan(&generation))

	after := rawAuthoritativeSnapshot(t, f)
	again, err := f.sink.ReconcileRawSources(t.Context(), true)
	require.NoError(t, err)
	require.Zero(t, again.Repaired)
	require.Equal(t, after, rawAuthoritativeSnapshot(t, f))
}
