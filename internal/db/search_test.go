package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearch(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)

	// Session s1: older ended_at, agent "claude"
	insertSession(t, d, "s1", "proj-a",
		func(s *Session) {
			s.Agent = "claude"
			s.FirstMessage = new("alpha beta gamma")
			s.StartedAt = new("2024-01-01T10:00:00Z")
			s.EndedAt = new("2024-01-01T11:00:00Z")
		},
	)
	// Session s2: newer ended_at, agent "codex"
	insertSession(t, d, "s2", "proj-b",
		func(s *Session) {
			s.Agent = "codex"
			s.FirstMessage = new("alpha delta epsilon")
			s.StartedAt = new("2024-01-02T10:00:00Z")
			s.EndedAt = new("2024-01-02T11:00:00Z")
		},
	)
	// Session s3: system messages only — should be excluded
	insertSession(t, d, "s3", "proj-c",
		func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-01-03T10:00:00Z")
			s.EndedAt = new("2024-01-03T11:00:00Z")
		},
	)

	// s1: two messages both containing "alpha" — should collapse to 1 result
	insertMessages(t, d,
		userMsg("s1", 0, "alpha beta gamma"),
		asstMsg("s1", 1, "alpha zeta unique-s1-1"),
	)
	// s2: one matching message
	insertMessages(t, d,
		userMsg("s2", 0, "alpha delta epsilon"),
	)
	// s3: system message — must be excluded
	sysMsg := userMsg("s3", 0, "alpha system hidden")
	sysMsg.IsSystem = true
	insertMessages(t, d, sysMsg)

	// Session s-sysonly-dn: only session_name matches, system messages only.
	insertSession(t, d, "s-sysonly-dn", "proj-sysonly",
		func(s *Session) {
			s.Agent = "claude"
			s.SessionName = new("sysonlydnterm unique display")
			s.FirstMessage = new("no match here")
			s.StartedAt = new("2024-01-04T10:00:00Z")
			s.EndedAt = new("2024-01-04T11:00:00Z")
		},
	)
	sysonlyDN := userMsg("s-sysonly-dn", 0, "irrelevant content")
	sysonlyDN.IsSystem = true
	insertMessages(t, d, sysonlyDN)

	// Session s-sysonly-fm: only first_message matches, system messages only.
	insertSession(t, d, "s-sysonly-fm", "proj-sysonly",
		func(s *Session) {
			s.Agent = "claude"
			s.FirstMessage = new("sysonlyfmterm unique first")
			s.StartedAt = new("2024-01-05T10:00:00Z")
			s.EndedAt = new("2024-01-05T11:00:00Z")
		},
	)
	sysonlyFM := userMsg("s-sysonly-fm", 0, "irrelevant content")
	sysonlyFM.IsSystem = true
	insertMessages(t, d, sysonlyFM)

	// Session s-prefixonly: only prefix-detected system messages (is_system=false).
	// Name branch must exclude this session since it has no visible messages.
	insertSession(t, d, "s-prefixonly", "proj-prefixonly",
		func(s *Session) {
			s.Agent = "claude"
			s.SessionName = new("prefixonlydnterm unique display")
			s.StartedAt = new("2024-01-06T10:00:00Z")
			s.EndedAt = new("2024-01-06T11:00:00Z")
		},
	)
	insertMessages(t, d, userMsg("s-prefixonly", 0,
		"This session is being continued from a previous conversation"))

	t.Run("deduplication: two messages in same session → one result", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha", Limit: 10,
		})
		require.NoError(t, err, "Search")
		// s1 and s2 each have alpha matches; s3 is excluded (system msg)
		assert.Len(t, page.Results, 2, "one per session")
	})

	t.Run("agent field populated from sessions join", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha beta", Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.NotEmpty(t, page.Results, "expected at least one result")
		assert.NotEmpty(t, page.Results[0].Agent, "Agent field empty")
		assert.Equal(t, "claude", page.Results[0].Agent, "Agent")
	})

	t.Run("session_ended_at populated from COALESCE(ended_at, started_at)", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha beta", Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.NotEmpty(t, page.Results, "expected at least one result")
		assert.NotEmpty(t, page.Results[0].SessionEndedAt, "SessionEndedAt")
	})

	t.Run("sort recency: newer session appears first", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha", Limit: 10, Sort: "recency",
		})
		require.NoError(t, err, "Search")
		require.GreaterOrEqual(t, len(page.Results), 2, "want >= 2 results")
		// s2 has ended_at 2024-01-02, s1 has 2024-01-01 — s2 must be first
		assert.Equal(t, "s2", page.Results[0].SessionID, "recency sort: first result")
	})

	t.Run("system messages excluded from results", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "system hidden", Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results, "system-only session results")
	})

	t.Run("name branch excludes system-only sessions via session_name", func(t *testing.T) {
		// s-sysonly-dn has session_name matching "sysonlydnterm" but only
		// system messages. The EXISTS guard must prevent it from appearing.
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "sysonlydnterm", Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results,
			"system-only session via session_name")
	})

	t.Run("name branch excludes system-only sessions via first_message", func(t *testing.T) {
		// s-sysonly-fm has first_message matching "sysonlyfmterm" but only
		// system messages. The EXISTS guard must prevent it from appearing.
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "sysonlyfmterm", Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results,
			"system-only session via first_message")
	})

	t.Run("name branch excludes prefix-only sessions", func(t *testing.T) {
		// s-prefixonly has session_name matching "prefixonlydnterm" but only
		// prefix-detected system messages (is_system=false). The EXISTS guard
		// with prefix exclusion must prevent it from appearing.
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "prefixonlydnterm", Limit: 10,
		})
		require.NoError(t, err, "Search")
		assert.Empty(t, page.Results, "prefix-only session")
	})

	t.Run("invalid sort value defaults to relevance (SQL injection guard)", func(t *testing.T) {
		// Must not return an error or panic — just treats as relevance
		_, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha", Limit: 10, Sort: "'; DROP TABLE sessions; --",
		})
		assert.NoError(t, err, "invalid Sort caused error")
	})

	t.Run("pagination at session level", func(t *testing.T) {
		// Limit 1 should return 1 session with a NextCursor
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha", Limit: 1,
		})
		require.NoError(t, err, "Search")
		assert.Len(t, page.Results, 1, "limit=1 results")
		assert.NotZero(t, page.NextCursor, "NextCursor (more results exist)")
	})

	t.Run("multi-word FTS query matches session name via plain text", func(t *testing.T) {
		// s6: display_name contains two-word phrase; search with a multi-word
		// query that prepareFTSQuery would wrap in quotes ("unique phrase").
		// The name branch must strip those quotes before LIKE matching.
		insertSession(t, d, "s6", "proj-f", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-01-06T10:00:00Z")
		})
		require.NoError(t, d.RenameSession("s6", new("unique phrase session")),
			"RenameSession")
		insertMessages(t, d, userMsg("s6", 0, "no match here"))

		// Simulate prepareFTSQuery wrapping: multi-word queries get quoted.
		page, err := d.Search(context.Background(), SearchFilter{
			Query: `"unique phrase"`, Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.Len(t, page.Results, 1, "quoted query results")
		assert.Equal(t, "s6", page.Results[0].SessionID, "session")
		assert.Equal(t, -1, page.Results[0].Ordinal, "ordinal (name-only match)")
	})

	t.Run("session name match via display_name", func(t *testing.T) {
		// s4: display_name contains "uniquename", no messages match
		insertSession(t, d, "s4", "proj-d", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-01-04T10:00:00Z")
		})
		require.NoError(t, d.RenameSession("s4", new("my uniquename session")),
			"RenameSession")
		// message that does NOT contain "uniquename"
		insertMessages(t, d, userMsg("s4", 0, "hello world"))

		page, err := d.Search(context.Background(), SearchFilter{
			Query: "uniquename", Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.Len(t, page.Results, 1)
		assert.Equal(t, "s4", page.Results[0].SessionID, "session")
		assert.Equal(t, -1, page.Results[0].Ordinal, "ordinal (name-only match)")
	})

	t.Run("name field populated on message-content match", func(t *testing.T) {
		page, err := d.Search(context.Background(), SearchFilter{
			Query: "alpha", Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.NotEmpty(t, page.Results, "expected results")
		// s1 and s2 have no display_name — name should fall back to first_message
		for _, r := range page.Results {
			assert.NotEmpty(t, r.Name, "result %q has empty Name", r.SessionID)
		}
	})

	t.Run("snippet shows matching field when display_name set but first_message matches", func(t *testing.T) {
		// s7: display_name is set to something else; only first_message matches
		insertSession(t, d, "s7", "proj-g", func(s *Session) {
			s.Agent = "claude"
			s.FirstMessage = new("firstmsgonlyterm present here")
			s.StartedAt = new("2024-01-07T10:00:00Z")
		})
		require.NoError(t, d.RenameSession("s7", new("unrelated display name")),
			"RenameSession")
		// message that does NOT contain the search term
		insertMessages(t, d, userMsg("s7", 0, "no match content"))

		page, err := d.Search(context.Background(), SearchFilter{
			Query: "firstmsgonlyterm", Limit: 10,
		})
		require.NoError(t, err, "Search")
		require.Len(t, page.Results, 1)
		r := page.Results[0]
		assert.Equal(t, "s7", r.SessionID, "session")
		assert.Equal(t, -1, r.Ordinal, "ordinal (name-only match)")
		// Snippet must be the first_message (the matching field), not display_name
		assert.Equal(t, "firstmsgonlyterm present here", r.Snippet,
			"snippet should be first_message")
	})

	t.Run("no duplicate when session matches both name and content", func(t *testing.T) {
		// s5: display_name AND message content both contain "doublehit"
		insertSession(t, d, "s5", "proj-e", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-01-05T10:00:00Z")
		})
		require.NoError(t, d.RenameSession("s5", new("doublehit session")),
			"RenameSession")
		insertMessages(t, d, userMsg("s5", 0, "doublehit in message too"))

		page, err := d.Search(context.Background(), SearchFilter{
			Query: "doublehit", Limit: 10,
		})
		require.NoError(t, err, "Search")
		seen := map[string]int{}
		for _, r := range page.Results {
			seen[r.SessionID]++
		}
		assert.Equal(t, 1, seen["s5"], "s5 occurrences")
		// When matched by both, FTS branch wins — ordinal should not be -1
		for _, r := range page.Results {
			if r.SessionID == "s5" {
				assert.NotEqual(t, -1, r.Ordinal,
					"expected real ordinal (message match)")
			}
		}
	})
}

// TestSearchEmptyQueryGuard verifies that Search returns an empty page
// (not an error) when the query is an empty FTS phrase such as `""`,
// mirroring the guard already present in the PostgreSQL Search path.
func TestSearchEmptyQueryGuard(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, userMsg("s1", 0, "hello world"))

	for _, q := range []string{"", `""`} {
		page, err := d.Search(context.Background(), SearchFilter{Query: q, Limit: 10})
		require.NoError(t, err, "Search(%q)", q)
		assert.Empty(t, page.Results, "Search(%q) results", q)
	}
}

// TestSearchDeduplicationManyMessages verifies that a session with many
// matching messages produces exactly one search result. The large message
// count forces FTS5 to maintain multiple internal index segments, which
// previously caused the outer JOIN to return one row per segment rather
// than one row per session.
func TestSearchDeduplicationManyMessages(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2024-01-01T10:00:00Z")
		s.EndedAt = new("2024-01-01T11:00:00Z")
	})

	// Insert enough messages to force multiple FTS5 internal segments.
	const n = 150
	msgs := make([]Message, n)
	for i := range n {
		msgs[i] = userMsg("s1", i, fmt.Sprintf("needle content message number %d", i))
	}
	insertMessages(t, d, msgs...)

	// Optimize the FTS5 index to merge all existing segments into one, then
	// insert additional matching messages in a separate batch. This creates a
	// second segment, reproducing the multi-segment state that caused the outer
	// JOIN to return duplicate rows before the MATCH clause was added.
	_, err := d.getWriter().Exec(
		"INSERT INTO messages_fts(messages_fts) VALUES('optimize')",
	)
	require.NoError(t, err, "fts optimize")
	extra := make([]Message, 20)
	for i := range extra {
		extra[i] = userMsg("s1", n+i,
			fmt.Sprintf("needle extra post-optimize message %d", i))
	}
	insertMessages(t, d, extra...)

	page, err := d.Search(context.Background(), SearchFilter{
		Query: "needle", Limit: 10,
	})
	require.NoError(t, err, "Search")
	if !assert.Len(t, page.Results, 1,
		"single session with %d matching messages", n) {
		for i, r := range page.Results {
			t.Logf("  result[%d]: session_id=%q ordinal=%d", i, r.SessionID, r.Ordinal)
		}
	}
}

// TestSearchTieBreak verifies that when two messages in the same session have
// identical content (and therefore equal FTS5 rank), the ROW_NUMBER()
// tie-breaker consistently returns the message with the lower ordinal.
func TestSearchTieBreak(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)

	insertSession(t, d, "s1", "proj")
	// Insert ordinal=1 first so it gets the lower rowid. If the tie-breaker
	// were "rowid ASC" alone, ordinal=1 would win. The test asserts ordinal=0
	// wins, proving "ordinal ASC" takes precedence over "rowid ASC".
	insertMessages(t, d,
		userMsg("s1", 1, "tiebreak unique phrase alpha"),
	)
	insertMessages(t, d,
		userMsg("s1", 0, "tiebreak unique phrase alpha"),
	)

	page, err := d.Search(context.Background(), SearchFilter{
		Query: "tiebreak unique phrase alpha", Limit: 10,
	})
	require.NoError(t, err, "Search")
	require.Len(t, page.Results, 1)
	assert.Equal(t, 0, page.Results[0].Ordinal,
		"tie-break: lower ordinal wins")
}

func TestSearchSession(t *testing.T) {
	t.Parallel()
	d := testDB(t)

	insertSession(t, d, "s1", "proj")
	insertSession(t, d, "s2", "proj")

	// Message at ordinal 4 has no match in its content but has a tool call
	// whose result_content contains a unique term ("uniquetooloutput").
	toolMsg := asstMsg("s1", 4, "I ran a tool here")
	toolMsg.HasToolUse = true
	toolMsg.ToolCalls = []ToolCall{
		{
			SessionID:     "s1",
			ToolName:      "Bash",
			Category:      "execution",
			ResultContent: "uniquetooloutput: the command succeeded",
		},
	}

	// System message in s1 — excluded from session search (hidden in UI).
	sysMsg := userMsg("s1", 5, "syssearchterm hidden system content")
	sysMsg.IsSystem = true

	// Prefix-detected system message with is_system=false (legacy data).
	prefixMsg := userMsg("s1", 6, "This session is being continued from prefixlegacyterm")

	// Leading-whitespace prefix message — frontend trims before checking.
	wsMsg := userMsg("s1", 7, "  \n This session is being continued wstrimterm")

	// Vertical tab / form feed prefix — exercises \v and \f in LTRIM.
	vfMsg := userMsg("s1", 8, "\v\f This session is being continued vftrimterm")

	// Non-breaking space (U+00A0) prefix — exercises Unicode whitespace in LTRIM.
	nbspMsg := userMsg("s1", 9, "\u00A0 This session is being continued nbsptrimterm")

	// BOM (U+FEFF) prefix — exercises BOM stripping in LTRIM.
	bomMsg := userMsg("s1", 10, "\uFEFF This session is being continued bomtrimterm")

	insertMessages(t, d,
		userMsg("s1", 0, "Hello world, this is a test message"),
		asstMsg("s1", 1, "Here is some Python code: import os; print(os.getcwd())"),
		userMsg("s1", 2, "Can you search for **bold markdown** syntax?"),
		asstMsg("s1", 3, "Another message with no special content"),
		userMsg("s2", 0, "This belongs to a different session entirely"),
		toolMsg,
		sysMsg,
		prefixMsg,
		wsMsg,
		vfMsg,
		nbspMsg,
		bomMsg,
	)

	tests := []struct {
		name      string
		sessionID string
		query     string
		want      []int // expected ordinals
	}{
		{
			name:      "simple substring match",
			sessionID: "s1",
			query:     "test",
			want:      []int{0},
		},
		{
			name:      "case insensitive",
			sessionID: "s1",
			query:     "HELLO",
			want:      []int{0},
		},
		{
			name:      "matches multiple messages",
			sessionID: "s1",
			query:     "message",
			want:      []int{0, 3},
		},
		{
			name:      "matches inside code content",
			sessionID: "s1",
			query:     "import os",
			want:      []int{1},
		},
		{
			name:      "matches raw markdown syntax",
			sessionID: "s1",
			query:     "bold markdown",
			want:      []int{2},
		},
		{
			name:      "no match returns empty",
			sessionID: "s1",
			query:     "nonexistent",
			want:      []int{},
		},
		{
			name:      "scoped to session — does not bleed across sessions",
			sessionID: "s1",
			query:     "different session",
			want:      []int{},
		},
		{
			name:      "other session scoped correctly",
			sessionID: "s2",
			query:     "different session",
			want:      []int{0},
		},
		{
			name:      "empty query returns nil",
			sessionID: "s1",
			query:     "",
			want:      []int{},
		},
		{
			name:      "LIKE special chars escaped — percent sign",
			sessionID: "s1",
			query:     "%",
			want:      []int{},
		},
		{
			name:      "LIKE special chars escaped — underscore",
			sessionID: "s1",
			query:     "_",
			want:      []int{},
		},
		{
			name:      "results ordered by ordinal ascending",
			sessionID: "s1",
			query:     "is",
			want:      []int{0, 1},
		},
		{
			name:      "match in tool result_content only — message content has no match",
			sessionID: "s1",
			query:     "uniquetooloutput",
			want:      []int{4},
		},
		{
			name:      "tool result match is scoped to correct session",
			sessionID: "s2",
			query:     "uniquetooloutput",
			want:      []int{},
		},
		{
			name:      "message with tool call not double-counted when both content and result match",
			sessionID: "s1",
			query:     "tool",
			want:      []int{4},
		},
		{
			name:      "system messages excluded from session search",
			sessionID: "s1",
			query:     "syssearchterm",
			want:      []int{},
		},
		{
			name:      "prefix-detected system messages excluded even with is_system=false",
			sessionID: "s1",
			query:     "prefixlegacyterm",
			want:      []int{},
		},
		{
			name:      "leading-whitespace prefix system message excluded",
			sessionID: "s1",
			query:     "wstrimterm",
			want:      []int{},
		},
		{
			name:      "vertical-tab and form-feed prefix system message excluded",
			sessionID: "s1",
			query:     "vftrimterm",
			want:      []int{},
		},
		{
			name:      "non-breaking space prefix system message excluded",
			sessionID: "s1",
			query:     "nbsptrimterm",
			want:      []int{},
		},
		{
			name:      "BOM prefix system message excluded",
			sessionID: "s1",
			query:     "bomtrimterm",
			want:      []int{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := d.SearchSession(context.Background(), tt.sessionID, tt.query)
			require.NoError(t, err, "SearchSession(%q, %q)", tt.sessionID, tt.query)
			if got == nil {
				got = []int{}
			}
			assert.Equal(t, tt.want, got,
				"SearchSession(%q, %q)", tt.sessionID, tt.query)
		})
	}
}

func TestSearchPaginationStability(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)

	// Three sessions with identical timestamps — ordering must be
	// deterministic via session_id tie-breaker.
	for _, id := range []string{"stab-a", "stab-b", "stab-c"} {
		insertSession(t, d, id, "proj-stab", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new("2024-06-01T12:00:00Z")
			s.EndedAt = new("2024-06-01T13:00:00Z")
		})
		insertMessages(t, d, userMsg(id, 0, "stability test keyword"))
	}

	// Page through results one at a time.
	var allIDs []string
	cursor := 0
	for i := range 3 {
		page, err := d.Search(context.Background(), SearchFilter{
			Query:  "stability",
			Sort:   "recency",
			Limit:  1,
			Cursor: cursor,
		})
		require.NoError(t, err, "page %d", i)
		require.Len(t, page.Results, 1, "page %d", i)
		allIDs = append(allIDs, page.Results[0].SessionID)
		cursor = page.NextCursor
	}

	// Verify no duplicates and ascending session_id order (tie-breaker).
	for i := 1; i < len(allIDs); i++ {
		assert.NotEqual(t, allIDs[i-1], allIDs[i],
			"duplicate session at pages %d-%d: %s", i-1, i, allIDs[i])
		assert.GreaterOrEqual(t, allIDs[i], allIDs[i-1],
			"unstable order: page %d=%s, page %d=%s",
			i-1, allIDs[i-1], i, allIDs[i])
	}
}
