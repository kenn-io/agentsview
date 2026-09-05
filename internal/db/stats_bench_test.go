package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The sidebar polls these totals even when no transcript is open.
func BenchmarkGetStats(b *testing.B) {
	d := testDB(b)
	_, err := d.getWriter().Exec(`WITH RECURSIVE n(i) AS (
		SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 10000
		) INSERT INTO sessions (id, project, machine, agent, message_count,
		user_message_count, started_at)
		SELECT 'session-' || i, 'project-' || (i % 100), 'machine-' || (i % 3),
		'claude', 10, 5, '2026-01-01T00:00:00Z' FROM n`)
	require.NoError(b, err)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		stats, err := d.GetStats(ctx, false, false)
		require.NoError(b, err)
		require.Equal(b, 10000, stats.SessionCount)
		require.Equal(b, 100000, stats.MessageCount)
	}
}
