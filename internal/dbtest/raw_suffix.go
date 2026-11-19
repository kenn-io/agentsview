package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// SeedRawSuffix writes sessions whose IDs cover each raw session ID form:
// bare, agent-prefixed, host-prefixed, a raw ID that itself contains a
// colon, a trailing delimiter, and SQL wildcard characters. Call
// TrashRawSuffix before AssertRawSuffix.
func SeedRawSuffix(t *testing.T, d *db.DB) {
	t.Helper()
	for _, s := range []struct{ id, ended string }{
		{"plain-id", "2026-01-01T00:00:00Z"},
		{"claude:uuid-1", "2026-01-01T00:00:00Z"},
		{"codex:uuid-1", "2026-01-02T00:00:00Z"},
		{"host~codex:uuid-1", "2026-01-03T00:00:00Z"},
		{"host~uuid-1-fork", "2026-01-04T00:00:00Z"},
		{"kimi:project-hash:session-x", "2026-01-01T00:00:00Z"},
		{"host~wild_%_literal", "2026-01-01T00:00:00Z"},
		{"host~trailing:", "2026-01-01T00:00:00Z"},
		{"host~trashed", "2026-01-01T00:00:00Z"},
	} {
		SeedSession(t, d, s.id, "raw-suffix", func(sess *db.Session) {
			sess.StartedAt = new(s.ended)
			sess.EndedAt = new(s.ended)
		})
	}
}

// TrashRawSuffix moves one SeedRawSuffix session to the trash. A mirror
// pushed between SeedRawSuffix and this call keeps a live older version of
// that session.
func TrashRawSuffix(t *testing.T, d *db.DB) {
	t.Helper()
	require.NoError(t, d.SoftDeleteSession(t.Context(), "host~trashed"))
}

// AssertRawSuffix checks raw session ID resolution for SeedRawSuffix. An
// exact match comes first, then matches by most recent activity.
func AssertRawSuffix(t *testing.T, store db.Store) {
	t.Helper()
	for _, tc := range []struct {
		raw   string
		limit int
		want  []string
	}{
		{"uuid-1", 5, []string{"host~codex:uuid-1", "codex:uuid-1", "claude:uuid-1"}},
		{"uuid-1", 2, []string{"host~codex:uuid-1", "codex:uuid-1"}},
		{"codex:uuid-1", 5, []string{"codex:uuid-1", "host~codex:uuid-1"}},
		{"project-hash:session-x", 5, []string{"kimi:project-hash:session-x"}},
		{"session-x", 5, []string{"kimi:project-hash:session-x"}},
		{"plain-id", 5, []string{"plain-id"}},
		{"wild_%_literal", 5, []string{"host~wild_%_literal"}},
		{"trailing:", 5, []string{"host~trailing:"}},
		{"fork", 5, nil},
		{"wild", 5, nil},
		{"trashed", 5, nil},
		{"missing", 5, nil},
	} {
		got, err := store.FindSessionIDsByRawSuffix(t.Context(), tc.raw, tc.limit)
		require.NoError(t, err, "raw %q", tc.raw)
		if tc.want == nil {
			assert.Empty(t, got, "raw %q", tc.raw)
			continue
		}
		assert.Equal(t, tc.want, got, "raw %q limit %d", tc.raw, tc.limit)
	}
}
