package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedAutomationAuditRows(tb testing.TB, d *DB, sessions, promptBytes int) {
	tb.Helper()
	_, err := d.getWriter().Exec(tb.Context(), `
		WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1 < ?)
		INSERT INTO sessions(id, project, agent, first_message, user_message_count)
		SELECT 'session-'||x, 'project-a', 'claude', ?, 1 FROM n`,
		sessions, strings.Repeat("x", promptBytes))
	require.NoError(tb, err)
}

func BenchmarkBackfillIsAutomatedUnchanged(b *testing.B) {
	for _, sessions := range []int{1000, 10000} {
		for _, promptBytes := range []int{4096, 65536} {
			b.Run(fmt.Sprintf("sessions-%d/prompt-%d", sessions, promptBytes), func(b *testing.B) {
				d := testDB(b)
				seedAutomationAuditRows(b, d, sessions, promptBytes)
				require.NoError(b, d.backfillIsAutomatedLocked(b.Context(), d.getWriter()))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					require.NoError(b, d.backfillIsAutomatedLocked(b.Context(), d.getWriter()))
				}
			})
		}
	}
}
