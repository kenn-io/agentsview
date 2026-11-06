//go:build fts5

package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CheckUsageTopSessionsMatchLegacy checks cached reads against the archive for the external fixture test.
func CheckUsageTopSessionsMatchLegacy(t *testing.T, database *DB) {
	t.Helper()
	// Owners share units with ordinary runs. Labels come from the latest named
	// member, including members below the SQL cut before the owner joins.
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='cron:'||agent, session_name='Daily report · run' WHERE id LIKE 'journal-%' AND id NOT IN ('journal-00000','journal-00020')`)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='owner', machine='machine-0', agent='claude', session_name='Owner report · run' WHERE id IN ('duplicate-a','journal-00000','journal-00020')`)
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key='reported', agent='copilot', machine='machine-0' WHERE id IN ('reported','journal-00004')`)
	require.NoError(t, err)
	filter := UsageFilter{From: "2026-08-03", To: "2026-08-10", Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	for _, tc := range usageDayTotalsFilterCases() {
		for _, sort := range []string{"cost", "tokens", "input+output"} {
			for _, limit := range []int{1, 3, 100} {
				for _, grouped := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/group=%t", tc.name, sort, limit, grouped), func(t *testing.T) {
						f := filter
						tc.change(&f)
						f.TopSessionsSort = sort
						if sort == "input+output" {
							f.TopSessionsSort = "tokens"
							f.TopSessionsTokenTypes = UsageTokenTypeInput | UsageTokenTypeOutput
						}
						f.TopSessionsByGroup = grouped
						want, err := database.getTopSessionsByCostLegacy(t.Context(), f, limit)
						require.NoError(t, err)
						if grouped {
							all, err := database.getTopSessionsByCostLegacy(t.Context(), f, 100)
							require.NoError(t, err)
							want, err = GroupTopSessions(all, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
							require.NoError(t, err)
						}
						got, err := database.GetTopSessionsByCost(t.Context(), f, limit)
						require.NoError(t, err)
						assert.Equal(t, want, got)
						if f.ActiveSince == "" && f.Termination == "" {
							prepared, err := database.prepareUsageWindow(t.Context(), f, usageQueryKindTopSessions, false, true, nil)
							require.NoError(t, err)
							require.True(t, prepared.journaled)
							assert.Empty(t, prepared.snapshot.Sessions)
							assert.Empty(t, prepared.snapshot.Versions)
						}
					})
				}
			}
		}
	}
}
