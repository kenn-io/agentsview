package db

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Only one session needs copying. A join that scans all old messages makes
// the copy cost grow with transcripts that have already been reparsed.
func BenchmarkCopySparseOrphan(b *testing.B) {
	for _, sessions := range []int{10, 2000} {
		b.Run(strconv.Itoa(sessions), func(b *testing.B) {
			ctx := b.Context()
			source := testDB(b)
			_, err := source.getWriter().Exec(ctx, `
				WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1 < ?)
				INSERT INTO sessions(id, project, agent) SELECT 'session-'||x, 'sample', 'claude' FROM n`, sessions)
			require.NoError(b, err)
			_, err = source.getWriter().Exec(ctx, `
				WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<99)
				INSERT INTO messages(session_id, ordinal, role, content)
				SELECT s.id, n.x, 'assistant', 'Saved reply' FROM sessions s CROSS JOIN n`)
			require.NoError(b, err)
			path := source.Path()
			require.NoError(b, source.Close())
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				destination := testDB(b)
				_, err := destination.getWriter().Exec(ctx, `
					WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x+1 < ?)
					INSERT INTO sessions(id, project, agent) SELECT 'session-'||x, 'sample', 'claude' FROM n`, sessions)
				require.NoError(b, err)
				b.StartTimer()
				copied, err := destination.CopyOrphanedDataFrom(path)
				b.StopTimer()
				require.NoError(b, err)
				require.Equal(b, 1, copied)
				messages, err := destination.GetAllMessages(ctx, "session-0")
				require.NoError(b, err)
				require.Len(b, messages, 100)
				require.Equal(b, "Saved reply", messages[99].Content)
				require.Equal(b, 99, messages[99].Ordinal)
				require.NoError(b, destination.Close())
			}
		})
	}
}
