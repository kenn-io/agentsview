//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreSearchILIKE(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
	})
	require.NoError(t, err, "Search")
	assert.NotEmpty(t, page.Results, "expected at least 1 search result")
	for _, r := range page.Results {
		assert.NotEmpty(t, r.Agent, "Agent field is empty")
		assert.NotEmpty(t, r.SessionEndedAt, "SessionEndedAt is empty")
	}
}

func TestPGSearchDeduplication(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	// store-test-001 has 2 messages; searching "hello" only matches ordinal 0.
	// With session grouping, should return exactly 1 result.
	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
	})
	require.NoError(t, err, "Search")
	assert.Len(t, page.Results, 1, "deduplicated to session")
}

func TestPGSearchRecencySort(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	// Open a write connection to insert additional test data.
	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert a newer session that also matches "hello".
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('recency-test-002', 'test-machine',
			 'test-project', 'codex',
			 'hello again',
			 '2026-04-01T10:00:00Z'::timestamptz,
			 '2026-04-01T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting newer session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('recency-test-002', 0, 'user',
			 'hello again newer',
			 '2026-04-01T10:00:00Z'::timestamptz, 17)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting newer message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 10,
		Sort:  "recency",
	})
	require.NoError(t, err, "Search")
	require.GreaterOrEqual(t, len(page.Results), 2)
	// recency-test-002 has ended_at 2026-04-01, store-test-001 has 2026-03-12
	assert.Equal(t, "recency-test-002", page.Results[0].SessionID)
}

func TestPGSearchRelevanceSort(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert two sessions:
	// - relevance-early: match appears at position 1 (start of content)
	// - relevance-late: match appears after 50 chars of prefix
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('relevance-early', 'test-machine',
			 'test-project', 'claude',
			 'needle at start',
			 '2025-01-01T10:00:00Z'::timestamptz,
			 '2025-01-01T10:30:00Z'::timestamptz,
			 1, 1),
			('relevance-late', 'test-machine',
			 'test-project', 'claude',
			 'lots of text before needle',
			 '2025-01-02T10:00:00Z'::timestamptz,
			 '2025-01-02T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting sessions")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('relevance-early', 0, 'user',
			 'needleunique at the very beginning of content',
			 '2025-01-01T10:00:00Z'::timestamptz, 45),
			('relevance-late', 0, 'user',
			 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaneedleunique at the end',
			 '2025-01-02T10:00:00Z'::timestamptz, 73)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting messages")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "needleunique",
		Limit: 10,
		Sort:  "relevance",
	})
	require.NoError(t, err, "Search")
	require.GreaterOrEqual(t, len(page.Results), 2)
	// relevance-early has match at position 1; relevance-late has it after 50 chars
	// relevance sort = match_pos ASC, so relevance-early must come first
	assert.Equal(t, "relevance-early", page.Results[0].SessionID)
}

func TestPGSearchNullTimestampSorting(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert a session with NULL ended_at and started_at.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 message_count, user_message_count)
		VALUES
			('null-ts-001', 'test-machine',
			 'test-project', 'claude',
			 'nullsort keyword here',
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting null-ts session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('null-ts-001', 0, 'user',
			 'nullsort keyword here',
			 '2026-01-01T00:00:00Z'::timestamptz, 21)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting null-ts message")

	// Insert a session with real timestamps.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('null-ts-002', 'test-machine',
			 'test-project', 'claude',
			 'nullsort keyword here',
			 '2026-03-10T10:00:00Z'::timestamptz,
			 '2026-03-10T10:30:00Z'::timestamptz,
			 1, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting real-ts session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('null-ts-002', 0, 'user',
			 'nullsort keyword here',
			 '2026-03-10T10:00:00Z'::timestamptz, 21)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting real-ts message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()

	// Test recency sort: NULL-timestamp session must not be first.
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "nullsort",
		Limit: 10,
		Sort:  "recency",
	})
	require.NoError(t, err, "recency search")
	require.GreaterOrEqual(t, len(page.Results), 2)
	assert.NotEqual(t, "null-ts-001", page.Results[0].SessionID,
		"recency: NULL-timestamp session appeared first, want last")

	// Test relevance sort: both have same match_pos, so
	// session_ended_at DESC is the tie-breaker — NULL must not win.
	page, err = store.Search(ctx, db.SearchFilter{
		Query: "nullsort",
		Limit: 10,
		Sort:  "relevance",
	})
	require.NoError(t, err, "relevance search")
	require.GreaterOrEqual(t, len(page.Results), 2)
	assert.NotEqual(t, "null-ts-001", page.Results[0].SessionID,
		"relevance: NULL-timestamp session appeared first, want last")
}

func TestPGSearchSystemMessageExcluded(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert a session whose only matching message is a system message.
	// first_message intentionally does NOT contain the search term so the
	// name branch does not accidentally surface this session.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('sysonly-session', 'test-machine',
			 'test-project', 'claude',
			 'hello world',
			 '2026-03-01T10:00:00Z'::timestamptz,
			 '2026-03-01T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('sysonly-session', 0, 'user',
			 'sysonly unique term',
			 '2026-03-01T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting system message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "sysonly unique",
		Limit: 10,
	})
	require.NoError(t, err, "Search")
	assert.Empty(t, page.Results,
		"system-only session should not appear in search results")
}

// TestPGSearchNameBranchExcludesSystemOnlySessions verifies that a
// session whose display_name or first_message matches the search query
// does not appear in global search results when all its messages are
// system messages (the EXISTS guard in the name branch).
func TestPGSearchNameBranchExcludesSystemOnlySessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Session with only display_name matching, system messages only.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 display_name, started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('name-sysonly-dn', 'test-machine',
			 'test-project', 'claude',
			 'no match here',
			 'pgdnguardterm display',
			 '2026-03-10T10:00:00Z'::timestamptz,
			 '2026-03-10T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting dn session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('name-sysonly-dn', 0, 'user',
			 'irrelevant content',
			 '2026-03-10T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting dn system message")

	// Session with only first_message matching, system messages only.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent,
			 first_message, started_at, ended_at,
			 message_count, user_message_count)
		VALUES
			('name-sysonly-fm', 'test-machine',
			 'test-project', 'claude',
			 'pgfmguardterm first msg',
			 '2026-03-11T10:00:00Z'::timestamptz,
			 '2026-03-11T10:30:00Z'::timestamptz,
			 1, 0)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting fm session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('name-sysonly-fm', 0, 'user',
			 'irrelevant content',
			 '2026-03-11T10:00:00Z'::timestamptz, 19, TRUE)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting fm system message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()

	t.Run("display_name path", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{
			Query: "pgdnguardterm",
			Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results,
			"system-only session via display_name should not appear")
	})

	t.Run("first_message path", func(t *testing.T) {
		page, err := store.Search(ctx, db.SearchFilter{
			Query: "pgfmguardterm",
			Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results,
			"system-only session via first_message should not appear")
	})
}

// TestPGSearchSessionExcludesSystemMessages verifies that SearchSession
// (the in-session Cmd+F find-bar) excludes system messages since the
// frontend hides them and matching would produce phantom highlights.
func TestPGSearchSessionExcludesSystemMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert a session with one regular and one system message, both
	// containing the search term "syssearch".
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('sess-syssearch', 'test-machine',
			 'test-project', 'claude',
			 'syssearch regular',
			 NOW(), 2, 1)
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "inserting session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, is_system)
		VALUES
			('sess-syssearch', 0, 'user',
			 'syssearch regular content',
			 NOW(), 25, FALSE),
			('sess-syssearch', 1, 'assistant',
			 'syssearch system-only content',
			 NOW(), 29, TRUE)
		ON CONFLICT DO NOTHING
	`)
	require.NoError(t, err, "inserting messages")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	ctx := context.Background()
	ordinals, err := store.SearchSession(ctx, "sess-syssearch", "syssearch")
	require.NoError(t, err, "SearchSession")
	require.Len(t, ordinals, 1,
		"session search excludes system messages: %v", ordinals)
	assert.Equal(t, 0, ordinals[0])
}

func TestPGSearchSessionNameMatch(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Insert session with display_name containing unique search term,
	// no messages match.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message, display_name,
			 started_at, message_count, user_message_count)
		VALUES
			('name-match-001', 'test-machine', 'test-project', 'claude',
			 'first msg text', 'uniquedisplayterm session',
			 '2026-03-15T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('name-match-001', 0, 'user', 'no match here',
			 '2026-03-15T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "uniquedisplayterm", Limit: 10,
	})
	require.NoError(t, err, "Search")
	require.Len(t, page.Results, 1)
	r := page.Results[0]
	assert.Equal(t, "name-match-001", r.SessionID)
	assert.Equal(t, -1, r.Ordinal, "name-only match")
	assert.NotEmpty(t, r.Name, "Name field is empty")
}

func TestPGSearchRecencyNameOnlyBeatsOlderContent(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// older-content-001: message matches "recencytestterm", older timestamp
	// newer-name-001: display_name matches "recencytestterm", newer timestamp
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('older-content-recency', 'test-machine', 'test-project', 'claude',
			 'first msg', '2026-01-01T10:00:00Z'::timestamptz, 1, 1),
			('newer-name-recency', 'test-machine', 'test-project', 'claude',
			 'first msg', '2026-01-02T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert sessions")
	_, err = pg.Exec(`
		UPDATE sessions SET display_name = 'recencytestterm session'
		WHERE id = 'newer-name-recency'`)
	require.NoError(t, err, "set display_name")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('older-content-recency', 0, 'user', 'recencytestterm content',
			 '2026-01-01T10:00:00Z'::timestamptz, 22),
			('newer-name-recency', 0, 'user', 'no match here',
			 '2026-01-02T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "recencytestterm", Limit: 10, Sort: "recency",
	})
	require.NoError(t, err, "Search")
	require.GreaterOrEqual(t, len(page.Results), 2)
	// Recency mode: newer session (name-only) must appear before older content match.
	assert.Equal(t, "newer-name-recency", page.Results[0].SessionID,
		"name-only but newer should win")
}

// TestPGSearchSnippetMatchingField verifies that when a session has a
// display_name set but the search term only matches first_message, the
// snippet returned is the first_message (the matching field), not the
// display_name.
func TestPGSearchSnippetMatchingField(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	// Session: display_name is set to something unrelated; only first_message
	// contains the search term.
	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message, display_name,
			 started_at, message_count, user_message_count)
		VALUES
			('snippet-field-001', 'test-machine', 'test-project', 'claude',
			 'snippetfieldterm in first message', 'unrelated display name',
			 '2026-03-16T10:00:00Z'::timestamptz, 1, 1)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert session")
	// Message that does NOT contain the search term.
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('snippet-field-001', 0, 'user', 'no match here',
			 '2026-03-16T10:00:00Z'::timestamptz, 13)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert message")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: "snippetfieldterm", Limit: 10,
	})
	require.NoError(t, err, "Search")
	require.Len(t, page.Results, 1)
	r := page.Results[0]
	assert.Equal(t, "snippet-field-001", r.SessionID)
	assert.Equal(t, -1, r.Ordinal, "name-only match")
	// Snippet must be first_message (the matching field), not display_name.
	assert.Equal(t, "snippetfieldterm in first message", r.Snippet)
}

// TestGetMessagesIsSystemField verifies that GetMessages and GetAllMessages
// correctly populate db.Message.IsSystem from the is_system column.
func TestGetMessagesIsSystemField(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_is_system_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('is-system-001', 'test-machine', 'test-project', 'claude',
			 'hello', '2026-03-16T10:00:00Z'::timestamptz, 2, 1)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length, is_system)
		VALUES
			('is-system-001', 0, 'user', 'normal message',
			 '2026-03-16T10:00:00Z'::timestamptz, 14, FALSE),
			('is-system-001', 1, 'user', 'system message',
			 '2026-03-16T10:00:01Z'::timestamptz, 14, TRUE)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	// GetMessages
	msgs, err := store.GetMessages(ctx, "is-system-001", 0, 10, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 2)
	assert.False(t, msgs[0].IsSystem)
	assert.True(t, msgs[1].IsSystem)

	// GetAllMessages
	all, err := store.GetAllMessages(ctx, "is-system-001")
	require.NoError(t, err, "GetAllMessages")
	require.Len(t, all, 2)
	assert.False(t, all[0].IsSystem)
	assert.True(t, all[1].IsSystem)
}

// TestGetMessagesIDPopulated regresses #439: scanPGMessages must
// populate db.Message.ID with a unique-within-session value that
// matches int64(ordinal). The frontend keys {#each messages
// (message.id)} on it, and joins with TurnRow.MessageID via
// turnByMessage.get(message.id) — so collisions or zero-fill
// crash the message panel with each_key_duplicate. PG has no id
// column (composite PK on session_id, ordinal), so the synthetic
// ID matches the existing convention used by TurnRow.MessageID
// and CallRow.MessageID in session_timing.go.
func TestGetMessagesIDPopulated(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_msg_id_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('msg-id-001', 'test-machine', 'test-project', 'claude',
			 'hello', '2026-03-16T10:00:00Z'::timestamptz, 4, 2)`)
	require.NoError(t, err, "insert session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length, has_tool_use)
		VALUES
			('msg-id-001', 0, 'user', 'first',
			 '2026-03-16T10:00:00Z'::timestamptz, 5, FALSE),
			('msg-id-001', 1, 'assistant', 'second',
			 '2026-03-16T10:00:01Z'::timestamptz, 6, TRUE),
			('msg-id-001', 2, 'user', 'third',
			 '2026-03-16T10:00:02Z'::timestamptz, 5, FALSE),
			('msg-id-001', 3, 'assistant', 'fourth',
			 '2026-03-16T10:00:03Z'::timestamptz, 6, TRUE)`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	check := func(t *testing.T, label string, msgs []db.Message) {
		t.Helper()
		require.Len(t, msgs, 4, label)
		seen := map[int64]int{}
		for i, m := range msgs {
			prev, ok := seen[m.ID]
			assert.False(t, ok,
				"%s: msgs[%d].ID == msgs[%d].ID == %d (regression of #439)",
				label, prev, i, m.ID)
			seen[m.ID] = i
			assert.Equal(t, int64(m.Ordinal), m.ID,
				"%s: msgs[%d].ID = %d, want int64(ordinal=%d)",
				label, i, m.ID, m.Ordinal)
		}
	}

	msgs, err := store.GetMessages(ctx, "msg-id-001", 0, 100, true)
	require.NoError(t, err, "GetMessages")
	check(t, "GetMessages", msgs)

	all, err := store.GetAllMessages(ctx, "msg-id-001")
	require.NoError(t, err, "GetAllMessages")
	check(t, "GetAllMessages", all)

	// Cross-check: every TurnRow.MessageID must correspond to a message
	// returned by GetMessages with the same ID. The frontend looks up
	// turns via turnByMessage.get(message.id); messages without a turn
	// are allowed (lookup returns undefined), but a turn whose
	// MessageID has no matching message means the join will silently
	// drop timing data.
	timing, err := store.GetSessionTiming(ctx, "msg-id-001")
	require.NoError(t, err, "GetSessionTiming")
	require.NotNil(t, timing, "GetSessionTiming returned nil")
	require.NotEmpty(t, timing.Turns, "GetSessionTiming.Turns empty")
	msgIDs := map[int64]bool{}
	for _, m := range msgs {
		msgIDs[m.ID] = true
	}
	for _, turn := range timing.Turns {
		assert.True(t, msgIDs[turn.MessageID],
			"turn.MessageID=%d has no matching message.ID; "+
				"frontend turnByMessage join will drop this turn",
			turn.MessageID)
	}
}

// TestPGSearchOperatorTokenNoError mirrors the SQLite FTS 500 regression on the
// PostgreSQL/ILIKE backend: a single token containing operator characters
// (hyphen, colon), prepared the way the HTTP handler does, must match content
// and not error. ILIKE has no FTS-operator hazard, but this pins parity.
func TestPGSearchOperatorTokenNoError(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('optok-001', 'test-machine', 'test-project', 'claude',
			 'first msg text',
			 '2026-03-20T10:00:00Z'::timestamptz, 2, 2)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert session")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('optok-001', 0, 'user', 'hit error-401 from the api',
			 '2026-03-20T10:00:00Z'::timestamptz, 26),
			('optok-001', 1, 'assistant', 'returned status:500 to client',
			 '2026-03-20T10:00:01Z'::timestamptz, 30)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	for _, raw := range []string{"error-401", "status:500"} {
		page, err := store.Search(context.Background(), db.SearchFilter{
			Query: db.PrepareFTSQuery(raw), Limit: 10,
		})
		require.NoError(t, err, "Search(%q)", raw)
		require.Len(t, page.Results, 1, "results for %q", raw)
		assert.Equal(t, "optok-001", page.Results[0].SessionID, "session for %q", raw)
	}
}

// TestPGSearchMultiTermAND verifies that a multi-term query matches a session
// only when every term appears in its content (AND), matching SQLite FTS5's
// implicit AND so the same user query behaves identically across backends.
func TestPGSearchMultiTermAND(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	pg, err := Open(pgURL, testSchema, false)
	require.NoError(t, err, "Open")
	defer pg.Close()

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, message_count, user_message_count)
		VALUES
			('andboth-001', 'test-machine', 'test-project', 'claude',
			 'first msg text',
			 '2026-03-21T10:00:00Z'::timestamptz, 2, 2),
			('andone-001', 'test-machine', 'test-project', 'claude',
			 'first msg text',
			 '2026-03-21T11:00:00Z'::timestamptz, 2, 2)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "insert sessions")
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('andboth-001', 0, 'user', 'pgquickterm and pgfoxterm both here',
			 '2026-03-21T10:00:00Z'::timestamptz, 35),
			('andone-001', 0, 'user', 'only pgquickterm present',
			 '2026-03-21T11:00:00Z'::timestamptz, 24)
		ON CONFLICT DO NOTHING`)
	require.NoError(t, err, "insert messages")

	store, err := NewStore(pgURL, testSchema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	page, err := store.Search(context.Background(), db.SearchFilter{
		Query: db.PrepareFTSQuery("pgquickterm pgfoxterm"), Limit: 10,
	})
	require.NoError(t, err, "Search")
	require.Len(t, page.Results, 1, "only the session containing both terms")
	assert.Equal(t, "andboth-001", page.Results[0].SessionID, "session")
}
