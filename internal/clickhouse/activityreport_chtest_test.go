//go:build chtest

package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func TestStoreActivityReportAndRecentEdits(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()
	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)

	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Timezone:         "UTC",
		IncludeSubagents: true,
	}, q)
	require.NoError(t, err)
	assert.False(t, report.Partial)
	assert.Equal(t, 2, report.Totals.Sessions,
		"alpha root and its subagent child on 2026-01-10")

	edits, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, edits.Files, 1)
	assert.Equal(t, "alpha", edits.Files[0].Project)
	assert.Equal(t, "src/main.go", edits.Files[0].FilePath)
	assert.Equal(t, 1, edits.Files[0].EditCount)
	assert.Equal(t, fixtureAlphaID, edits.Files[0].LastSessionID)
}
