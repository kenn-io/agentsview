package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkSessionAnnotationFilters(b *testing.B) {
	d := testDB(b)
	ctx := b.Context()
	_, err := d.getWriter().ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 100000)
		INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count, started_at, ended_at, pr_links)
		SELECT printf('bench-%06d', n), printf('project-%d', n % 10), 'local', 'claude', 4, 2, '2026-01-01T00:00:00Z', '2026-01-01T00:02:00Z',
			CASE WHEN n % 10 = 0 THEN json_array(json_object(
				'url', 'https://github.com/acme/widgets/pull/' || (n % 100 + 1),
				'host', 'github.com', 'repository', 'acme/widgets', 'number', n % 100 + 1)) ELSE '' END
		FROM seq`)
	require.NoError(b, err)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO session_labels (session_id, label)
		SELECT id, 'nightly' FROM sessions WHERE CAST(substr(id, 7) AS INTEGER) % 1000 = 0`)
	require.NoError(b, err)
	where, args := BuildSessionFilterSQL(SessionFilter{Labels: []string{"nightly"}}, SQLiteQueryDialect())
	rows, err := d.getReader().QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT id FROM sessions WHERE "+where, args...)
	require.NoError(b, err)
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(b, rows.Scan(&id, &parent, &unused, &detail))
		b.Log(detail)
	}
	require.NoError(b, rows.Err())
	for _, tt := range []struct {
		name   string
		filter SessionFilter
	}{
		{"unfiltered", SessionFilter{}},
		{"label", SessionFilter{Labels: []string{"nightly"}}},
		{"pr", SessionFilter{PR: PRFilter{Repository: "acme/widgets", Number: 1}}},
		{"pr-project", SessionFilter{Project: "project-0", PR: PRFilter{Repository: "acme/widgets", Number: 1}}},
	} {
		b.Run(tt.name, func(b *testing.B) {
			tt.filter.Limit = 40
			b.ReportAllocs()
			for range b.N {
				_, err := d.ListSessions(b.Context(), tt.filter)
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkSessionUpsertTransaction(b *testing.B) {
	for _, linked := range []bool{false, true} {
		for _, transactional := range []bool{false, true} {
			b.Run(fmt.Sprintf("linked=%t/transaction=%t", linked, transactional), func(b *testing.B) {
				d := testDB(b)
				s := Session{ID: "worker", Project: "bench", Machine: "local", Agent: "claude", MessageCount: 4, UserMessageCount: 2}
				require.NoError(b, d.UpsertSession(b.Context(), s))
				if linked {
					require.NoError(b, d.UpsertSession(b.Context(), Session{ID: "launcher", Project: "bench", Machine: "local", Agent: "claude"}))
					_, err := d.SetSessionExternalParent(b.Context(), s.ID, "launcher")
					require.NoError(b, err)
				}
				writer := d.getWriter()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if transactional {
						require.NoError(b, d.UpsertSession(b.Context(), s))
						continue
					}
					// Autocommit is a measurement baseline; production keeps row and parent recomputation atomic.
					d.mu.Lock()
					_, err := upsertSessionExec(b.Context(), writer.ExecContext, writer.QueryRowContext, d.sessionForStorage(s), true)
					d.mu.Unlock()
					require.NoError(b, err)
				}
			})
		}
	}
}
