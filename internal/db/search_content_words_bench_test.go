package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSearchContentWordPages measures complete searches over 100,000
// messages, with each common word in one third or one eighth of the corpus.
func BenchmarkSearchContentWordPages(b *testing.B) {
	for _, frequency := range []int{3, 8} {
		b.Run(fmt.Sprintf("one-in-%d", frequency), func(b *testing.B) {
			d := testDB(b)
			if !d.HasFTS(b.Context()) {
				b.Skip("fts5 not available")
			}
			for _, stmt := range []string{
				`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<1999)
				 INSERT INTO sessions(id, project, agent, first_message, started_at, ended_at, created_at, message_count, user_message_count)
				 SELECT printf('s-%05d',i), 'bench', 'claude', 'hello',
				 strftime('%Y-%m-%dT%H:%M:%SZ','2026-01-01',printf('+%d minutes',i*60)),
				 strftime('%Y-%m-%dT%H:%M:%SZ','2026-01-01',printf('+%d minutes',i*60+50)),
				 '2026-01-01', 50, 25 FROM n`,
				fmt.Sprintf(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<49)
				 INSERT INTO messages(session_id,ordinal,role,content,timestamp,content_length)
				 SELECT sid,i,'assistant', CASE WHEN g %% %[1]d=0 THEN 'alpha extra ' ELSE '' END ||
				 CASE WHEN (g/%[1]d) %% %[1]d=0 THEN 'beta extra ' ELSE '' END ||
				 CASE WHEN (g/(%[1]d*%[1]d)) %% %[1]d=0 THEN 'gamma extra ' ELSE '' END ||
				 'filler text ' || hex(randomblob(500+(g%%3)*500)) || ' ' || g, st, 1000+(g%%3)*1000
				 FROM (SELECT s.id sid,n.i i,s.started_at st,CAST(substr(s.id,3) AS INTEGER)*50+n.i g FROM sessions s CROSS JOIN n)`, frequency),
				`ANALYZE`,
			} {
				_, err := d.getWriter().Exec(b.Context(), stmt)
				require.NoError(b, err)
			}
			for _, tc := range []struct {
				name, mode, pattern string
				cursor              int
			}{
				{"default-full", "", "alpha beta gamma", 0},
				{"fts-full", "fts", "alpha beta gamma", 0},
				{"default-full-later", "", "alpha beta gamma", 100},
				{"fts-full-later", "fts", "alpha beta gamma", 100},
				{"default-partial", "", "alpha beta absentword", 0},
				{"fts-partial", "fts", "alpha beta absentword", 0},
				{"default-partial-later", "", "alpha beta absentword", 250},
				{"fts-partial-later", "fts", "alpha beta absentword", 250},
			} {
				b.Run(tc.name, func(b *testing.B) {
					f := ContentSearchFilter{Pattern: tc.pattern, Mode: tc.mode, Limit: 25, Cursor: tc.cursor, Sources: []string{"messages"}}
					for range b.N {
						page, err := d.SearchContent(b.Context(), f)
						require.NoError(b, err)
						require.Len(b, page.Matches, 25)
					}
				})
			}
		})
	}
}
