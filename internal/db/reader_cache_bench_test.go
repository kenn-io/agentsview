package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkArchiveSessionReads(b *testing.B) {
	d := testDB(b)
	_, err := d.getWriter().ExecContext(b.Context(), `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10000)
		INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count, started_at, ended_at)
		SELECT printf('reader-%06d', i), 'project-' || (i % 100), 'local', 'codex', 20, 10,
			'2026-01-01T00:00:00Z', '2026-01-01T00:05:00Z' FROM n`)
	require.NoError(b, err)
	content := strings.Repeat("Synthetic archived message. ", 40)
	_, err = d.getWriter().ExecContext(b.Context(), `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 400)
		INSERT INTO messages (session_id, ordinal, role, content, content_length, timestamp)
		SELECT printf('reader-%06d', (i - 1) / 20 + 1), (i - 1) % 20,
			CASE WHEN i % 2 = 0 THEN 'assistant' ELSE 'user' END, ?, ?,
			'2026-01-01T00:00:00Z' FROM n`, content, len(content))
	require.NoError(b, err)

	for _, read := range []struct {
		name string
		get  func(context.Context, string) (*Session, error)
	}{
		{"session", d.GetSession},
		{"full", d.GetSessionFull},
	} {
		b.Run(read.name, func(b *testing.B) {
			ids := make([]string, b.N)
			for i := range ids {
				ids[i] = fmt.Sprintf("reader-%06d", i%10000+1)
			}
			_, err := read.get(b.Context(), ids[0])
			require.NoError(b, err)
			b.ReportAllocs()
			b.ResetTimer()
			for _, id := range ids {
				session, err := read.get(b.Context(), id)
				require.NoError(b, err)
				require.NotNil(b, session)
				require.Equal(b, id, session.ID)
			}
		})
	}
	b.Run("messages", func(b *testing.B) {
		ids := make([]string, b.N)
		for i := range ids {
			ids[i] = fmt.Sprintf("reader-%06d", i%20+1)
		}
		_, err := d.GetMessages(b.Context(), ids[0], 0, 20, true)
		require.NoError(b, err)
		b.ReportAllocs()
		b.ResetTimer()
		for _, id := range ids {
			messages, err := d.GetMessages(b.Context(), id, 0, 20, true)
			require.NoError(b, err)
			require.Len(b, messages, 20)
			require.Equal(b, id, messages[0].SessionID)
			require.Equal(b, content, messages[0].Content)
		}
	})
}
