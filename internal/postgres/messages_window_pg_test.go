//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const mwTestSchema = "agentsview_messages_window_test"

func mwEnsureSchema(t *testing.T, pgURL string) *sql.DB {
	t.Helper()
	pg, err := Open(pgURL, mwTestSchema, true)
	require.NoError(t, err, "Open")
	ctx := context.Background()
	_, err = pg.ExecContext(ctx,
		`DROP SCHEMA IF EXISTS `+mwTestSchema+` CASCADE`,
	)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, mwTestSchema), "EnsureSchema")
	return pg
}

// mwSeedWindowMessages seeds a session with 12 messages (ordinals 0..11)
// with the same user/assistant/system role layout used by the SQLite/DuckDB
// GetMessagesWindow parity tests:
//
//	0 user, 1 assistant, 2 user, 3 assistant, 4 system, 5 user,
//	6 assistant, 7 user, 8 assistant, 9 system, 10 user, 11 assistant
func mwSeedWindowMessages(t *testing.T, pg *sql.DB, sessionID string) {
	t.Helper()
	_, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent,
			 message_count, user_message_count)
		VALUES ($1, 'test', 'proj', 'claude', 12, 8)
	`, sessionID)
	require.NoError(t, err, "insert session %s", sessionID)

	roles := []string{
		"user", "assistant", "user", "assistant", "system", "user",
		"assistant", "user", "assistant", "system", "user", "assistant",
	}
	for ordinal, role := range roles {
		_, err := pg.Exec(`
			INSERT INTO messages
				(session_id, ordinal, role, content,
				 content_length, is_system)
			VALUES ($1, $2, $3, 'msg', 3, $4)
		`, sessionID, ordinal, role, role == "system")
		require.NoError(t, err, "insert message %s/%d", sessionID, ordinal)
	}
}

func mwNewStore(t *testing.T, pgURL string) *Store {
	t.Helper()
	store, err := NewStore(pgURL, mwTestSchema, true)
	require.NoError(t, err, "NewStore")
	return store
}

func mwOrdinalsOf(msgs []db.Message) []int {
	out := make([]int, len(msgs))
	for i, m := range msgs {
		out[i] = m.Ordinal
	}
	return out
}

// TestPGGetMessagesWindow_AroundMidSession mirrors
// TestGetMessagesWindow_AroundMidSession.
func TestPGGetMessagesWindow_AroundMidSession(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sMid")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	anchor := 6
	msgs, err := store.GetMessagesWindow(ctx, "sMid", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{4, 5, 6, 7, 8}, mwOrdinalsOf(msgs),
		"unfiltered window should return anchor +/- 2 ordinals ascending")
}

// TestPGGetMessagesWindow_RoleFilterCountsFilteredMessages mirrors
// TestGetMessagesWindow_RoleFilterCountsFilteredMessages.
func TestPGGetMessagesWindow_RoleFilterCountsFilteredMessages(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sRoleCount")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	anchor := 6
	msgs, err := store.GetMessagesWindow(ctx, "sRoleCount", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{3, 5, 6, 7, 8}, mwOrdinalsOf(msgs),
		"before/after counts should count role-filtered messages, not raw ordinals")
}

// TestPGGetMessagesWindow_AnchorIncludedEvenWhenRoleFiltered mirrors
// TestGetMessagesWindow_AnchorIncludedEvenWhenRoleFiltered.
func TestPGGetMessagesWindow_AnchorIncludedEvenWhenRoleFiltered(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sAnchorFiltered")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	anchor := 4 // role "system", excluded by the role filter
	msgs, err := store.GetMessagesWindow(ctx, "sAnchorFiltered", db.MessageWindow{
		Around: &anchor, Before: 1, After: 1,
		Roles: []string{"user", "assistant"},
	})
	require.NoError(t, err)
	require.Equal(t, []int{3, 4, 5}, mwOrdinalsOf(msgs),
		"anchor must be included even though its own role is filtered out")
	assert.Equal(t, "system", msgs[1].Role)
}

// TestPGGetMessagesWindow_AroundOrdinalZeroHasNoBefore mirrors
// TestGetMessagesWindow_AroundOrdinalZeroHasNoBefore.
func TestPGGetMessagesWindow_AroundOrdinalZeroHasNoBefore(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sFirst")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	anchor := 0
	msgs, err := store.GetMessagesWindow(ctx, "sFirst", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, mwOrdinalsOf(msgs),
		"no before rows exist above the first ordinal")
}

// TestPGGetMessagesWindow_AroundLastOrdinalHasNoAfter mirrors
// TestGetMessagesWindow_AroundLastOrdinalHasNoAfter.
func TestPGGetMessagesWindow_AroundLastOrdinalHasNoAfter(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sLast")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	anchor := 11
	msgs, err := store.GetMessagesWindow(ctx, "sLast", db.MessageWindow{
		Around: &anchor, Before: 2, After: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{9, 10, 11}, mwOrdinalsOf(msgs),
		"no after rows exist below the last ordinal")
}

// TestPGGetMessagesWindow_LinearModeWithRoles mirrors
// TestGetMessagesWindow_LinearModeWithRoles.
func TestPGGetMessagesWindow_LinearModeWithRoles(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sLinearRoles")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	msgs, err := store.GetMessagesWindow(ctx, "sLinearRoles", db.MessageWindow{
		Limit: 100, Asc: true, Roles: []string{"user"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2, 5, 7, 10}, mwOrdinalsOf(msgs),
		"linear mode should apply the role filter like the around mode")
}

// TestPGGetMessagesWindow_EmptyRolesEquivalentToGetMessages mirrors
// TestGetMessagesWindow_EmptyRolesEquivalentToGetMessages.
func TestPGGetMessagesWindow_EmptyRolesEquivalentToGetMessages(t *testing.T) {
	pgURL := testPGURL(t)
	pg := mwEnsureSchema(t, pgURL)
	defer pg.Close()
	defer func() {
		_, _ = pg.Exec(`DROP SCHEMA IF EXISTS ` + mwTestSchema + ` CASCADE`)
	}()
	ctx := context.Background()

	mwSeedWindowMessages(t, pg, "sEquiv")
	store := mwNewStore(t, pgURL)
	defer store.Close()

	direct, err := store.GetMessages(ctx, "sEquiv", 3, 5, true)
	require.NoError(t, err)

	from := 3
	windowed, err := store.GetMessagesWindow(ctx, "sEquiv", db.MessageWindow{
		From: &from, Limit: 5, Asc: true,
	})
	require.NoError(t, err)
	assert.Equal(t, direct, windowed,
		"empty Roles should behave identically to GetMessages")
}
