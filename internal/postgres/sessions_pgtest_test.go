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

// TestListSessions_SortMultiKey verifies a mixed-direction multi-key sort on
// PostgreSQL, where the lexicographic OR-expansion places ::bigint and
// ::timestamptz casts on the equality and comparison clauses. The paginated walk
// must match the full listing.
func TestListSessions_SortMultiKey(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('mk-a', 'm', 'mk', 'claude', 'a', '2024-03-01T00:00:00Z'::timestamptz, 1, 2),
			('mk-b', 'm', 'mk', 'claude', 'b', '2024-01-01T00:00:00Z'::timestamptz, 1, 2),
			('mk-c', 'm', 'mk', 'claude', 'c', '2024-02-01T00:00:00Z'::timestamptz, 2, 2),
			('mk-d', 'm', 'mk', 'claude', 'd', '2024-05-01T00:00:00Z'::timestamptz, 2, 2),
			('mk-e', 'm', 'mk', 'claude', 'e', '2024-03-01T00:00:00Z'::timestamptz, 1, 2)
	`)
	require.NoError(t, err, "seeding multi-key sessions")

	ctx := context.Background()
	ids := func(sessions []db.Session) []string {
		out := make([]string, len(sessions))
		for i, s := range sessions {
			out[i] = s.ID
		}
		return out
	}
	asc, desc := false, true
	sortKeys := []db.SortKey{
		{Key: "messages", Descending: &asc},
		{Key: "started", Descending: &desc},
	}

	full, err := store.ListSessions(ctx, db.SessionFilter{
		Project: "mk", Sort: sortKeys, Limit: 100,
	})
	require.NoError(t, err, "ListSessions multi-key")
	want := ids(full.Sessions)
	require.Equal(t, []string{"mk-e", "mk-a", "mk-b", "mk-d", "mk-c"}, want)

	var walked []string
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := store.ListSessions(ctx, db.SessionFilter{
			Project: "mk", Sort: sortKeys, Limit: 1, Cursor: cursor,
		})
		require.NoError(t, err, "ListSessions multi-key page")
		for _, s := range page.Sessions {
			require.False(t, seen[s.ID], "duplicate %s", s.ID)
			seen[s.ID] = true
			walked = append(walked, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Equal(t, want, walked, "paginated walk matches full listing")
}

// TestListSessions_SortSecretsVersioned verifies the version-gated secrets sort
// renders on PostgreSQL, where the gating CASE places numbered placeholders
// inside both ORDER BY and the keyset cursor predicate.
func TestListSessions_SortSecretsVersioned(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count, user_message_count,
			 secret_leak_count, secrets_rules_version)
		VALUES
			('sec-cur', 'm', 'sec-test', 'claude', 'a',
			 '2026-03-01T00:00:00Z'::timestamptz, '2026-03-01T00:10:00Z'::timestamptz, 2, 1, 5, 'v1'),
			('sec-stale', 'm', 'sec-test', 'claude', 'b',
			 '2026-03-02T00:00:00Z'::timestamptz, '2026-03-02T00:10:00Z'::timestamptz, 2, 1, 9, 'old'),
			('sec-none', 'm', 'sec-test', 'claude', 'c',
			 '2026-03-03T00:00:00Z'::timestamptz, '2026-03-03T00:10:00Z'::timestamptz, 2, 1, 0, '')
	`)
	require.NoError(t, err, "seeding secret sessions")

	ctx := context.Background()
	ids := func(sessions []db.Session) []string {
		out := make([]string, len(sessions))
		for i, s := range sessions {
			out[i] = s.ID
		}
		return out
	}

	// With v1 active, the stale 9 gates to 0, so the current-version 5 leads.
	gated, err := store.ListSessions(ctx, db.SessionFilter{
		Project:              "sec-test",
		OrderBy:              "secrets",
		Descending:           new(true),
		SecretsRulesVersions: []string{"v1"},
		Limit:                10,
	})
	require.NoError(t, err, "ListSessions secrets gated")
	require.Equal(t, "sec-cur", ids(gated.Sessions)[0])

	// Paginate one at a time so the gating CASE also renders in the cursor
	// predicate; the current-version session must still lead with no dupes.
	var walked []string
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := store.ListSessions(ctx, db.SessionFilter{
			Project:              "sec-test",
			OrderBy:              "secrets",
			Descending:           new(true),
			SecretsRulesVersions: []string{"v1"},
			Limit:                1,
			Cursor:               cursor,
		})
		require.NoError(t, err, "ListSessions secrets page")
		for _, s := range page.Sessions {
			require.False(t, seen[s.ID], "duplicate %s", s.ID)
			seen[s.ID] = true
			walked = append(walked, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, walked, 3)
	require.Equal(t, "sec-cur", walked[0])
}

// TestListSessions_SortNullsLast verifies a nullable sort (health) places NULL
// rows last on PostgreSQL and paginates across the sentinel boundary.
func TestListSessions_SortNullsLast(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count, user_message_count, health_score)
		VALUES
			('h20', 'm', 'null-test', 'claude', 'a',
			 '2026-03-01T00:00:00Z'::timestamptz, '2026-03-01T00:10:00Z'::timestamptz, 2, 1, 20),
			('h80', 'm', 'null-test', 'claude', 'b',
			 '2026-03-02T00:00:00Z'::timestamptz, '2026-03-02T00:10:00Z'::timestamptz, 2, 1, 80),
			('hnull', 'm', 'null-test', 'claude', 'c',
			 '2026-03-03T00:00:00Z'::timestamptz, '2026-03-03T00:10:00Z'::timestamptz, 2, 1, NULL)
	`)
	require.NoError(t, err, "seeding health sessions")

	ctx := context.Background()
	var walked []string
	cursor := ""
	for {
		page, err := store.ListSessions(ctx, db.SessionFilter{
			Project: "null-test", OrderBy: "health", Limit: 1, Cursor: cursor,
		})
		require.NoError(t, err, "ListSessions health page")
		for _, s := range page.Sessions {
			walked = append(walked, s.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	// Ascending with NULLs last, paginated across the sentinel boundary.
	require.Equal(t, []string{"h20", "h80", "hnull"}, walked)
}
