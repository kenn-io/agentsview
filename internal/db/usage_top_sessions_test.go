//go:build fts5

package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageTopSessionsLoadsOnlyReturnedMetadata(t *testing.T) {
	database := testDB(t)
	for i := range 128 {
		id := fmt.Sprintf("owner-%03d", i)
		seedJournaledUsage(t, database, id)
		groupKey := ""
		if i >= 126 {
			groupKey = "daily:job"
		}
		_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET display_name=?, session_name=?, group_key=? WHERE id=?`, fmt.Sprintf("Owner %03d", i), fmt.Sprintf("Report %03d · run", i), groupKey, id)
		require.NoError(t, err)
		cost := money.Money{Microdollars: int64(i+1) * 1000}
		require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), id, []UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: CopilotReportedCostSource, OccurredAt: "2026-08-10T23:30:00Z", DedupKey: id}}))
	}
	seedJournaledUsage(t, database, "ordinary")
	filter := UsageFilter{Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)

	// Observe display-name evaluations at the archive query boundary, preserving
	// NULLIF semantics. Cached group labels require no archive name reads.
	database.rawReader().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	var loaded []string
	require.NoError(t, conn.Raw(func(driverConn any) error {
		return driverConn.(interface{ RegisterFunc(string, any, bool) error }).RegisterFunc("nullif", func(value, other any) any {
			if name, ok := value.(string); ok && strings.HasPrefix(name, "Owner ") {
				loaded = append(loaded, name)
			}
			if value == other {
				return nil
			}
			return value
		}, false)
	}))
	require.NoError(t, conn.Close())
	for _, tc := range []struct {
		name    string
		grouped bool
		limit   int
		loaded  []string
	}{
		{"individual", false, 1, []string{"Owner 127"}},
		{"group", true, 1, nil},
		{"group and individual", true, 2, []string{"Owner 125"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loaded = nil
			filter.TopSessionsByGroup = tc.grouped
			got, err := database.GetTopSessionsByCost(t.Context(), filter, tc.limit)
			require.NoError(t, err)
			assert.Equal(t, tc.loaded, loaded)
			if tc.grouped {
				require.Len(t, got, tc.limit+1)
				assert.Equal(t, "Report 127", got[0].GroupLabel)
				assert.Equal(t, "Report 127", got[0].DisplayName)
				assert.Equal(t, int64(255000), got[0].Cost.Microdollars)
				if tc.limit == 2 {
					assert.Equal(t, "owner-125", got[1].SessionID)
					assert.Equal(t, "Owner 125", got[1].DisplayName)
					assert.Equal(t, "Report 125 · run", got[1].SessionName)
					assert.Empty(t, got[1].GroupLabel)
				}
			} else {
				require.Len(t, got, 1)
				assert.Equal(t, "owner-127", got[0].SessionID)
				assert.Equal(t, "Owner 127", got[0].DisplayName)
				assert.Equal(t, "Report 127 · run", got[0].SessionName)
				assert.Empty(t, got[0].GroupLabel)
			}
		})
	}
}

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

// CheckUsageTopSessionsBoundedGroupMembers checks bounded candidates for the external fixture test.
func CheckUsageTopSessionsBoundedGroupMembers(t *testing.T, database *DB, filter UsageFilter) {
	t.Helper()
	database.StopUsageCacheBackfill()
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET project='project-0', machine='machine-0', group_key='daily:job', started_at='invalid', session_name=''`)
	require.NoError(t, err)
	for _, row := range []struct{ id, started, name string }{
		{"journal-00001", "2026-08-08T13:00:00.000000009+01:00", "Earlier report · run"},
		{"journal-00002", "2026-08-08T12:00:00.000000010Z", "Tied report · run"},
		{"journal-00003", "2026-08-08T08:00:00.000000010-04:00", "Latest report · run"},
		{"journal-00010", "2026-08-09T12:00:00Z", "daily · run"},
	} {
		_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET started_at=?, session_name=? WHERE id=?`, row.started, row.name, row.id)
		require.NoError(t, err)
	}
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET agent='claude' WHERE id='journal-00010'`)
	require.NoError(t, err)
	filter.TopSessionsByGroup = true
	warmJournaledUsage(t, database, filter)
	_, facts, _, err := database.queryUsageRollups(t.Context(), filter, usageQueryKindTopSessions, false, 1)
	require.NoError(t, err)
	require.NotNil(t, facts.TopSessions)
	var loaded []string
	for _, entry := range facts.TopSessions.Entries {
		loaded = append(loaded, entry.SessionID)
	}
	// Replacing representative selection with all members loads 400 rows.
	assert.Equal(t, []string{"journal-00000", "journal-00003", "journal-00010"}, loaded)
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].SessionID)
	assert.Equal(t, "codex", got[0].Agent)
	assert.Equal(t, "2026-08-09T12:00:00Z", got[0].StartedAt)
	assert.Equal(t, "Latest report", got[0].GroupLabel)
	assert.Equal(t, "Latest report", got[0].DisplayName)
	assert.Equal(t, 120000, got[0].InputTokens)
	assert.Equal(t, 24000, got[0].OutputTokens)
	assert.Equal(t, 144000, got[0].TotalTokens)
	assert.Equal(t, int64(168000), got[0].Cost.Microdollars)

	// A name-only edit must refresh the cached label and select its successor.
	_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET session_name='daily · run' WHERE id='journal-00003'`)
	require.NoError(t, err)
	got, err = database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Tied report", got[0].GroupLabel)
	assert.Equal(t, int64(168000), got[0].Cost.Microdollars)
}
