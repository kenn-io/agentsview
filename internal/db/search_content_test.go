package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSearchSession inserts one session with the given messages
// (role/content pairs) for content-search tests.
func seedSearchSession(t *testing.T, d *DB, id, project string, msgs [][2]string) {
	t.Helper()
	// UserMessageCount > 1 so the session is not treated as one-shot and
	// excluded by the default session-list-parity filter.
	insertSession(t, d, id, project, func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	var out []Message
	for i, rc := range msgs {
		out = append(out, Message{
			SessionID: id, Ordinal: i, Role: rc[0],
			Content: rc[1], Timestamp: "2026-05-20T12:00:0" + itoa(i) + "Z",
		})
	}
	require.NoError(t, d.ReplaceSessionMessages(id, out), "ReplaceSessionMessages")
}

func TestSearchContentSubstringMessages(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "please find the DATABASE_URL value"},
		{"assistant", "sure, here is the answer"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "database_url", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches")
	m := got.Matches[0]
	assert.Equal(t, "s1", m.SessionID, "SessionID")
	assert.Equal(t, "message", m.Location, "Location")
	assert.Equal(t, 0, m.Ordinal, "Ordinal")
	assert.Equal(t, "user", m.Role, "Role")
	assert.Contains(t, m.Snippet, "DATABASE_URL", "snippet")
}

// TestSearchContentRedactsStraddlingSecret pins the default (non-reveal)
// content-search guarantee: a secret adjacent to the match that extends past
// the snippet window must not leak. A snippet-only redaction would cut the PEM
// block short and ship raw key bytes.
func TestSearchContentRedactsStraddlingSecret(t *testing.T) {
	d := testDB(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat("MIIBSECRETKEYMATERIAL0123456789ABCDEF\n", 5) +
		"-----END RSA PRIVATE KEY-----"
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "deploy with this attached key " + pem + " ok"},
		{"assistant", "done"},
	})
	base := ContentSearchFilter{
		Pattern: "attached key", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	}
	got, err := d.SearchContent(context.Background(), base)
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1)
	assert.NotContains(t, got.Matches[0].Snippet, "SECRETKEYMATERIAL",
		"default snippet leaked key material")
	assert.Contains(t, got.Matches[0].Snippet, "attached key",
		"snippet lost the matched context")

	// Reveal opts out of redaction (localhost-gated upstream): raw bytes show.
	base.RevealSecrets = true
	rev, err := d.SearchContent(context.Background(), base)
	require.NoError(t, err, "SearchContent reveal")
	assert.Contains(t, rev.Matches[0].Snippet, "SECRETKEYMATERIAL",
		"reveal snippet should show raw bytes")
}

// TestCaseInsensitiveIndexUnicodeOffset pins that the returned offset indexes
// the original string, not strings.ToLower(s). The Kelvin sign U+212A is three
// bytes but lowercases to one ('k'), so a ToLower-based index would report a
// byte offset shifted left of the real match position.
func TestCaseInsensitiveIndexUnicodeOffset(t *testing.T) {
	body := strings.Repeat("K", 5) + "match here"
	got := CaseInsensitiveIndex(body, "MATCH")
	want := strings.Index(body, "match") // real offset into the original string
	assert.Equal(t, want, got,
		"CaseInsensitiveIndex offset into original body")
}

// TestSubstringSnippetUnicodeOffset guards against the snippet panic and
// mis-centering when lowercasing changes byte length. U+023A lowercases to the
// 3-byte U+2C65, so a ToLower-derived offset runs past the original bounds and
// slicing panics; the offset-preserving search must center on the real match.
func TestSubstringSnippetUnicodeOffset(t *testing.T) {
	pat := "MATCH"
	body := strings.Repeat("Ⱥ", 100) + pat + " trailing context here"
	f := ContentSearchFilter{Pattern: pat, Mode: "substring"}
	got := f.substringSnippet(body) // must not panic
	assert.Contains(t, got, pat, "snippet did not center on the match")
}

func TestSearchContentToolIO(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s2", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "s2", Ordinal: 0, Role: "assistant", Content: "running it",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"printenv"}`,
			ResultContent: "AWS_SECRET=topsecretvalue123",
		}},
	}}
	require.NoError(t, d.ReplaceSessionMessages("s2", msgs),
		"ReplaceSessionMessages")
	// match in tool input
	in, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "printenv", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err, "tool_input search")
	require.Len(t, in.Matches, 1, "tool_input search")
	require.Equal(t, "tool_input", in.Matches[0].Location, "Location")
	assert.Equal(t, "Bash", in.Matches[0].ToolName, "ToolName")
	// match in tool result
	res, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "topsecretvalue", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "tool_result search")
	require.Len(t, res.Matches, 1, "tool_result search")
	assert.Equal(t, "tool_result", res.Matches[0].Location, "Location")
}

// TestSearchContentEmptyToolUseIDNotSuppressed guards the tool-result dedup:
// when one empty-tool_use_id call has a result event, it must not suppress the
// result_content of a different empty-tool_use_id call. The dedup is keyed on
// tool_use_id, so one empty ID matching another would hide the second result.
func TestSearchContentEmptyToolUseIDNotSuppressed(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "empti", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "empti", Ordinal: 0, Role: "assistant",
		Content: "running tools", Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{
			{ // empty tool_use_id, result only in result_content, no events
				ToolName: "Bash", Category: "Bash", ToolUseID: "",
				InputJSON: `{"command":"a"}`, ResultContent: "FINDA in result",
			},
			{ // empty tool_use_id, result delivered as an event
				ToolName: "Bash", Category: "Bash", ToolUseID: "",
				InputJSON: `{"command":"b"}`,
				ResultEvents: []ToolResultEvent{
					{Source: "stdout", Status: "ok", Content: "FINDB event"},
				},
			},
		},
	}}
	require.NoError(t, d.ReplaceSessionMessages("empti", msgs),
		"ReplaceSessionMessages")
	// ReplaceSessionMessages routes empty ToolUseID through nilIfEmpty so
	// it lands as NULL. NULL = NULL is false in SQL, so the dedup bug we
	// want to pin (an empty string matching another empty string) only
	// triggers when the column actually holds ''. Force both rows to the
	// literal empty-string form here so the test fails on the old buggy
	// query.
	for _, sql := range []string{
		"UPDATE tool_calls SET tool_use_id = '' WHERE session_id = 'empti'",
		"UPDATE tool_result_events SET tool_use_id = '' WHERE session_id = 'empti'",
	} {
		_, err := d.getWriter().Exec(sql)
		require.NoError(t, err, "force empty tool_use_id")
	}
	for _, mode := range []string{"substring", "regex"} {
		got, err := d.SearchContent(context.Background(), ContentSearchFilter{
			Pattern: "FINDA", Mode: mode,
			Sources: []string{"tool_result"}, Limit: 50,
		})
		require.NoError(t, err, "SearchContent %s", mode)
		require.Len(t, got.Matches, 1,
			"%s: empty-ID result_content suppressed", mode)
		assert.Equal(t, "tool_result", got.Matches[0].Location,
			"%s: want 1 tool_result", mode)
	}
	// The event-delivered result is still searchable via the events branch.
	ev, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "FINDB", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent events")
	assert.Len(t, ev.Matches, 1, "event content not found")
}

// TestSearchContentPaginationStableAcrossTies seeds one message ordinal that
// produces three hits tying on (session, ordinal) — the message body, the tool
// input, and the tool result. Without a stable tie-break, OFFSET paging over
// the UNION could duplicate or skip these rows; the src/row_id keys make
// page-by-page retrieval reproduce the single-page order exactly.
func TestSearchContentPaginationStableAcrossTies(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "tie", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	msgs := []Message{{
		SessionID: "tie", Ordinal: 0, Role: "assistant",
		Content:   "FINDME in message body",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"FINDME"}`,
			ResultContent: "FINDME in result",
		}},
	}}
	require.NoError(t, d.ReplaceSessionMessages("tie", msgs),
		"ReplaceSessionMessages")
	base := ContentSearchFilter{
		Pattern: "FINDME", Mode: "substring",
		Sources: []string{"messages", "tool_input", "tool_result"},
	}
	full := base
	full.Limit = 50
	all, err := d.SearchContent(context.Background(), full)
	require.NoError(t, err, "SearchContent full")
	require.Len(t, all.Matches, 3, "tied matches")
	// The tie-break orders the three sources deterministically by source rank.
	wantOrder := []string{"message", "tool_input", "tool_result"}
	for i, loc := range wantOrder {
		assert.Equal(t, loc, all.Matches[i].Location, "match %d Location", i)
	}
	// Page one row at a time; the sequence must equal the single-page order.
	var paged []ContentMatch
	for cursor := 0; ; {
		p := base
		p.Limit = 1
		p.Cursor = cursor
		page, err := d.SearchContent(context.Background(), p)
		require.NoError(t, err, "SearchContent page at cursor %d", cursor)
		paged = append(paged, page.Matches...)
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	require.Len(t, paged, len(all.Matches),
		"paged rows (duplicates or gaps)")
	for i := range all.Matches {
		assert.Equal(t, all.Matches[i].Location, paged[i].Location,
			"row %d: paged Location != single-page", i)
	}
}

func TestSearchContentRegex(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "r1", "proj", [][2]string{
		{"user", "key AKIA" + "7QHWN2DKR4FYPLJM here"},
		{"assistant", "no secrets in this line"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `AKIA[0-9A-Z]{16}`, Mode: "regex",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent regex")
	require.Len(t, got.Matches, 1, "regex match")
	assert.Equal(t, 0, got.Matches[0].Ordinal, "regex match ordinal")
}

func TestSearchContentUnknownSource(t *testing.T) {
	d := testDB(t)
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "x", Mode: "substring", Sources: []string{"messages", "bogus"},
	})
	require.Error(t, err, "expected error for unknown source name")
}

func TestSearchContentRegexInvalid(t *testing.T) {
	d := testDB(t)
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `(unclosed`, Mode: "regex", Sources: []string{"messages"},
	})
	require.Error(t, err, "expected error for invalid regex")
}

func TestSearchContentFTS(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "f1", "proj", [][2]string{
		{"user", "optimize the database query performance"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "optimize", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	require.Len(t, got.Matches, 1, "fts match")
	assert.Equal(t, "message", got.Matches[0].Location, "fts match Location")
}

func TestSearchContentFTSPhraseSnippetFallsBackToFirstToken(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	body := strings.Repeat("prefix ", 30) + "foo-bar lives here"
	seedSearchSession(t, d, "f-phrase", "proj", [][2]string{
		{"user", body},
	})

	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `"foo bar"`, Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts phrase")
	require.Len(t, got.Matches, 1, "fts phrase match")
	assert.Contains(t, got.Matches[0].Snippet, "foo-bar")
}

func TestSearchContentFTSInvalidQuery(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedSearchSession(t, d, "f2", "proj", [][2]string{
		{"user", "hello world"},
	})
	// A lone double quote is an unbalanced FTS phrase, so SQLite raises a
	// generic syntax error that must be classified as user input, not a 500.
	_, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `"`, Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	var inputErr *SearchInputError
	require.True(t, errors.As(err, &inputErr),
		"malformed FTS query error = %v, want *SearchInputError", err)
}

func TestSearchContentFTSUnavailable(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	// Drop the FTS table so HasFTS reports unavailable; the FTS search must
	// then fail with an internal (non-input) error rather than being
	// misclassified as an invalid user query (HTTP 400).
	_, err := d.getWriter().Exec("DROP TABLE IF EXISTS messages_fts")
	require.NoError(t, err, "drop messages_fts")
	_, err = d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "x", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.Error(t, err, "expected error when FTS is unavailable")
	var inputErr *SearchInputError
	assert.False(t, errors.As(err, &inputErr),
		"FTS-unavailable misclassified as input error: %v", err)
}

func TestSearchContentExcludeSystem(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s3", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	// Plain content (no legacy system-prefix string) so the exclusion is
	// driven solely by the persisted is_system flag, not SystemPrefixSQL.
	msgs := []Message{
		{SessionID: "s3", Ordinal: 0, Role: "user",
			Content: "ordinary message holding NEEDLE", IsSystem: true,
			Timestamp: "2026-05-20T12:00:00Z"},
	}
	require.NoError(t, d.ReplaceSessionMessages("s3", msgs),
		"ReplaceSessionMessages")
	withSys, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent with system")
	assert.Len(t, withSys.Matches, 1, "default should include system messages")
	noSys, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent exclude system")
	assert.Empty(t, noSys.Matches,
		"ExcludeSystem should drop system messages")
}

func TestSearchContentExcludesAutomatedByDefault(t *testing.T) {
	d := testDB(t)
	// Automated sessions are single-turn by definition (UserMessageCount <= 1
	// plus a recognized first message, per sessionIsAutomated), so this one is
	// excluded by default. IncludeAutomated must re-include it via the one-shot
	// automated exemption — which only works if the automated flag is wired.
	insertSession(t, d, "auto", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
		fm := "Warmup"
		s.FirstMessage = &fm
	})
	msgs := []Message{{
		SessionID: "auto", Ordinal: 0, Role: "user",
		Content: "automated NEEDLE run", Timestamp: "2026-05-20T12:00:00Z",
	}}
	require.NoError(t, d.ReplaceSessionMessages("auto", msgs),
		"ReplaceSessionMessages")
	def, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	assert.Empty(t, def.Matches,
		"automated session should be excluded by default")
	inc, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		IncludeAutomated: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent include")
	assert.Len(t, inc.Matches, 1,
		"IncludeAutomated should include the session")
}

func TestSearchContentExcludesOneShotByDefault(t *testing.T) {
	d := testDB(t)
	// A one-shot session: user_message_count <= 1.
	insertSession(t, d, "one", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 1
	})
	msgs := []Message{{
		SessionID: "one", Ordinal: 0, Role: "user",
		Content: "leaked NEEDLE token", Timestamp: "2026-05-20T12:00:00Z",
	}}
	require.NoError(t, d.ReplaceSessionMessages("one", msgs),
		"ReplaceSessionMessages")
	def, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	assert.Empty(t, def.Matches,
		"one-shot session should be excluded by default")
	inc, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		IncludeOneShot: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent include")
	assert.Len(t, inc.Matches, 1,
		"IncludeOneShot should include the session")
}

func TestSearchContentToolResultDedup(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "dup", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	// The pattern appears in BOTH result_content and a result event. The
	// canonical rule must report it once, from the event branch.
	msgs := []Message{{
		SessionID: "dup", Ordinal: 0, Role: "assistant", Content: "run",
		Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON:     `{"command":"echo"}`,
			ResultContent: "DUPNEEDLE in result_content",
			ResultEvents: []ToolResultEvent{{
				ToolUseID: "tu1", Status: "success",
				Content: "DUPNEEDLE in event", EventIndex: 0,
			}},
		}},
	}}
	require.NoError(t, d.ReplaceSessionMessages("dup", msgs),
		"ReplaceSessionMessages")
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "DUPNEEDLE", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "canonical dedup")
	assert.Contains(t, got.Matches[0].Snippet, "event",
		"expected the event content to win")
}

func TestSearchContentCursorPagination(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "pg", "proj", [][2]string{
		{"user", "alpha NEEDLE one"},
		{"user", "beta NEEDLE two"},
		{"user", "gamma NEEDLE three"},
	})
	first, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"}, Limit: 2,
	})
	require.NoError(t, err, "SearchContent page1")
	require.Len(t, first.Matches, 2, "page1 matches")
	require.Equal(t, 2, first.NextCursor, "page1 cursor")
	second, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "NEEDLE", Mode: "substring", Sources: []string{"messages"},
		Limit: 2, Cursor: first.NextCursor,
	})
	require.NoError(t, err, "SearchContent page2")
	require.Len(t, second.Matches, 1, "page2 matches")
	require.Equal(t, 0, second.NextCursor, "page2 cursor")
}

func TestSearchContentMultiSourceWithProjectFilter(t *testing.T) {
	d := testDB(t)
	for _, p := range [][2]string{{"a", "alpha"}, {"b", "beta"}} {
		id, project := p[0], p[1]
		insertSession(t, d, id, project, func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
		})
		msgs := []Message{{
			SessionID: id, Ordinal: 0, Role: "assistant", Content: "FINDME here",
			Timestamp: "2026-05-20T12:00:00Z",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: id + "-tu",
				InputJSON: `{"command":"FINDME"}`, ResultContent: "out FINDME",
			}},
		}}
		require.NoError(t, d.ReplaceSessionMessages(id, msgs),
			"ReplaceSessionMessages %s", id)
	}
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "FINDME", Mode: "substring",
		Sources: []string{"messages", "tool_input", "tool_result"},
		Project: "alpha", Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.NotEmpty(t, got.Matches, "expected matches in project alpha")
	for _, m := range got.Matches {
		assert.Equal(t, "a", m.SessionID, "session leaked")
		assert.Equal(t, "alpha", m.Project, "project leaked")
	}
}

func TestSearchContentDateFilterUsesRequestedTimezone(t *testing.T) {
	d := testDB(t)
	for _, row := range []struct {
		id, started, ended string
	}{
		{"new-york-previous-day", "2024-06-16T01:00:00Z", "2024-06-16T02:00:00Z"},
		{"new-york-requested-day", "2024-06-16T05:00:00Z", "2024-06-16T06:00:00Z"},
	} {
		insertSession(t, d, row.id, "proj", func(s *Session) {
			s.Agent = "claude"
			s.StartedAt = new(row.started)
			s.EndedAt = new(row.ended)
			s.UserMessageCount = 2
		})
		insertMessages(t, d, Message{
			SessionID: row.id, Ordinal: 0, Role: "user",
			Content: "TIMEZONE_NEEDLE", Timestamp: row.started,
		})
	}

	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "TIMEZONE_NEEDLE", Mode: "substring",
		Sources: []string{"messages"}, Date: "2024-06-16",
		Timezone: "America/New_York", Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "new-york-requested-day", got.Matches[0].SessionID)
}

func TestSnippetWindowRuneBoundaries(t *testing.T) {
	// "é" and "ü" are two bytes each, so a byte-radius that lands inside them
	// would slice mid-rune. The match itself is ASCII; only the padding edges
	// are at risk.
	text := strings.Repeat("é", 5) + "MATCH" + strings.Repeat("ü", 5)
	start := strings.Index(text, "MATCH")
	end := start + len("MATCH")
	window := func(radius int) string {
		lo, hi := snippetBounds(text, start, end, radius)
		return text[lo:hi]
	}
	// radius 3 lands mid-rune on both sides, so the partial padding runes are
	// trimmed back to a boundary, leaving one whole rune of padding each side.
	got := window(3)
	assert.Equal(t, "éMATCHü", got, "mid-rune radius")
	// radius 4 lands exactly on rune boundaries, so aligned padding is kept.
	assert.Equal(t, "ééMATCHüü", window(4), "boundary-aligned radius")
	assert.True(t, utf8.ValidString(got), "snippet not valid UTF-8: %q", got)
}

func TestFTSSnippetCentersOnPhrase(t *testing.T) {
	// A stray first token ("error") sits at the start; the real phrase ("error
	// handler") is far past the snippet radius. Centering on the first token
	// alone would window the stray match and drop the phrase.
	prefix := "error in the early part " + strings.Repeat("x ", 80)
	body := prefix + "the real error handler lives here"

	t.Run("phrase present centers on phrase", func(t *testing.T) {
		f := ContentSearchFilter{Pattern: `"error handler"`, Mode: "fts"}
		assert.Contains(t, f.ftsSnippet(body), "error handler",
			"snippet did not center on the phrase")
	})
	t.Run("phrase absent falls back to first token", func(t *testing.T) {
		// No contiguous "error handler" substring, so centering falls back to
		// the first token "error", windowing its early occurrence.
		f := ContentSearchFilter{Pattern: `"error nonexistent"`, Mode: "fts"}
		assert.Contains(t, f.ftsSnippet(body), "error in the early",
			"fallback snippet not centered on first token")
	})
}

// seedUnitSession inserts a session (lineage-configurable via opts) plus full
// Message rows, for derived-unit citation tests that need
// is_system/is_sidechain/tool fields beyond seedSearchSession's role/content
// pairs. SessionID and a per-ordinal timestamp are filled in when unset.
func seedUnitSession(
	t *testing.T, d *DB, id string, opts func(*Session), msgs []Message,
) {
	t.Helper()
	insertSession(t, d, id, "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
		if opts != nil {
			opts(s)
		}
	})
	for i := range msgs {
		msgs[i].SessionID = id
		if msgs[i].Timestamp == "" {
			msgs[i].Timestamp = fmt.Sprintf("2026-05-20T12:00:%02dZ", i)
		}
	}
	require.NoError(t, d.ReplaceSessionMessages(id, msgs),
		"ReplaceSessionMessages %s", id)
}

// matchesByOrdinal indexes a page's matches by anchor ordinal, requiring the
// ordinals to be unique.
func matchesByOrdinal(t *testing.T, page ContentSearchPage) map[int]ContentMatch {
	t.Helper()
	out := make(map[int]ContentMatch, len(page.Matches))
	for _, m := range page.Matches {
		_, dup := out[m.Ordinal]
		require.False(t, dup, "duplicate match ordinal %d", m.Ordinal)
		out[m.Ordinal] = m
	}
	return out
}

// TestSearchContentSubstringDerivedRunRange pins the derived
// conversation-unit citation on substring rows: every match in one assistant
// run carries the run's full range (spanning a non-member system row), an
// embeddable user row and a system row are their own units, and ExcludeSystem
// changes nothing but which rows match.
func TestSearchContentSubstringDerivedRunRange(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "run1", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the RUNHIT question"},
		{Ordinal: 1, Role: "assistant", Content: "RUNHIT step one"},
		{Ordinal: 2, Role: "user", Content: "sys RUNHIT note", IsSystem: true},
		{Ordinal: 3, Role: "assistant", Content: "RUNHIT step two"},
		{Ordinal: 4, Role: "assistant", Content: "RUNHIT step three"},
		{Ordinal: 5, Role: "user", Content: "next question"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 5, "matches")
	byOrd := matchesByOrdinal(t, got)
	assert.Equal(t, [2]int{0, 0}, byOrd[0].OrdinalRange, "user row is its own unit")
	assert.Equal(t, [2]int{2, 2}, byOrd[2].OrdinalRange, "system row is its own unit")
	for _, o := range []int{1, 3, 4} {
		m := byOrd[o]
		assert.Equal(t, [2]int{1, 4}, m.OrdinalRange, "run member %d", o)
		assert.Equal(t, o, m.Ordinal, "anchor ordinal %d", o)
		assert.False(t, m.Subordinate, "top-level run member %d", o)
		assert.False(t, m.Sidechain, "non-sidechain run member %d", o)
		assert.Empty(t, m.Relationship, "top-level relationship %d", o)
		assert.Empty(t, m.ParentSessionID, "top-level parent %d", o)
	}

	// ExcludeSystem drops the system row but leaves the derived ranges of the
	// surviving rows unchanged.
	ex, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "RUNHIT", Mode: "substring",
		Sources: []string{"messages"}, ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent ExcludeSystem")
	require.Len(t, ex.Matches, 4, "ExcludeSystem matches")
	exByOrd := matchesByOrdinal(t, ex)
	assert.NotContains(t, exByOrd, 2, "system row excluded")
	assert.Equal(t, [2]int{0, 0}, exByOrd[0].OrdinalRange)
	for _, o := range []int{1, 3, 4} {
		assert.Equal(t, [2]int{1, 4}, exByOrd[o].OrdinalRange,
			"ExcludeSystem run member %d", o)
	}
}

// TestSearchContentSubstringSidechainRunSubordinate pins the sidechain rules:
// a sidechain run's members are Subordinate + Sidechain, and the sidechain
// flip bounds both the sidechain run and the following top-level run.
func TestSearchContentSubstringSidechainRunSubordinate(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "side1", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the question"},
		{Ordinal: 1, Role: "assistant", Content: "SIDEHIT step a", IsSidechain: true},
		{Ordinal: 2, Role: "assistant", Content: "SIDEHIT step b", IsSidechain: true},
		{Ordinal: 3, Role: "assistant", Content: "main MAINHIT answer"},
	})
	side, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "SIDEHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent sidechain")
	require.Len(t, side.Matches, 2, "sidechain matches")
	for _, m := range side.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "sidechain run range")
		assert.True(t, m.Subordinate, "sidechain run is subordinate")
		assert.True(t, m.Sidechain, "anchor sidechain flag")
		assert.Empty(t, m.Relationship, "no session lineage")
	}
	main, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "MAINHIT", Mode: "substring",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent main")
	require.Len(t, main.Matches, 1, "main matches")
	m := main.Matches[0]
	assert.Equal(t, [2]int{3, 3}, m.OrdinalRange,
		"sidechain flip bounds the top-level run")
	assert.False(t, m.Subordinate, "top-level run")
	assert.False(t, m.Sidechain, "top-level anchor")
}

// TestSearchContentSubstringSubagentLineage pins session-level lineage on
// lexical rows: a match inside a subagent session is Subordinate with
// Relationship and ParentSessionID populated from the sessions join.
func TestSearchContentSubstringSubagentLineage(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "parent", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	seedUnitSession(t, d, "child", func(s *Session) {
		s.ParentSessionID = Ptr("parent")
		s.RelationshipType = "subagent"
	}, []Message{
		{Ordinal: 0, Role: "user", Content: "subagent prompt"},
		{Ordinal: 1, Role: "assistant", Content: "SUBHIT answer"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "SUBHIT", Mode: "substring",
		Sources: []string{"messages"}, IncludeChildren: true, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches")
	m := got.Matches[0]
	assert.Equal(t, [2]int{1, 1}, m.OrdinalRange, "single-member run")
	assert.True(t, m.Subordinate, "subagent session is subordinate")
	assert.Equal(t, "subagent", m.Relationship, "Relationship")
	assert.Equal(t, "parent", m.ParentSessionID, "ParentSessionID")
	assert.False(t, m.Sidechain, "anchor not sidechain")
}

// TestSearchContentToolDerivedRunRange pins derivation for tool_input and
// canonical tool_result rows: the anchor is the tool call's message row, so
// both locations carry the enclosing run's range while the wire Role stays
// the hard-coded "assistant".
func TestSearchContentToolDerivedRunRange(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "tool1", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the question"},
		{Ordinal: 1, Role: "assistant", Content: "running the tool",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
				InputJSON:     `{"command":"TOOLHIT"}`,
				ResultContent: "output RESHIT data",
			}}},
		{Ordinal: 2, Role: "assistant", Content: "continuing the answer"},
		{Ordinal: 3, Role: "user", Content: "thanks"},
	})
	in, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "TOOLHIT", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err, "tool_input search")
	require.Len(t, in.Matches, 1, "tool_input matches")
	assert.Equal(t, "assistant", in.Matches[0].Role, "wire role stays assistant")
	assert.Equal(t, 1, in.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, in.Matches[0].OrdinalRange,
		"tool_input anchor classified from the real message row")

	res, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "RESHIT", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "tool_result search")
	require.Len(t, res.Matches, 1, "tool_result matches")
	assert.Equal(t, [2]int{1, 2}, res.Matches[0].OrdinalRange,
		"canonical tool_result anchor classified from the real message row")
}

// TestSearchContentToolAnchorUsesRealRowRole pins the role-sensitive anchor
// classification: a tool call hanging off a user-role message keeps the
// hard-coded "assistant" wire role, but derivation must classify the anchor
// by the REAL row's role — an embeddable user row is its own unit, never an
// assistant run member.
func TestSearchContentToolAnchorUsesRealRowRole(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "toolu", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "prompt"},
		{Ordinal: 1, Role: "user", Content: "user-attached call",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tuu",
				InputJSON: `{"command":"UHIT"}`,
			}}},
		{Ordinal: 2, Role: "assistant", Content: "assistant reply"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "UHIT", Mode: "substring",
		Sources: []string{"tool_input"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent")
	require.Len(t, got.Matches, 1, "matches")
	m := got.Matches[0]
	assert.Equal(t, "assistant", m.Role, "wire role stays assistant")
	assert.Equal(t, [2]int{1, 1}, m.OrdinalRange,
		"user-role anchor row is its own unit")
}

// TestSearchContentToolResultEventsDerived pins the events branches: an
// orphaned event (no message row at its ordinal) still returns its match
// (row cardinality must not change) with the [o, o] fallback and session
// lineage, while an event whose message row sits inside a run gets the run's
// range via the post-scan anchor lookup.
func TestSearchContentToolResultEventsDerived(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "boss", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
	})
	insertSession(t, d, "evorph", "proj", func(s *Session) {
		s.Agent = "claude"
		s.UserMessageCount = 2
		s.ParentSessionID = Ptr("boss")
		s.RelationshipType = "subagent"
	})
	_, err := d.getWriter().Exec(`INSERT INTO tool_result_events
		(session_id, tool_call_message_ordinal, tool_use_id, source, status,
		 content, content_length, timestamp, event_index)
		VALUES ('evorph', 7, 'tux', 'stdout', 'success',
		 'ORPHHIT event content', 21, '2026-05-20T12:00:00Z', 0)`)
	require.NoError(t, err, "insert orphan event")

	seedUnitSession(t, d, "evrun", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the question"},
		{Ordinal: 1, Role: "assistant", Content: "running",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
				InputJSON: `{"command":"x"}`,
				ResultEvents: []ToolResultEvent{{
					ToolUseID: "tu1", Source: "stdout", Status: "success",
					Content: "EVHIT streamed output", EventIndex: 0,
				}},
			}}},
		{Ordinal: 2, Role: "assistant", Content: "wrapping up"},
	})

	orph, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "ORPHHIT", Mode: "substring",
		Sources: []string{"tool_result"}, IncludeChildren: true, Limit: 50,
	})
	require.NoError(t, err, "orphan search")
	require.Len(t, orph.Matches, 1, "orphaned event row must not be dropped")
	m := orph.Matches[0]
	assert.Equal(t, 7, m.Ordinal, "event ordinal")
	assert.Equal(t, [2]int{7, 7}, m.OrdinalRange, "missing anchor falls back to [o, o]")
	assert.False(t, m.Sidechain, "missing anchor has no sidechain flag")
	assert.True(t, m.Subordinate, "session lineage still applies")
	assert.Equal(t, "subagent", m.Relationship, "Relationship from sessions join")
	assert.Equal(t, "boss", m.ParentSessionID, "ParentSessionID from sessions join")

	ev, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "EVHIT", Mode: "substring",
		Sources: []string{"tool_result"}, Limit: 50,
	})
	require.NoError(t, err, "event search")
	require.Len(t, ev.Matches, 1, "event matches")
	assert.Equal(t, 1, ev.Matches[0].Ordinal, "anchor ordinal")
	assert.Equal(t, [2]int{1, 2}, ev.Matches[0].OrdinalRange,
		"event with a message row inside a run gets the run's range")
}

// TestSearchContentRegexDerivedRange spot-checks that regex mode routes
// through the shared derivation pass.
func TestSearchContentRegexDerivedRange(t *testing.T) {
	d := testDB(t)
	seedUnitSession(t, d, "rx1", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the question"},
		{Ordinal: 1, Role: "assistant", Content: "RXHIT alpha"},
		{Ordinal: 2, Role: "assistant", Content: "RXHIT beta"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: `RXHIT [a-z]+`, Mode: "regex",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent regex")
	require.Len(t, got.Matches, 2, "regex matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
	}
}

// TestSearchContentFTSDerivedRange spot-checks that fts mode routes through
// the shared derivation pass.
func TestSearchContentFTSDerivedRange(t *testing.T) {
	d := testDB(t)
	if !d.HasFTS() {
		t.Skip("fts5 not available")
	}
	seedUnitSession(t, d, "fx1", nil, []Message{
		{Ordinal: 0, Role: "user", Content: "the question"},
		{Ordinal: 1, Role: "assistant", Content: "ftshit alpha step"},
		{Ordinal: 2, Role: "assistant", Content: "ftshit beta step"},
	})
	got, err := d.SearchContent(context.Background(), ContentSearchFilter{
		Pattern: "ftshit", Mode: "fts",
		Sources: []string{"messages"}, Limit: 50,
	})
	require.NoError(t, err, "SearchContent fts")
	require.Len(t, got.Matches, 2, "fts matches")
	for _, m := range got.Matches {
		assert.Equal(t, [2]int{1, 2}, m.OrdinalRange, "derived run range")
		assert.False(t, m.Subordinate, "top-level run")
	}
}
