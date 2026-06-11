//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// contentSearchSchema is an isolated schema for content-search tests so
// they don't interfere with other pgtest suites that reuse testSchema.
const contentSearchSchema = "agentsview_content_search_test"

// setupContentSearch creates a fresh schema and returns a *Store pointing
// at it plus a raw *sql.DB for direct inserts.
func setupContentSearch(t *testing.T) *Store {
	t.Helper()
	pgURL := testPGURL(t)

	pg, err := Open(pgURL, contentSearchSchema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + contentSearchSchema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, contentSearchSchema), "EnsureSchema")

	store, err := NewStore(pgURL, contentSearchSchema, true)
	require.NoError(t, err, "NewStore")
	t.Cleanup(func() { store.Close() })
	return store
}

// insertCSSession inserts a session into the content-search schema via the
// store's raw DB handle. Sessions need user_message_count > 1 so that the
// one-shot exclusion does not hide them from the default filter.
func insertCSSession(
	t *testing.T, store *Store,
	id, project, agent, startedAt, endedAt string,
) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count, user_message_count)
		VALUES ($1, 'test-machine', $2, $3, 'seed',
			$4::timestamptz, $5::timestamptz, 10, 5)
		ON CONFLICT (id) DO NOTHING`,
		id, project, agent, startedAt, endedAt,
	)
	require.NoError(t, err, "insert session %s", id)
}

// insertCSMessage inserts a message; isSystem=true sets is_system.
func insertCSMessage(
	t *testing.T, store *Store,
	sessionID string, ordinal int, role, content, ts string, isSystem bool,
) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content, timestamp,
			 content_length, is_system)
		VALUES ($1, $2, $3, $4, $5::timestamptz, $6, $7)
		ON CONFLICT DO NOTHING`,
		sessionID, ordinal, role, content, ts, len(content), isSystem,
	)
	require.NoError(t, err, "insert message ord=%d", ordinal)
}

// insertCSToolCall inserts a tool_call row.
func insertCSToolCall(
	t *testing.T, store *Store,
	sessionID string, messageOrdinal, callIndex int,
	toolName, toolUseID, inputJSON, resultContent string,
) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO tool_calls
			(session_id, message_ordinal, call_index, tool_name,
			 category, tool_use_id, input_json, result_content)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING`,
		sessionID, messageOrdinal, callIndex, toolName,
		toolName, toolUseID, inputJSON, resultContent,
	)
	require.NoError(t, err, "insert tool_call")
}

// insertCSToolResultEvent inserts a tool_result_event row.
func insertCSToolResultEvent(
	t *testing.T, store *Store,
	sessionID string, messageOrdinal, callIndex, eventIndex int,
	toolUseID, content string,
) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, source, status, content, event_index)
		VALUES ($1, $2, $3, $4, 'stdout', 'ok', $5, $6)
		ON CONFLICT DO NOTHING`,
		sessionID, messageOrdinal, callIndex, toolUseID, content, eventIndex,
	)
	require.NoError(t, err, "insert tool_result_event")
}

// ---- tests ----

// TestPGSearchContentSubstringMessages verifies substring match in messages.
func TestPGSearchContentSubstringMessages(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-m1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-m1", 0, "user",
		"please find the DATABASE_URL value", "2026-05-01T10:00:00Z", false)
	insertCSMessage(t, store, "cs-m1", 1, "assistant",
		"no match here", "2026-05-01T10:00:01Z", false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "database_url", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
		IncludeOneShot: true,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches=%+v", got.Matches)
	m := got.Matches[0]
	assert.Equal(t, "cs-m1", m.SessionID)
	assert.Equal(t, "message", m.Location)
	assert.Equal(t, 0, m.Ordinal)
	assert.Equal(t, "user", m.Role)
	assert.NotEmpty(t, m.Snippet)
}

// TestPGSearchContentRedactsStraddlingSecret pins the PG default (non-reveal)
// guarantee: a secret adjacent to the match that extends past the snippet
// window must not leak; reveal opts out and shows raw bytes.
func TestPGSearchContentRedactsStraddlingSecret(t *testing.T) {
	store := setupContentSearch(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	insertCSSession(t, store, "cs-sec", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-sec", 0, "user",
		"deploy with this attached key "+pem+" ok", "2026-05-01T10:00:00Z", false)

	ctx := context.Background()
	base := db.ContentSearchFilter{
		Pattern: "attached key", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50, IncludeOneShot: true,
	}
	got, err := store.SearchContent(ctx, base)
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches=%+v", got.Matches)
	assert.NotContains(t, got.Matches[0].Snippet, "SECRETKEYMATERIAL",
		"default snippet leaked key material")

	base.RevealSecrets = true
	rev, err := store.SearchContent(ctx, base)
	require.NoError(t, err, "SearchContent reveal")
	assert.Contains(t, rev.Matches[0].Snippet, "SECRETKEYMATERIAL",
		"reveal snippet should show raw bytes")
}

// TestPGSearchContentSubstringToolInput verifies substring match in tool_input.
func TestPGSearchContentSubstringToolInput(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-ti1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-ti1", 0, "assistant",
		"running it", "2026-05-01T10:00:00Z", false)
	insertCSToolCall(t, store, "cs-ti1", 0, 0,
		"Bash", "tu1", `{"command":"printenv"}`, "output here")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "printenv", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches=%+v", got.Matches)
	m := got.Matches[0]
	assert.Equal(t, "tool_input", m.Location)
	assert.Equal(t, "Bash", m.ToolName)
}

// TestPGSearchContentSubstringToolResult verifies substring match in tool_result
// result_content (no events -> result_content branch).
func TestPGSearchContentSubstringToolResult(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-tr1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-tr1", 0, "assistant",
		"running it", "2026-05-01T10:00:00Z", false)
	insertCSToolCall(t, store, "cs-tr1", 0, 0,
		"Bash", "tu1", `{"command":"ls"}`, "AWS_SECRET=topsecretvalue123")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "topsecretvalue", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches=%+v", got.Matches)
	assert.Equal(t, "tool_result", got.Matches[0].Location)
}

// TestPGSearchContentToolResultEvents verifies that the tool_result_events
// branch is searched.
func TestPGSearchContentToolResultEvents(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-tre1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-tre1", 0, "assistant",
		"running it", "2026-05-01T10:00:00Z", false)
	// tool_call with no result_content but has a result event.
	insertCSToolCall(t, store, "cs-tre1", 0, 0,
		"Bash", "tu1", `{"command":"ls"}`, "")
	insertCSToolResultEvent(t, store, "cs-tre1", 0, 0, 0,
		"tu1", "EVENTNEEDLE in event output")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "EVENTNEEDLE", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches=%+v", got.Matches)
	assert.Equal(t, "tool_result", got.Matches[0].Location)
}

// TestPGSearchContentToolResultDedup verifies dedup: when a tool_use_id has
// result events, only the events branch matches, not result_content.
func TestPGSearchContentToolResultDedup(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-dup1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-dup1", 0, "assistant",
		"running it", "2026-05-01T10:00:00Z", false)
	// result_content also contains the needle — but the call has an event,
	// so the NOT EXISTS guard should suppress the result_content branch.
	insertCSToolCall(t, store, "cs-dup1", 0, 0,
		"Bash", "tu1", `{"command":"echo"}`,
		"DUPNEEDLE in result_content")
	insertCSToolResultEvent(t, store, "cs-dup1", 0, 0, 0,
		"tu1", "DUPNEEDLE in event")

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "DUPNEEDLE", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	// Should be exactly 1 match (from the event, not result_content).
	require.Len(t, got.Matches, 1, "dedup: matches=%+v", got.Matches)
}

// TestPGSearchContentSourcesSelector verifies that searching only "messages"
// does not return tool_input or tool_result hits.
func TestPGSearchContentSourcesSelector(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-src1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-src1", 0, "assistant",
		"running it", "2026-05-01T10:00:00Z", false)
	insertCSToolCall(t, store, "cs-src1", 0, 0,
		"Bash", "tu1", `{"command":"SRCNEEDLE"}`, "SRCNEEDLE in result")

	ctx := context.Background()
	// Only messages — must not match tool_input or tool_result.
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SRCNEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	assert.Empty(t, got.Matches,
		"messages-only source returned tool match")

	// Both tool sources — must match.
	all, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SRCNEEDLE", Mode: "substring",
		Sources: []string{"tool_input", "tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent all")
	assert.NotEmpty(t, all.Matches,
		"expected matches when sources include tool_input/tool_result")
}

// TestPGSearchContentExcludeSystem verifies that is_system=true messages are
// excluded when ExcludeSystem=true.
func TestPGSearchContentExcludeSystem(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-sys1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	// is_system = true
	insertCSMessage(t, store, "cs-sys1", 0, "user",
		"SYSNEEDLE in a system message", "2026-05-01T10:00:00Z", true)

	ctx := context.Background()
	// Default (ExcludeSystem=false) should find it.
	with, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SYSNEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent with system")
	assert.Len(t, with.Matches, 1)

	// ExcludeSystem=true should suppress it.
	without, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SYSNEEDLE", Mode: "substring",
		Sources: []string{"messages"}, ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent exclude system")
	assert.Empty(t, without.Matches,
		"ExcludeSystem=true should suppress system messages")
}

// TestPGSearchContentProjectFilter verifies the Project session filter.
func TestPGSearchContentProjectFilter(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-pf-a", "alpha", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-pf-a", 0, "user",
		"PROJNEEDLE alpha", "2026-05-01T10:00:00Z", false)
	insertCSSession(t, store, "cs-pf-b", "beta", "claude",
		"2026-05-01T11:00:00Z", "2026-05-01T11:30:00Z")
	insertCSMessage(t, store, "cs-pf-b", 0, "user",
		"PROJNEEDLE beta", "2026-05-01T11:00:00Z", false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "PROJNEEDLE", Mode: "substring",
		Sources: []string{"messages"},
		Project: "alpha",
		Limit:   50,
	})
	require.NoError(t, err, "SearchContent project filter")
	for _, m := range got.Matches {
		assert.Equal(t, "alpha", m.Project, "project filter leak")
	}
	assert.NotEmpty(t, got.Matches, "expected matches in alpha project")
}

// TestPGSearchContentPagination verifies Limit+1 sentinel and NextCursor.
func TestPGSearchContentPagination(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-page", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	for i := 0; i < 3; i++ {
		insertCSMessage(t, store, "cs-page", i, "user",
			"PAGENEEDLE msg", "2026-05-01T10:00:0"+string(rune('0'+i))+"Z", false)
	}

	ctx := context.Background()
	first, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "PAGENEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Limit: 2,
	})
	require.NoError(t, err, "SearchContent page1")
	require.Len(t, first.Matches, 2)
	require.NotZero(t, first.NextCursor, "page1: expected NextCursor to be set")

	second, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "PAGENEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Limit: 2, Cursor: first.NextCursor,
	})
	require.NoError(t, err, "SearchContent page2")
	require.Len(t, second.Matches, 1)
	assert.Zero(t, second.NextCursor, "page2: last page")
}

// TestPGSearchContentPaginationStableAcrossTies seeds one message ordinal that
// yields three hits tying on (session, ordinal): the message body, the tool
// input, and the tool result. The src/row_id tie-break must make page-by-page
// retrieval reproduce the single-page order with no duplicates or gaps.
func TestPGSearchContentPaginationStableAcrossTies(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-tie", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-tie", 0, "assistant",
		"FINDME in message body", "2026-05-01T10:00:00Z", false)
	insertCSToolCall(t, store, "cs-tie", 0, 0,
		"Bash", "tu1", `{"command":"FINDME"}`, "FINDME in result")

	ctx := context.Background()
	base := db.ContentSearchFilter{
		Pattern: "FINDME", Mode: "substring",
		Sources: []string{"messages", "tool_input", "tool_result"},
	}
	full := base
	full.Limit = 50
	all, err := store.SearchContent(ctx, full)
	require.NoError(t, err, "SearchContent full")
	require.Len(t, all.Matches, 3, "tied matches: %+v", all.Matches)
	wantOrder := []string{"message", "tool_input", "tool_result"}
	for i, loc := range wantOrder {
		assert.Equal(t, loc, all.Matches[i].Location, "match %d", i)
	}
	var paged []db.ContentMatch
	for cursor := 0; ; {
		p := base
		p.Limit = 1
		p.Cursor = cursor
		page, err := store.SearchContent(ctx, p)
		require.NoError(t, err, "SearchContent page at cursor %d", cursor)
		paged = append(paged, page.Matches...)
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, paged, len(all.Matches), "duplicates or gaps")
	for i := range all.Matches {
		assert.Equal(t, all.Matches[i].Location, paged[i].Location,
			"row %d: paged vs single-page", i)
	}
}

// TestPGSearchContentEmptyToolUseIDNotSuppressed mirrors the SQLite guard: an
// empty-tool_use_id call's result_content must not be suppressed because a
// different empty-ID call in the session has a result event.
func TestPGSearchContentEmptyToolUseIDNotSuppressed(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-empti", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-empti", 0, "assistant",
		"running tools", "2026-05-01T10:00:00Z", false)
	// Call 0: empty tool_use_id, result in result_content, no events.
	insertCSToolCall(t, store, "cs-empti", 0, 0,
		"Bash", "", `{"command":"a"}`, "FINDA in result")
	// Call 1: empty tool_use_id, result delivered as an event.
	insertCSToolCall(t, store, "cs-empti", 0, 1,
		"Bash", "", `{"command":"b"}`, "")
	insertCSToolResultEvent(t, store, "cs-empti", 0, 1, 0, "", "FINDB event")

	ctx := context.Background()
	for _, mode := range []string{"substring", "regex"} {
		got, err := store.SearchContent(ctx, db.ContentSearchFilter{
			Pattern: "FINDA", Mode: mode,
			Sources: []string{"tool_result"}, Limit: 50,
		})
		require.NoError(t, err, "SearchContent %s", mode)
		require.Len(t, got.Matches, 1,
			"%s: empty-ID result_content suppressed: %+v", mode, got.Matches)
		assert.Equal(t, "tool_result", got.Matches[0].Location)
	}
}

// TestPGContentSearchTrigramIndex verifies EnsureSchema installs the pg_trgm
// extension and the messages.content trigram index that keeps ILIKE content
// search off a sequential scan. Index creation is best-effort, so the check is
// skipped on an instance where pg_trgm cannot be installed.
func TestPGContentSearchTrigramIndex(t *testing.T) {
	store := setupContentSearch(t)
	ctx := context.Background()

	var hasExt bool
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`,
	).Scan(&hasExt), "query pg_extension")
	if !hasExt {
		t.Skip("pg_trgm not installable on this instance; index is best-effort")
	}

	var hasIdx bool
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1 AND indexname = 'idx_messages_content_trgm'
		)`, contentSearchSchema,
	).Scan(&hasIdx), "query pg_indexes")
	assert.True(t, hasIdx,
		"idx_messages_content_trgm missing after EnsureSchema")

	// fastupdate=off must be set so the pending list cannot grow
	// unbounded under continuous ingest. The schema bootstrap applies
	// this on every boot (including stores upgraded from an earlier
	// schema that created the index with the default fastupdate=on).
	var fastupdateOff bool
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1
			   AND c.relname = 'idx_messages_content_trgm'
			   AND 'fastupdate=off' = ANY(c.reloptions)
		)`, contentSearchSchema,
	).Scan(&fastupdateOff), "query pg_class.reloptions")
	assert.True(t, fastupdateOff,
		"idx_messages_content_trgm must have fastupdate=off")
}

// TestPGContentSearchTrigramIndexUpgrade verifies the upgrade path: a store
// whose trigram index was created by an earlier schema with the default
// fastupdate=on gets fastupdate=off reapplied on the next EnsureSchema run.
func TestPGContentSearchTrigramIndexUpgrade(t *testing.T) {
	store := setupContentSearch(t)
	ctx := context.Background()

	hasFastupdateOff := func() bool {
		var off bool
		require.NoError(t, store.DB().QueryRowContext(ctx,
			`SELECT EXISTS (
				SELECT 1
				  FROM pg_class c
				  JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = $1
				   AND c.relname = 'idx_messages_content_trgm'
				   AND 'fastupdate=off' = ANY(c.reloptions)
			)`, contentSearchSchema,
		).Scan(&off), "query pg_class.reloptions")
		return off
	}

	var hasIdx bool
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1 AND indexname = 'idx_messages_content_trgm'
		)`, contentSearchSchema,
	).Scan(&hasIdx), "query pg_indexes")
	if !hasIdx {
		t.Skip("trigram index not created on this instance; index is best-effort")
	}

	// Simulate an index left behind by the pre-#606 schema, which created
	// it with the PostgreSQL default fastupdate=on.
	_, err := store.DB().ExecContext(ctx,
		`ALTER INDEX idx_messages_content_trgm SET (fastupdate = on)`)
	require.NoError(t, err, "reset index to fastupdate=on")
	require.False(t, hasFastupdateOff(),
		"precondition: index should report fastupdate=on")

	require.NoError(t, EnsureSchema(ctx, store.DB(), contentSearchSchema),
		"rerun EnsureSchema")
	assert.True(t, hasFastupdateOff(),
		"EnsureSchema must reapply fastupdate=off to a pre-existing index")
}

// TestPGSearchContentRegex verifies regex mode.
func TestPGSearchContentRegex(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-re1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-re1", 0, "user",
		"key AKIA7QHWN2DKR4FYPLJM here", "2026-05-01T10:00:00Z", false)
	insertCSMessage(t, store, "cs-re1", 1, "user",
		"no secrets in this line", "2026-05-01T10:00:01Z", false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: `AKIA[0-9A-Z]{16}`, Mode: "regex",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent regex")
	require.Len(t, got.Matches, 1)
	assert.Equal(t, 0, got.Matches[0].Ordinal)
}

// TestPGSearchContentRegexInvalid verifies that an invalid pattern returns a
// SearchInputError.
func TestPGSearchContentRegexInvalid(t *testing.T) {
	store := setupContentSearch(t)
	ctx := context.Background()
	_, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: `(unclosed`, Mode: "regex",
		Sources: []string{"messages"},
	})
	require.Error(t, err, "expected error for invalid regex")
	var inputErr *db.SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

// TestPGSearchContentFTSFallsBackToSubstring verifies that fts mode runs
// ILIKE (not an error) on PG.
func TestPGSearchContentFTSFallsBackToSubstring(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-fts1", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-fts1", 0, "user",
		"optimize the database query performance", "2026-05-01T10:00:00Z", false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "optimize", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts (should fall back)")
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "message", got.Matches[0].Location)
}

// TestPGSearchContentUnknownSource verifies that an unknown source name
// returns a SearchInputError.
func TestPGSearchContentUnknownSource(t *testing.T) {
	store := setupContentSearch(t)
	ctx := context.Background()
	_, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "x", Mode: "substring",
		Sources: []string{"messages", "bogus"},
	})
	require.Error(t, err, "expected error for unknown source name")
	var inputErr *db.SearchInputError
	assert.True(t, errors.As(err, &inputErr),
		"expected *SearchInputError, got %T: %v", err, err)
}

// TestPGSearchContentSnippetPresent verifies that the snippet field is
// populated with content surrounding the match.
func TestPGSearchContentSnippetPresent(t *testing.T) {
	store := setupContentSearch(t)
	insertCSSession(t, store, "cs-snip", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "cs-snip", 0, "user",
		"the secret key is SNIPNEEDLE and nothing more", "2026-05-01T10:00:00Z", false)

	ctx := context.Background()
	got, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "SNIPNEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1)
	assert.NotEmpty(t, got.Matches[0].Snippet)
}

// insertCSChildSession inserts a child session (relationship_type=subagent)
// linked to parentID. message_count and user_message_count > 1 avoid one-shot
// exclusion when the filter requires it.
func insertCSChildSession(
	t *testing.T, store *Store,
	id, project, agent, parentID, startedAt, endedAt string,
) {
	t.Helper()
	_, err := store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 parent_session_id, relationship_type,
			 started_at, ended_at, message_count, user_message_count)
		VALUES ($1, 'test-machine', $2, $3, 'child-seed',
			$4, 'subagent',
			$5::timestamptz, $6::timestamptz, 10, 5)
		ON CONFLICT (id) DO NOTHING`,
		id, project, agent, parentID, startedAt, endedAt,
	)
	require.NoError(t, err, "insert child session %s", id)
}

// TestPGSearchContentIncludeChildren verifies that IncludeChildren=true
// surfaces tool_input and tool_result matches from child (subagent) sessions,
// while IncludeChildren=false excludes them. The scoped CTE wraps only the
// sessions table, so id is unambiguous in the tool-table JOINs.
func TestPGSearchContentIncludeChildren(t *testing.T) {
	store := setupContentSearch(t)

	// Parent session (non-child, passes root filter).
	insertCSSession(t, store, "ic-parent", "proj", "claude",
		"2026-05-01T10:00:00Z", "2026-05-01T10:30:00Z")
	insertCSMessage(t, store, "ic-parent", 0, "assistant",
		"parent running tool", "2026-05-01T10:00:00Z", false)

	// Child session linked to the parent.
	insertCSChildSession(t, store, "ic-child", "proj", "claude",
		"ic-parent", "2026-05-01T10:05:00Z", "2026-05-01T10:25:00Z")
	// Message needed so the JOIN in tool branches can resolve the timestamp.
	insertCSMessage(t, store, "ic-child", 0, "assistant",
		"child running tool", "2026-05-01T10:05:00Z", false)
	insertCSToolCall(t, store, "ic-child", 0, 0,
		"Bash", "child-tu1",
		`{"command":"CHILDNEEDLE"}`, "CHILDNEEDLE in result")

	// Also add a tool_result_events row for the child to cover that branch.
	insertCSChildSession(t, store, "ic-child2", "proj", "claude",
		"ic-parent", "2026-05-01T10:10:00Z", "2026-05-01T10:20:00Z")
	insertCSMessage(t, store, "ic-child2", 0, "assistant",
		"child2 running tool", "2026-05-01T10:10:00Z", false)
	insertCSToolCall(t, store, "ic-child2", 0, 0,
		"Bash", "child2-tu1", `{"command":"ls"}`, "")
	insertCSToolResultEvent(t, store, "ic-child2", 0, 0, 0,
		"child2-tu1", "CHILDNEEDLE in event output")

	ctx := context.Background()

	// --- IncludeChildren=true: child tool_input match must be found ---
	withChildren, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:         "CHILDNEEDLE",
		Mode:            "substring",
		Sources:         []string{"tool_input", "tool_result"},
		Limit:           50,
		IncludeChildren: true,
		IncludeOneShot:  true,
	})
	require.NoError(t, err, "SearchContent IncludeChildren=true")

	var foundToolInput, foundToolResult bool
	for _, m := range withChildren.Matches {
		if m.SessionID == "ic-child" && m.Location == "tool_input" {
			foundToolInput = true
		}
		if m.SessionID == "ic-child2" && m.Location == "tool_result" {
			foundToolResult = true
		}
	}
	assert.True(t, foundToolInput,
		"IncludeChildren=true: child tool_input match not found; matches=%+v",
		withChildren.Matches)
	assert.True(t, foundToolResult,
		"IncludeChildren=true: child tool_result event match not found; matches=%+v",
		withChildren.Matches)

	// --- IncludeChildren=false: child matches must be absent ---
	withoutChildren, err := store.SearchContent(ctx, db.ContentSearchFilter{
		Pattern:         "CHILDNEEDLE",
		Mode:            "substring",
		Sources:         []string{"tool_input", "tool_result"},
		Limit:           50,
		IncludeChildren: false,
		IncludeOneShot:  true,
	})
	require.NoError(t, err, "SearchContent IncludeChildren=false")
	for _, m := range withoutChildren.Matches {
		assert.NotContains(t, []string{"ic-child", "ic-child2"}, m.SessionID,
			"IncludeChildren=false: child session %q appeared in results", m.SessionID)
	}
}
