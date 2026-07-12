package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestUpsertSessionProjectIdentitySnapshotsBatchesStatements(t *testing.T) {
	snapshots := make([]export.ProjectIdentityObservation, 501)
	for i := range snapshots {
		snapshots[i] = export.ProjectIdentityObservation{
			SessionID: "session", Project: "app", Machine: "local",
		}
	}
	var argCounts []int
	err := upsertSessionProjectIdentitySnapshots(
		func(_ string, args ...any) error {
			argCounts = append(argCounts, len(args))
			return nil
		},
		"archive", "generation", snapshots,
	)
	require.NoError(t, err)
	assert.Equal(t, []int{500 * 20, 20}, argCounts)
}
