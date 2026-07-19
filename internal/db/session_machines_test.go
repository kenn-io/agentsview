package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionMachinesByFilePathPlanIsArchiveCardinalityIndependent(t *testing.T) {
	// SyncAllSince performs this lookup for each old discovered source. Keep
	// each lookup constrained by the agent/path index as unrelated archive
	// cardinality grows.
	const targetPath = "/archive/shared-source.jsonl"
	plans := make(map[int]string)

	for _, archiveSize := range []int{8, 8000} {
		t.Run(fmt.Sprintf("%d_sessions", archiveSize), func(t *testing.T) {
			d := testDB(t)
			seeds := make([]storedSourcePathSeed, 0, archiveSize+1)
			seeds = append(seeds, storedSourcePathSeed{
				id:    "claude:target",
				agent: "claude",
				path:  targetPath,
			})
			for i := range archiveSize {
				seeds = append(seeds, storedSourcePathSeed{
					id:    fmt.Sprintf("codex:unrelated-%05d", i),
					agent: "codex",
					path:  targetPath,
				})
			}
			insertSessionsWithSourcePaths(t, d, seeds)

			hasSessions, matches := d.SessionMachinesByFilePathMatch(
				"claude", targetPath, defaultMachine,
			)
			require.True(t, hasSessions)
			assert.True(t, matches)

			rows, err := d.getReader().Query(
				"EXPLAIN QUERY PLAN "+sessionMachinesByFilePathSQL,
				defaultMachine, "claude", targetPath,
			)
			require.NoError(t, err)
			defer rows.Close()

			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			plan := strings.Join(details, "\n")
			assert.Contains(t, plan, "idx_sessions_agent_file_path_active")
			plans[archiveSize] = plan
		})
	}

	assert.Equal(t, plans[8], plans[8000],
		"one old-file attribution lookup must retain the same indexed plan as unrelated archive cardinality grows")
}
