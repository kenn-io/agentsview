package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A source's retained sessions include one whose source went missing, whose
// archive is still the saved history, and leave out one the user deleted.
func TestListRetainedSessionIDsByFilePath(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := "/s/conversation.jsonl"
	for _, id := range []string{"cb:live", "cb:missing", "cb:trashed"} {
		insertSession(t, d, id, "p", func(s *Session) {
			s.Agent = "codebuddy"
			s.FilePath = Ptr(path)
		})
	}
	insertSession(t, d, "cb:other-path", "p", func(s *Session) {
		s.Agent = "codebuddy"
		s.FilePath = Ptr("/s/other.jsonl")
	})
	insertSession(t, d, "other:same-path", "p", func(s *Session) {
		s.Agent = "claude"
		s.FilePath = Ptr(path)
	})
	_, err := d.getWriter().Exec(ctx,
		`UPDATE sessions SET source_missing_at = ? WHERE id = ?`,
		"2026-07-29T15:10:00Z", "cb:missing",
	)
	require.NoError(t, err)
	require.NoError(t, d.SoftDeleteSession(ctx, "cb:trashed"))

	retained, err := d.ListRetainedSessionIDsByFilePath(ctx, path, "codebuddy")
	require.NoError(t, err)
	assert.Equal(t, []string{"cb:live", "cb:missing"}, retained)

	active, err := d.ListSessionIDsByFilePath(ctx, path, "codebuddy")
	require.NoError(t, err)
	assert.Equal(t, []string{"cb:live"}, active)
}
