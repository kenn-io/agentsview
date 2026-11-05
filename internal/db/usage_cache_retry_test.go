//go:build fts5

package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Another handle replaces the prepared install on each of the three journaled
// attempts. Later attempts can finish through the ordinary cache path.
func conflictJournaledUsageReads(t *testing.T, database *DB, filter *UsageFilter, timestamp string, afterConflict func(int)) {
	t.Helper()
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	reads := 0
	filter.Progress = func(phase string) {
		if phase != "Reading cached daily totals" || reads == 3 {
			return
		}
		reads++
		require.NoError(t, other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: reads, Role: "assistant", Timestamp: timestamp, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
		_, err := other.GetDailyUsage(t.Context(), UsageFilter{Timezone: filter.Timezone, SkipSessionCounts: true})
		require.NoError(t, err)
		if afterConflict != nil {
			afterConflict(reads)
		}
	}
	t.Cleanup(func() { assert.Equal(t, 3, reads) })
}

func TestUsageJournaledRetryPreservesGroups(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"session", "same-group", "group-b", "group-c"} {
		seedJournaledUsage(t, database, id)
	}
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key=CASE WHEN id IN ('session','same-group') THEN 'group-a' ELSE id END`)
	require.NoError(t, err)
	filter := UsageFilter{Timezone: "UTC", TopSessionsByGroup: true, TopSessionsSort: TopSessionsSortTokens}
	warmJournaledUsage(t, database, filter)
	conflictJournaledUsageReads(t, database, &filter, "2026-08-10T23:31:00Z", nil)
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "group-a", got[0].GroupKey)
	assert.Empty(t, got[0].SessionID)
	assert.Equal(t, 350, got[0].InputTokens)
	assert.Empty(t, got[1].GroupKey)
	assert.Equal(t, 200, got[1].InputTokens)
}

func TestUsageJournaledRetryPreservesOffsetDay(t *testing.T) {
	for _, consumer := range []string{"daily", "ranking", "counts"} {
		t.Run(consumer, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			timestamp := "2026-08-09T07:00:00-04:00"
			require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "assistant", Timestamp: timestamp, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100}`)}}))
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "Pacific/Kiritimati"}
			warmJournaledUsage(t, database, filter)
			conflictJournaledUsageReads(t, database, &filter, timestamp, nil)
			switch consumer {
			case "daily":
				got, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				require.Len(t, got.Daily, 1)
				assert.Equal(t, "2026-08-10", got.Daily[0].Date)
				assert.Equal(t, 250, got.Totals.InputTokens)
				assert.Equal(t, 1, got.SessionCounts.Total)
			case "ranking":
				got, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, "session", got[0].SessionID)
				assert.Equal(t, 250, got[0].InputTokens)
			case "counts":
				got, err := database.GetUsageSessionCounts(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, UsageSessionCounts{Total: 1, ByProject: map[string]int{"project": 1}, ByAgent: map[string]int{"codex": 1}}, got)
			}
		})
	}
}

func TestUsageJournaledRetryHonorsBulkLane(t *testing.T) {
	for _, consumer := range []string{"daily", "ranking", "counts"} {
		t.Run(consumer, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			warmJournaledUsage(t, database, filter)
			conflictJournaledUsageReads(t, database, &filter, "2026-08-10T23:31:00Z", func(reads int) {
				if reads == 3 {
					for range bulkReadSlots {
						release, err := database.acquireBulkRead(t.Context())
						require.NoError(t, err)
						t.Cleanup(release)
					}
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var err error
			switch consumer {
			case "daily":
				_, err = database.GetDailyUsage(ctx, filter)
			case "ranking":
				_, err = database.GetTopSessionsByCost(ctx, filter, 10)
			case "counts":
				_, err = database.GetUsageSessionCounts(ctx, filter)
			}
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}
