//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuckListProjectIdentityObservationsChunksLargeLabelLists(t *testing.T) {
	ctx := context.Background()
	database := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, database))

	// Cross the duckMaxSQLVars chunk boundary with duplicate labels that
	// straddle it, and interleave two source archives across the label
	// range: the ORDER BY leads with source_archive_id, so concatenated
	// per-chunk (label-range) results are not globally ordered until the
	// Go re-sort restores the documented ordering.
	const labelCount = duckMaxSQLVars + 50
	observedAt := time.Date(2025, 6, 2, 10, 0, 0, 0, time.UTC)
	labels := make([]string, 0, 2*labelCount)
	const batch = 200
	for start := 0; start < labelCount; start += batch {
		end := min(start+batch, labelCount)
		var sb strings.Builder
		sb.WriteString(`INSERT INTO source_project_identity_observations
			(source_archive_id, project, machine, root_path, observed_at) VALUES `)
		args := make([]any, 0, (end-start)*3)
		for i := start; i < end; i++ {
			label := fmt.Sprintf("chunked-project-%04d", i)
			labels = append(labels, label, label)
			archive := "arch-a"
			if i%2 == 0 {
				archive = "arch-b"
			}
			if i > start {
				sb.WriteString(",")
			}
			sb.WriteString("(?, ?, 'host.example', '/srv/app', ?)")
			args = append(args, archive, label, observedAt)
		}
		_, err := database.ExecContext(ctx, sb.String(), args...)
		require.NoError(t, err, "seed chunked observations %d-%d", start, end)
	}

	store := NewStoreFromDB(database)
	// Reverse the (duplicated) label list to prove the lookup sorts it
	// before partitioning into chunks.
	slices.Reverse(labels)
	got, err := store.ListProjectIdentityObservations(ctx, labels)
	require.NoError(t, err)
	all, err := store.ListProjectIdentityObservations(ctx, nil)
	require.NoError(t, err)
	require.Len(t, all, labelCount)
	assert.Equal(t, all, got,
		"chunked label lookup must match the unfiltered scan, order included")
}
