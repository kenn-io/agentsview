//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStatsAllTotalsUseSameFilter(t *testing.T) {
	_, d := prepareUsageSchema(t, "agentsview_stats_filters_test")
	ctx := context.Background()
	stats, err := d.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, db.Stats{}, stats)

	for _, row := range []struct {
		id, project, machine, started, relationship string
		messages, users                             int
		automated                                   bool
		deleted                                     *string
	}{
		{"human", "project-a", "machine-a", "2026-01-04T00:00:00Z", "", 10, 5, false, nil},
		{"one-shot", "project-a", "machine-b", "2026-01-03T00:00:00Z", "", 3, 1, false, nil},
		{"automated", "project-b", "machine-b", "2026-01-02T00:00:00Z", "", 4, 1, true, nil},
		{"automated-multi", "project-b", "machine-c", "2026-01-01T00:00:00Z", "", 6, 3, true, nil},
		{"empty", "excluded", "excluded", "2025-01-01T00:00:00Z", "", 0, 0, false, nil},
		{"child", "excluded", "excluded", "2025-01-01T00:00:00Z", "subagent", 100, 50, false, nil},
		{"fork", "excluded", "excluded", "2025-01-01T00:00:00Z", "fork", 100, 50, false, nil},
		{"deleted", "excluded", "excluded", "2025-01-01T00:00:00Z", "", 100, 50, false, new("2026-02-01T00:00:00Z")},
	} {
		_, err := d.DB().ExecContext(ctx, `INSERT INTO sessions
			(id, project, machine, agent, started_at, relationship_type,
			 message_count, user_message_count, is_automated, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, row.id, row.project, row.machine, "claude",
			row.started, row.relationship, row.messages, row.users, row.automated, row.deleted)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name               string
		oneShot, automated bool
		want               db.Stats
	}{
		{"all", false, false, db.Stats{SessionCount: 4, MessageCount: 23,
			ProjectCount: 2, MachineCount: 3,
			EarliestSession: new("2026-01-01T00:00:00Z")}},
		{"exclude one-shot", true, false, db.Stats{SessionCount: 3, MessageCount: 20,
			ProjectCount: 2, MachineCount: 3,
			EarliestSession: new("2026-01-01T00:00:00Z")}},
		{"exclude automated", false, true, db.Stats{SessionCount: 2, MessageCount: 13,
			ProjectCount: 1, MachineCount: 2,
			EarliestSession: new("2026-01-03T00:00:00Z")}},
		{"exclude both", true, true, db.Stats{SessionCount: 1, MessageCount: 10,
			ProjectCount: 1, MachineCount: 1,
			EarliestSession: new("2026-01-04T00:00:00Z")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.GetStats(ctx, tc.oneShot, tc.automated)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
