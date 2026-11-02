package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkSidebarSessionIndexRoots(b *testing.B) {
	for _, roots := range []int{1000, 5000} {
		for _, limit := range []int{20, 0} {
			b.Run(fmt.Sprintf("roots=%d/limit=%d", roots, limit), func(b *testing.B) {
				d := testDB(b)
				_, err := d.getWriter().ExecContext(b.Context(), `
				WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
				INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count, started_at, ended_at)
				SELECT printf('bench-%06d', n), 'project', 'local', 'claude', 4, 2, '2026-01-01T00:00:00Z', '2026-01-01T00:02:00Z'
				FROM seq`, roots)
				require.NoError(b, err)
				path := d.Path()
				require.NoError(b, d.Close())
				d, err = OpenIsolated(b.Context(), path)
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, d.Close()) })

				filter := SessionFilter{Limit: limit, IncludeChildren: true, ExcludeAutomated: true, ExcludeOneShot: true}
				wantSessions := limit
				if limit == 0 {
					wantSessions = roots
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					index, err := d.GetSidebarSessionIndex(b.Context(), filter)
					require.NoError(b, err)
					require.Len(b, index.Sessions, wantSessions)
				}
			})
		}
	}
}
