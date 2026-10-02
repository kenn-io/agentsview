package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSessionsSourceComparisonMetadata(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "codex:copy", "app", func(s *Session) {
		s.Cwd = "/workspace/app"
		s.IsTruncated = true
		s.FilePath = new("/archive/session.jsonl")
		s.FileSize = new(int64(1234))
	})
	_, err := d.getWriter().ExecContext(t.Context(), "UPDATE sessions SET local_modified_at = ? WHERE id = ?", "2026-09-01T12:00:00Z", "codex:copy")
	require.NoError(t, err)
	for _, source := range []bool{false, true} {
		page, err := d.ListSessions(t.Context(), SessionFilter{IDs: []string{"codex:copy"}, IncludeSource: source})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		s := page.Sessions[0]
		assert.Equal(t, "/workspace/app", s.Cwd)
		assert.True(t, s.IsTruncated)
		if source {
			require.NotNil(t, s.FileSize)
			assert.Equal(t, int64(1234), *s.FileSize)
			require.NotNil(t, s.LocalModifiedAt)
			assert.Equal(t, "2026-09-01T12:00:00Z", *s.LocalModifiedAt)
			require.NotNil(t, s.FilePath)
			assert.Equal(t, "/archive/session.jsonl", *s.FilePath)
		} else {
			assert.Nil(t, s.FileSize)
			assert.Nil(t, s.LocalModifiedAt)
			assert.Nil(t, s.FilePath)
		}
	}
}
