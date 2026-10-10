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

// A legacy page must be checked against ordinary orphans without scanning
// the entire orphan inventory once for every page.
func BenchmarkCopyMixedLegacyPageOrphans(b *testing.B) {
	for _, sessionsPerAgent := range []int{100, 10000} {
		b.Run(strconv.Itoa(sessionsPerAgent), func(b *testing.B) {
			ctx := b.Context()
			source := testDB(b)
			_, err := source.getWriter().Exec(ctx, `
				WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1 < ?)
				INSERT INTO sessions(id, project, agent, file_path)
				SELECT 'claude:'||x, 'sample', 'claude', NULL FROM n
				UNION ALL
				SELECT 'codex:'||printf('%08x-1111-4111-8111-111111111111',x), 'sample', 'codex',
					'rollout-2026-09-01T00-00-00-'||printf('%08x-1111-4111-8111-111111111111',x)||
					'_22222222-2222-4222-8222-222222222222.jsonl' FROM n;
				INSERT INTO messages(session_id, ordinal, role, content)
				SELECT id, 0, 'assistant', 'Saved reply' FROM sessions;
				PRAGMA user_version=130`, sessionsPerAgent)
			require.NoError(b, err)
			path := source.Path()
			require.NoError(b, source.Close())
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				destination := testDB(b)
				b.StartTimer()
				copied, err := destination.CopyOrphanedDataFrom(path)
				b.StopTimer()
				require.NoError(b, err)
				require.Equal(b, 2*sessionsPerAgent, copied)
				for _, id := range []string{
					"claude:0",
					"codex:00000000-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
				} {
					messages, err := destination.GetAllMessages(ctx, id)
					require.NoError(b, err)
					require.Len(b, messages, 1)
					require.Equal(b, "Saved reply", messages[0].Content)
				}
				require.NoError(b, destination.Close())
			}
		})
	}
}

// Populated archive statistics must not turn pin migration into one
// transcript scan per unpinned message, even when the pin table is empty.
func BenchmarkCopySessionMetadataPins(b *testing.B) {
	const seed = `INSERT INTO sessions(id, project, agent) VALUES ('chat', 'sample', 'claude');
		WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x+1 < ?)
		INSERT INTO messages(session_id, ordinal, role, content, source_uuid)
		SELECT 'chat', x, 'assistant', 'Saved reply '||x, 'message-'||x FROM n`
	for _, messages := range []int{128, 8192} {
		for _, pinCount := range []int{0, 1} {
			b.Run(strconv.Itoa(messages)+"/pins-"+strconv.Itoa(pinCount), func(b *testing.B) {
				ctx := b.Context()
				source := testDB(b)
				_, err := source.getWriter().Exec(ctx, seed, messages)
				require.NoError(b, err)
				if pinCount != 0 {
					var messageID int64
					require.NoError(b, source.getWriter().QueryRow(ctx,
						`SELECT id FROM messages WHERE session_id='chat' AND ordinal=7`).Scan(&messageID))
					id, err := source.PinMessage(ctx, "chat", messageID, new("Keep this reply"))
					require.NoError(b, err)
					require.Positive(b, id)
				}
				// Writable opens maintain planner statistics for populated archives.
				_, err = source.getWriter().Exec(ctx, "ANALYZE")
				require.NoError(b, err)
				path := source.Path()
				require.NoError(b, source.Close())
				b.ReportAllocs()
				b.ResetTimer()
				b.StopTimer()
				for range b.N {
					destination := testDB(b)
					_, err := destination.getWriter().Exec(ctx, seed, messages)
					require.NoError(b, err)
					b.StartTimer()
					err = destination.CopySessionMetadataFrom(path)
					b.StopTimer()
					require.NoError(b, err)
					pins, err := destination.ListPinnedMessages(ctx, "", "")
					require.NoError(b, err)
					require.Len(b, pins, pinCount)
					if pinCount != 0 {
						require.Equal(b, "chat", pins[0].SessionID)
						require.Equal(b, new("Keep this reply"), pins[0].Note)
						require.Equal(b, new("Saved reply 7"), pins[0].Content)
					}
					require.NoError(b, destination.Close())
				}
			})
		}
	}
}
