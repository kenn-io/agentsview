//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPGSessionReadsDecorateDuplicateRoles verifies that PG session and
// sidebar reads fill the duplicate indicator fields from the mirrored
// duplicate_group_members table, so mirror-served UIs show the same
// duplicate badges as the SQLite archive.
func TestPGSessionReadsDecorateDuplicateRoles(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()
	ctx := context.Background()

	_, err = store.DB().Exec(`
		INSERT INTO sessions (id, machine, project, agent, message_count)
		VALUES ('dup:canonical', 'm', 'p', 'goose', 3),
		       ('dup:copy', 'm', 'p', 'augure-desktop', 3);
		INSERT INTO duplicate_group_members
			(session_id, group_key, role, canonical_id, member_count)
		VALUES
			('dup:canonical', 'g1', 'canonical', '', 2),
			('dup:copy', 'g1', 'duplicate', 'dup:canonical', 2);
	`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = store.DB().Exec(`
			DELETE FROM duplicate_group_members
			WHERE session_id IN ('dup:canonical', 'dup:copy');
			DELETE FROM sessions
			WHERE id IN ('dup:canonical', 'dup:copy');
		`)
	})

	page, err := store.ListSessions(ctx, db.SessionFilter{})
	require.NoError(t, err, "ListSessions")
	byID := map[string]db.Session{}
	for _, sess := range page.Sessions {
		byID[sess.ID] = sess
	}
	canonical, hasCanonical := byID["dup:canonical"]
	duplicate, hasDuplicate := byID["dup:copy"]
	require.True(t, hasCanonical && hasDuplicate,
		"seeded sessions are listed")
	assert.Equal(t, "canonical", canonical.DuplicateRole)
	assert.Equal(t, 2, canonical.DuplicateMemberCount)
	assert.Equal(t, "duplicate", duplicate.DuplicateRole)
	assert.Equal(t, "dup:canonical", duplicate.DuplicateCanonicalID)
	assert.Equal(t, 2, duplicate.DuplicateMemberCount)

	sess, err := store.GetSession(ctx, "dup:copy")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, sess)
	assert.Equal(t, "duplicate", sess.DuplicateRole)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{})
	require.NoError(t, err, "GetSidebarSessionIndex")
	var sidebarDuplicate *db.SidebarSessionIndexRow
	for i := range index.Sessions {
		if index.Sessions[i].ID == "dup:copy" {
			sidebarDuplicate = &index.Sessions[i]
		}
	}
	require.NotNil(t, sidebarDuplicate, "seeded session appears in sidebar")
	assert.Equal(t, "duplicate", sidebarDuplicate.DuplicateRole)
	assert.Equal(t, 2, sidebarDuplicate.DuplicateMemberCount)
}
