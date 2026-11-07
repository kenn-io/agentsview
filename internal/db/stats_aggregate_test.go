package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsCanonicalSessionCountPreservesOtherTotals(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	stats, err := d.GetStats(ctx, false, false)
	require.NoError(t, err)
	assert.Equal(t, Stats{}, stats)

	for _, row := range []struct {
		id, project, machine, started, relationship string
		parent                                      *string
		messages, users                             int
		automated                                   bool
		deleted                                     *string
	}{
		{"human", "project-a", "machine-a", "2026-01-04T00:00:00Z", "", nil, 10, 5, false, nil},
		{"one-shot", "project-a", "machine-b", "2026-01-03T00:00:00Z", "", nil, 3, 1, false, nil},
		{"automated", "project-b", "machine-b", "2026-01-02T00:00:00Z", "", nil, 4, 1, true, nil},
		{"automated-multi", "project-b", "machine-c", "2026-01-01T00:00:00Z", "", nil, 6, 3, true, nil},
		{"empty", "excluded", "excluded", "2025-01-01T00:00:00Z", "", nil, 0, 0, false, nil},
		{"fork", "excluded", "excluded", "2025-01-01T00:00:00Z", "fork", nil, 100, 50, false, nil},
		{"orphan", "excluded", "excluded", "2025-01-01T00:00:00Z", "subagent", new("missing-parent"), 100, 50, false, nil},
		{"parented-child", "excluded", "excluded", "2025-01-01T00:00:00Z", "subagent", new("human"), 100, 50, false, nil},
		{"continuation", "project-a", "machine-a", "2026-01-05T00:00:00Z", "continuation", new("human"), 2, 2, false, nil},
		{"deleted", "excluded", "excluded", "2025-01-01T00:00:00Z", "", nil, 100, 50, false, new("2026-02-01T00:00:00Z")},
	} {
		_, err := d.getWriter().ExecContext(ctx, `INSERT INTO sessions
			(id, project, machine, agent, started_at, relationship_type,
			 message_count, user_message_count, is_automated, deleted_at, parent_session_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row.id, row.project, row.machine, "claude",
			row.started, row.relationship, row.messages, row.users, row.automated, row.deleted, row.parent)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name               string
		oneShot, automated bool
		want               Stats
	}{
		{"all", false, false, Stats{
			SessionCount: 6, MessageCount: 25,
			ProjectCount: 2, MachineCount: 3,
			EarliestSession: new("2026-01-01T00:00:00Z"),
		}},
		{"exclude one-shot", true, false, Stats{
			SessionCount: 5, MessageCount: 22,
			ProjectCount: 2, MachineCount: 3,
			EarliestSession: new("2026-01-01T00:00:00Z"),
		}},
		{"exclude automated", false, true, Stats{
			SessionCount: 4, MessageCount: 15,
			ProjectCount: 1, MachineCount: 2,
			EarliestSession: new("2026-01-03T00:00:00Z"),
		}},
		{"exclude both", true, true, Stats{
			SessionCount: 3, MessageCount: 12,
			ProjectCount: 1, MachineCount: 1,
			EarliestSession: new("2026-01-04T00:00:00Z"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.GetStats(ctx, tc.oneShot, tc.automated)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			index, err := d.GetSidebarSessionIndex(ctx, SessionFilter{ExcludeOneShot: tc.oneShot, ExcludeAutomated: tc.automated})
			require.NoError(t, err)
			assert.Equal(t, index.Total, got.SessionCount)
		})
	}
}
