//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestListSessions_HasSecret verifies that the HasSecret filter
// returns only sessions where secret_leak_count > 0.
func TestListSessions_HasSecret(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	pg := store.DB()

	// Seed a session with leaks and one without.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count, secret_leak_count)
		VALUES
			('has-secret-leaky', 'test-machine', 'test-project',
			 'claude-code', 'secret session',
			 '2026-03-12T09:00:00Z'::timestamptz,
			 '2026-03-12T09:30:00Z'::timestamptz,
			 2, 1, 3),
			('has-secret-clean', 'test-machine', 'test-project',
			 'claude-code', 'clean session',
			 '2026-03-12T08:00:00Z'::timestamptz,
			 '2026-03-12T08:30:00Z'::timestamptz,
			 2, 1, 0)
	`)
	require.NoError(t, err, "inserting test sessions")

	ctx := context.Background()
	page, err := store.ListSessions(ctx, db.SessionFilter{
		HasSecret: true,
		Limit:     50,
	})
	require.NoError(t, err, "ListSessions")

	// Only the leaky session should appear.
	for _, s := range page.Sessions {
		assert.NotEqual(t, "has-secret-clean", s.ID,
			"clean session (secret_leak_count=0) included in HasSecret results")
	}

	var found *db.Session
	for i := range page.Sessions {
		if page.Sessions[i].ID == "has-secret-leaky" {
			found = &page.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "leaky session not found in HasSecret results")
	assert.Equal(t, 3, found.SecretLeakCount)

	_, err = pg.Exec(`
		UPDATE sessions
		SET secrets_rules_version = 'v-current'
		WHERE id = 'has-secret-leaky';
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count, secret_leak_count, secrets_rules_version)
		VALUES
			('has-secret-stale', 'test-machine', 'test-project',
			 'claude-code', 'stale secret session',
			 '2026-03-12T07:00:00Z'::timestamptz,
			 '2026-03-12T07:30:00Z'::timestamptz,
			 2, 1, 2, 'old-rules')
	`)
	require.NoError(t, err, "seeding stale secret session")
	current, err := store.ListSessions(ctx, db.SessionFilter{
		HasSecret:            true,
		SecretsRulesVersions: []string{"v-current"},
		Limit:                50,
	})
	require.NoError(t, err, "ListSessions current rules")
	for _, s := range current.Sessions {
		require.NotEqual(t, "has-secret-stale", s.ID,
			"stale secret session included in versioned HasSecret results")
	}
}

// TestListSessions_Sort verifies a non-default sort and its keyset cursor render
// correctly on PostgreSQL (numbered placeholders, ::bigint cast). Scoped to a
// unique project so the schema's seed session does not interfere.
func TestListSessions_Sort(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count, user_message_count)
		VALUES
			('sort-a', 'm', 'sort-test', 'claude', 'a',
			 '2026-03-01T00:00:00Z'::timestamptz, '2026-03-01T00:10:00Z'::timestamptz, 3, 1),
			('sort-b', 'm', 'sort-test', 'claude', 'b',
			 '2026-03-02T00:00:00Z'::timestamptz, '2026-03-02T00:10:00Z'::timestamptz, 9, 1),
			('sort-c', 'm', 'sort-test', 'claude', 'c',
			 '2026-03-03T00:00:00Z'::timestamptz, '2026-03-03T00:10:00Z'::timestamptz, 6, 1)
	`)
	require.NoError(t, err, "seeding sort sessions")

	ctx := context.Background()
	ids := func(sessions []db.Session) []string {
		out := make([]string, len(sessions))
		for i, s := range sessions {
			out[i] = s.ID
		}
		return out
	}

	// messages ascending (the default for non-recent sorts).
	asc, err := store.ListSessions(ctx, db.SessionFilter{
		Project: "sort-test", OrderBy: "messages", Limit: 10,
	})
	require.NoError(t, err, "ListSessions sort asc")
	require.Equal(t, []string{"sort-a", "sort-c", "sort-b"}, ids(asc.Sessions))

	// Paginated walk one row at a time exercises the ::bigint keyset cursor.
	var walked []string
	cursor := ""
	for {
		page, err := store.ListSessions(ctx, db.SessionFilter{
			Project: "sort-test", OrderBy: "messages", Limit: 1, Cursor: cursor,
		})
		require.NoError(t, err, "ListSessions sort page")
		walked = append(walked, ids(page.Sessions)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Equal(t, []string{"sort-a", "sort-c", "sort-b"}, walked)
}
