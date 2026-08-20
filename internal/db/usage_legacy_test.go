//go:build fts5

package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDailyUsageFactsMatchesLegacy(t *testing.T) {
	tests := []struct {
		name   string
		filter UsageFilter
	}{
		{name: "one day", filter: UsageFilter{
			From: "2024-06-15", To: "2024-06-15", Timezone: "UTC",
		}},
		{name: "seven days", filter: UsageFilter{
			From: "2024-06-09", To: "2024-06-15", Timezone: "UTC",
		}},
		{name: "thirty days", filter: UsageFilter{
			From: "2024-05-17", To: "2024-06-15", Timezone: "UTC",
		}},
		{name: "all history", filter: UsageFilter{Timezone: "UTC"}},
		{name: "breakdowns", filter: UsageFilter{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Breakdowns: true,
		}},
		{name: "project", filter: UsageFilter{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Project: "proj-a",
		}},
		{name: "model", filter: UsageFilter{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Model: "gpt-5",
		}},
		{name: "skip counts", filter: UsageFilter{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			SkipSessionCounts: true,
		}},
		{name: "empty", filter: UsageFilter{
			From: "2025-01-01", To: "2025-01-07", Timezone: "UTC",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := openDailyUsageFixtureDB(t)
			legacy, err := database.getDailyUsageLegacy(
				context.Background(), test.filter,
			)
			require.NoError(t, err)
			facts, err := database.GetDailyUsage(
				context.Background(), test.filter,
			)
			require.NoError(t, err)
			assert.Equal(t, legacy, facts)
		})
	}
}
