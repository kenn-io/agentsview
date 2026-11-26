package db

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/agentsview/internal/config"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/ocr"
)

type substringHit struct {
	Location string
	Ordinal  int
}

func substringHits(t *testing.T, d *DB, f ContentSearchFilter) []substringHit {
	t.Helper()
	page, err := d.SearchContent(t.Context(), f)
	require.NoError(t, err, "%s %q", f.Mode, f.Pattern)
	hits := []substringHit{}
	for _, m := range page.Matches {
		hits = append(hits, substringHit{m.Location, m.Ordinal})
	}
	return hits
}

// countMessagesForSubstringIndex sets a session's message count high enough
// that searches over it take the index for patterns that list a few
// messages.
func countMessagesForSubstringIndex(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(t.Context(), `UPDATE sessions SET message_count = 100 WHERE id = ?`, id)
	require.NoError(t, err)
}

func requireSubstringIndexIntact(t *testing.T, d *DB) {
	t.Helper()
	for _, table := range []string{"message_trigram_fts", "thinking_trigram_fts"} {
		_, err := d.getWriter().Exec(t.Context(),
			fmt.Sprintf("INSERT INTO %[1]s(%[1]s) VALUES('integrity-check')", table))
		require.NoError(t, err, table)
	}
}

// The index only narrows the rows LIKE checks, so every search must return
// what a full scan returns: ASCII case folding, non-ASCII case kept
// distinct, LIKE wildcards and quotes taken literally, patterns too short
// for the index, patterns longer than the trigram cap, and thinking text.
func TestSearchContentSubstringIndexMatchesScan(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "Please check the Deploy-Window settings"},
		{"assistant", `Done: 100% complete_now and "ship it" twice`},
		{"user", "\u00dcber alles"},
		{"assistant", "\u00fcber alles"},
		{"user", "ab cd"},
	})
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 5, Role: "assistant", Content: "answer",
		ThinkingText: "private plan mentions zebra-crossing",
		Timestamp:    "2026-05-20T12:00:05Z",
	})
	countMessagesForSubstringIndex(t, d, "s1")
	for _, pattern := range []string{"deploy-window", "zebra-crossing"} {
		candidates, err := d.substringCandidates(t.Context(), ContentSearchFilter{Pattern: pattern, Sources: []string{"messages", "thinking"}}, []string{pattern})
		require.NoError(t, err)
		require.NotEmpty(t, candidates.message.sql, pattern)
		require.NotEmpty(t, candidates.thinking.sql, pattern)
	}

	patterns := map[string][]substringHit{
		"deploy-window":  {{"message", 0}},
		"100% complete_": {{"message", 1}},
		`"ship it"`:      {{"message", 1}},
		"\u00fcber":      {{"message", 3}},
		"\u00dcBER":      {{"message", 2}},
		"ab":             {{"message", 4}},
		"zebra-crossing": {{"thinking", 5}},
		`Done: 100% complete_now and "ship it" twice`: {{"message", 1}},
		"nothing-here": {},
	}
	indexed := map[string][]substringHit{}
	for pattern, want := range patterns {
		got := substringHits(t, d, ContentSearchFilter{Pattern: pattern, Limit: 50})
		assert.Equal(t, want, got, "substring %q", pattern)
		indexed["substring "+pattern] = got
		indexed["regex "+pattern] = substringHits(t, d,
			ContentSearchFilter{Pattern: regexQuoteForTest(pattern), Mode: "regex", Limit: 50})
	}

	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(t.Context(), stmt)
		require.NoError(t, err, stmt)
	}
	require.False(t, d.hasSubstringIndex(t.Context()))
	for pattern := range patterns {
		assert.Equal(t, indexed["substring "+pattern],
			substringHits(t, d, ContentSearchFilter{Pattern: pattern, Limit: 50}), "substring %q", pattern)
		assert.Equal(t, indexed["regex "+pattern],
			substringHits(t, d, ContentSearchFilter{Pattern: regexQuoteForTest(pattern), Mode: "regex", Limit: 50}),
			"regex %q", pattern)
	}
}

func regexQuoteForTest(pattern string) string {
	out := make([]rune, 0, len(pattern))
	for _, r := range pattern {
		switch r {
		case '\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$':
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

// Inserts, edits to message, dialogue, and thinking text, transcript
// replacement, and session deletion must each keep the index equal to the
// stored text; a missing entry would hide a real match.
func TestSubstringIndexFollowsMessageWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "original request"},
		{"assistant", "original answer"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "original", Limit: 50}), 2)

	for _, stmt := range []string{
		`UPDATE messages SET content = 'rewritten request' WHERE session_id = 's1' AND ordinal = 0`,
		`UPDATE messages SET dialogue_text = 'projected answer' WHERE session_id = 's1' AND ordinal = 1`,
		`UPDATE messages SET thinking_text = 'quiet reasoning' WHERE session_id = 's1' AND ordinal = 1`,
	} {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 0}},
		substringHits(t, d, ContentSearchFilter{Pattern: "rewritten", Limit: 50}))
	assert.Equal(t, []substringHit{{"message", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "projected", Limit: 50}))
	assert.Equal(t, []substringHit{{"thinking", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "quiet reasoning", Limit: 50}))
	assert.Empty(t, substringHits(t, d, ContentSearchFilter{Pattern: "original", Limit: 50}))

	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		userMsg("s1", 0, "replacement prompt"),
		{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "replacement reply", ThinkingText: "fresh reasoning"},
	}))
	requireSubstringIndexIntact(t, d)
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "replacement", Limit: 50}), 2)
	assert.Equal(t, []substringHit{{"thinking", 1}},
		substringHits(t, d, ContentSearchFilter{Pattern: "fresh reasoning", Limit: 50}))

	require.NoError(t, d.DeleteSession(ctx, "s1"))
	requireSubstringIndexIntact(t, d)
	var rows int
	require.NoError(t, d.getReader().QueryRow(ctx,
		`SELECT count(*) FROM message_trigram_fts WHERE message_trigram_fts MATCH '"rep" AND "lac"'`).Scan(&rows))
	assert.Zero(t, rows)
}

// An archive from before the index gets it on its next writable open, and a
// bulk import that drops the search indexes falls back to scanning until it
// rebuilds them.
func TestSubstringIndexBuiltForExistingArchiveAndBulkImport(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(ctx, path)
	require.NoError(t, err)
	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "written before the upgrade"},
		{"assistant", "kept answer"},
	})
	require.NoError(t, d.Close())

	d, err = Open(ctx, path)
	require.NoError(t, err)
	defer d.Close()
	require.True(t, d.hasSubstringIndex(ctx))
	countMessagesForSubstringIndex(t, d, "s1")
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 0}},
		substringHits(t, d, ContentSearchFilter{Pattern: "before the upgrade", Limit: 50}))

	require.NoError(t, d.DropFTS(ctx))
	require.False(t, d.hasSubstringIndex(ctx))
	insertMessages(t, d, asstMsg("s1", 2, "imported while dropped"))
	assert.Equal(t, []substringHit{{"message", 2}},
		substringHits(t, d, ContentSearchFilter{Pattern: "while dropped", Limit: 50}))

	require.NoError(t, d.RebuildFTS(ctx))
	require.True(t, d.hasSubstringIndex(ctx))
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, []substringHit{{"message", 2}},
		substringHits(t, d, ContentSearchFilter{Pattern: "while dropped", Limit: 50}))
}

// A pattern the index lists for few messages drives the search from the
// index; one it lists for more than a quarter of the scope's messages, and
// patterns or scopes the index cannot help, keep the scan.
func TestSubstringCandidatesOnlyForSelectivePatterns(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.MessageCount = 8
		s.UserMessageCount = 4
	})
	for i := range 8 {
		content := "common words here"
		if i == 3 {
			content = "rare-token here"
		}
		insertMessages(t, d, asstMsg("s1", i, content))
	}
	candidates := func(f ContentSearchFilter) substringCandidateFilters {
		t.Helper()
		f.Sources = []string{"messages", "thinking"}
		got, err := d.substringCandidates(ctx, f, []string{f.Pattern})
		require.NoError(t, err)
		return got
	}

	rare := candidates(ContentSearchFilter{Pattern: "rare-token"})
	require.NotEmpty(t, rare.message.sql)
	for _, tc := range []struct {
		source            string
		message, thinking bool
	}{
		{"messages", true, false},
		{"thinking", false, true},
	} {
		got, err := d.substringCandidates(ctx, ContentSearchFilter{Sources: []string{tc.source}}, []string{"rare-token"})
		require.NoError(t, err)
		assert.Equal(t, tc.message, got.message.sql != "")
		assert.Equal(t, tc.thinking, got.thinking.sql != "")
	}
	rows, err := d.getReader().Query(ctx,
		"EXPLAIN QUERY PLAN SELECT m.id FROM messages m WHERE m.content LIKE '%rare-token%'"+rare.message.sql,
		rare.message.args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan.String(), "SEARCH m USING INTEGER PRIMARY KEY (rowid=?)")
	assert.Contains(t, plan.String(), "VIRTUAL TABLE INDEX 0:M")

	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "common"}).message.sql)
	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "ra"}).message.sql)
	assert.Empty(t, candidates(ContentSearchFilter{Pattern: "rare-token", SessionID: "s1"}).message.sql)
	assert.Equal(t, []substringHit{{"message", 3}},
		substringHits(t, d, ContentSearchFilter{Pattern: "rare-token", Limit: 50}))
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "common", Limit: 50}), 7)
	// A capture splits the prefix into runs too short for the index.
	_, err = d.getWriter().Exec(ctx, `UPDATE messages SET content = 'common rare-token foo here' WHERE session_id = 's1' AND ordinal = 3`)
	require.NoError(t, err)
	re := regexp.MustCompile(`(fo)o`)
	prefix := regexBodyLiterals(re, requiredLiterals(re.String()))
	got, err := d.substringCandidates(ctx, ContentSearchFilter{Sources: []string{"messages"}}, regexLiteralTexts(prefix))
	require.NoError(t, err)
	require.NotEmpty(t, got.message.sql)
}

// An archive import drops the index with the other FTS tables. A search that
// chose index filters just before the drop must rerun as a scan instead of
// failing on the missing tables.
func TestSubstringSearchScansWhenImportDropsIndexMidSearch(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "where is the rare-token"},
		{"assistant", "nothing to see"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	f := ContentSearchFilter{
		Pattern: "rare-token", Limit: 50,
		Sources: []string{"messages", "thinking", "tool_input", "tool_result"},
	}

	var usedIndex []bool
	page, err := searchWithSubstringCandidates(ctx, d, f, []string{f.Pattern},
		func(candidates substringCandidateFilters) (ContentSearchPage, error) {
			usedIndex = append(usedIndex, candidates.message.sql != "")
			if len(usedIndex) == 1 {
				require.NoError(t, d.DropFTS(ctx))
			}
			return d.substringSearchPage(ctx, f, candidates)
		})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, usedIndex)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "message", page.Matches[0].Location)
	assert.Equal(t, 0, page.Matches[0].Ordinal)
}

// Import can drop the search index after candidate selection but before the
// query starts. Both serving and cold read-only queries must retry as a scan.
func TestRegexSearchScansWhenImportDropsIndexBeforeFirstRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cold      bool
		wantIndex []bool
	}{
		{"serving", false, []bool{true, false}},
		{"cold read-only", true, []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := testDB(t)
			ctx := t.Context()
			seedSearchSession(t, archive, "s1", "proj", [][2]string{
				{"user", "where is the rare-token"},
				{"assistant", "nothing to see"},
			})
			countMessagesForSubstringIndex(t, archive, "s1")
			d := archive
			if tc.cold {
				reader, err := OpenReadOnly(ctx, archive.Path())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				d = reader
			}
			f := ContentSearchFilter{
				Pattern: `\brare-tok(en)`, Mode: "regex", Limit: 50,
				Sources: []string{"messages", "thinking", "tool_input", "tool_result"},
			}
			re, err := regexp.Compile(f.Pattern)
			require.NoError(t, err)
			literals := regexBodyLiterals(re, requiredLiterals(f.Pattern))

			var usedIndex []bool
			matches, err := searchWithSubstringCandidates(ctx, d, f, regexLiteralTexts(literals),
				func(candidates substringCandidateFilters) ([]ContentMatch, error) {
					usedIndex = append(usedIndex, candidates.message.sql != "")
					if len(usedIndex) == 1 {
						require.NoError(t, archive.DropFTS(ctx))
					}
					return d.regexSearchRows(ctx, f, re, literals, candidates)
				})
			require.NoError(t, err)
			assert.Equal(t, tc.wantIndex, usedIndex)
			require.Len(t, matches, 1)
			assert.Equal(t, "message", matches[0].Location)
			assert.Equal(t, 0, matches[0].Ordinal)
		})
	}
}

// Terms search prefilters sessions through the index only for terms it
// lists selectively, and returns the same exchanges as the scan.
func TestSearchContentTermsIndexMatchesScan(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	for i, msgs := range [][2]string{
		{"Deploy-Window opened", "the beta setting is required"},
		{"alpha%_ marker here", "beta follows"},
		{"nothing relevant", "still nothing"},
		{"Über alles", "über reply"},
	} {
		id := fmt.Sprintf("terms-%d", i)
		seedSearchSession(t, d, id, "proj", [][2]string{{"user", msgs[0]}, {"assistant", msgs[1]}})
		countMessagesForSubstringIndex(t, d, id)
	}
	filter := func(pattern string) ContentSearchFilter {
		return ContentSearchFilter{Pattern: pattern, Mode: "terms", Scope: "all", Limit: 50}
	}
	indexed, err := d.termsIndexQueries(ctx, filter("deploy-window beta"), []string{"deploy-window", "beta", "ab"})
	require.NoError(t, err)
	assert.Contains(t, indexed, "deploy-window")
	assert.NotContains(t, indexed, "ab")

	patterns := map[string][]substringHit{
		"deploy-window beta": {{"message", 0}},
		"alpha%_ beta":       {{"message", 0}},
		"über":               {{"message", 0}},
		"missing beta":       {},
	}
	before := map[string][]substringHit{}
	for pattern, want := range patterns {
		got := substringHits(t, d, filter(pattern))
		assert.Equal(t, want, got, pattern)
		before[pattern] = got
	}
	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	for pattern := range patterns {
		assert.Equal(t, before[pattern], substringHits(t, d, filter(pattern)), pattern)
	}
}

// Terms search gets the same rerun as substring search when an import drops
// the index between choosing the index prefilter and running the query.
func TestTermsSearchScansWhenImportDropsIndexMidSearch(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s1", "proj", [][2]string{
		{"user", "where is the rare-token"},
		{"assistant", "nothing to see"},
	})
	countMessagesForSubstringIndex(t, d, "s1")
	f := ContentSearchFilter{Pattern: "rare-token", Mode: "terms", Scope: "all", Limit: 50}
	terms := ParseContentSearchTerms(f.Pattern)

	var usedIndex []bool
	page, err := searchWithTermsIndex(ctx, d, f, terms,
		func(indexed map[string]string) (ContentSearchPage, error) {
			usedIndex = append(usedIndex, len(indexed) > 0)
			if len(usedIndex) == 1 {
				require.NoError(t, d.DropFTS(ctx))
			}
			return d.termsSearchPage(ctx, f, terms, indexed)
		})
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, usedIndex)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "message", page.Matches[0].Location)
	assert.Equal(t, 0, page.Matches[0].Ordinal)
}

func TestDaemonOpenDefersSubstringIndexRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	seedSearchSession(t, d, "session", "project", [][2]string{{"user", "needle before repair"}})
	for _, stmt := range dropSubstringIndexSQL {
		_, err := d.getWriter().Exec(t.Context(), stmt)
		require.NoError(t, err)
	}
	require.NoError(t, d.Close())
	d, err := OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	require.False(t, d.hasSubstringIndex(t.Context()))
	seedSearchSession(t, d, "new", "project", [][2]string{{"user", "needle after open"}})
	filter := ContentSearchFilter{Pattern: "needle", Limit: 50}
	before := substringHits(t, d, filter)
	require.Len(t, before, 2)
	var loads atomic.Int32
	d.searchKept.Size = func(SearchPage) int64 { loads.Add(1); return 1 }
	search := SearchFilter{Query: "needle"}
	page, err := d.Search(t.Context(), search)
	require.NoError(t, err)
	var token, after string
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&token))
	require.NoError(t, d.RepairMessageFTS(t.Context()))
	require.NoError(t, d.getReader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key='search_history_token'`).Scan(&after))
	assert.Equal(t, token, after)
	require.True(t, d.hasSubstringIndex(t.Context()))
	requireSubstringIndexIntact(t, d)
	assert.Equal(t, before, substringHits(t, d, filter))
	repeat, err := d.Search(t.Context(), search)
	require.NoError(t, err)
	assert.Equal(t, page, repeat)
	assert.EqualValues(t, 1, loads.Load())
}

func TestDaemonOpenBuildsIndexesForEmptyArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenDaemonWithProgress(t.Context(), path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assert.True(t, d.hasSubstringIndex(t.Context()))
	if simpleFTSRuntimeConfig.available() {
		assert.True(t, d.HasCJKFTS(t.Context()))
	}
	assert.False(t, d.searchIndexRepairPending.Load())
	seedSearchSession(t, d, "session", "project", [][2]string{{"user", "中文搜索 needleexample"}})
	requireSubstringIndexIntact(t, d)
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "needleexample", Limit: 50}), 1)
	require.NoError(t, d.RepairMessageFTS(t.Context()))
	if simpleFTSRuntimeConfig.available() {
		page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "中文搜索", Mode: "fts", Sources: []string{"messages"}, Limit: 20})
		require.NoError(t, err)
		assert.Len(t, page.Matches, 1)
	}
}

func TestRequiredLiterals(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		want    []regexLiteral
	}{
		{`\bdeploy-window`, []regexLiteral{{text: "deploy-window"}}},
		{`unique-rare.*common fixed portion`, []regexLiteral{{text: "unique-rare"}, {text: "common fixed portion"}}},
		{`foo(?:alpha|zebra)bar`, []regexLiteral{{text: "foo"}, {text: "bar"}}},
		{`fo(o)`, []regexLiteral{{text: "fo"}, {text: "o"}}},
		{`abc*`, []regexLiteral{{text: "ab"}}},
		{`(?:abc){2,3}`, []regexLiteral{{text: "abc"}}},
		{`(?i)KELVIN`, []regexLiteral{{text: "elvin", fold: true}}},
		{`(?i)sk`, nil},
		{`ab\x00cdef`, []regexLiteral{{text: "ab"}, {text: "cdef"}}},
		{`abc\x{FFFD}def`, []regexLiteral{{text: "abc"}, {text: "def"}}},
	} {
		t.Run(tc.pattern, func(t *testing.T) { assert.Equal(t, tc.want, requiredLiterals(tc.pattern)) })
	}
	re := regexp.MustCompile(`fo(o)`)
	assert.Equal(t, []regexLiteral{{text: "foo"}}, regexBodyLiterals(re, requiredLiterals(re.String())))
}

func TestRegexSearchCapsSQLLiterals(t *testing.T) {
	d := testDB(t)
	var pattern, body strings.Builder
	for i := range 1100 {
		word := fmt.Sprintf("word%04d ", i)
		pattern.WriteString(`\b` + word)
		body.WriteString(word)
	}
	for i := range 3 {
		seedSearchSession(t, d, fmt.Sprintf("long-%d", i), "proj", [][2]string{{"user", body.String()}})
	}
	page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: pattern.String(), Mode: "regex", Limit: 10, Sources: []string{"messages"}})
	require.NoError(t, err)
	assert.Len(t, page.Matches, 3)
}

func TestSubstringIndexQueryReachesLaterLiterals(t *testing.T) {
	d := testDB(t)
	common := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	insertSession(t, d, "suffix", "proj", func(s *Session) { s.MessageCount = 8; s.UserMessageCount = 4 })
	for i := range 8 {
		body := common
		if i == 3 {
			body += " rare-needle"
		}
		insertMessages(t, d, asstMsg("suffix", i, body))
	}
	literals := []string{common, "rare-needle"}
	query, ok := substringIndexQuery(literals...)
	require.True(t, ok)
	assert.Contains(t, query, `"rar"`)
	candidates, err := d.substringCandidates(t.Context(), ContentSearchFilter{Sources: []string{"messages"}}, literals)
	require.NoError(t, err)
	require.NotEmpty(t, candidates.message.sql)
	query, ok = substringIndexQuery(strings.Repeat("0", 40) + "needle")
	require.True(t, ok)
	assert.Contains(t, query, `"nee"`)
}

func TestRegexRequiredLiteralPrefiltersToolSources(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	insertSession(t, d, "s1", "proj", func(s *Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(ctx, "s1", []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "rare-needle in message"},
		{SessionID: "s1", Ordinal: 1, Role: "user", Content: "another question ſearch"},
		{SessionID: "s1", Ordinal: 2, Role: "assistant", Content: "Foo then bar", ThinkingText: "rare-needle in thought", ToolCalls: []ToolCall{
			{ToolName: "Bash", Category: "Bash", ToolUseID: "a", InputJSON: `{"cmd":"rare-needle"}`, ResultContent: "rare-needle output"},
			{ToolName: "Bash", Category: "Bash", ToolUseID: "b", InputJSON: `{"cmd":"other"}`, ResultContent: "panic in tool output"},
			{ToolName: "Bash", Category: "Bash", ToolUseID: "c", InputJSON: `{"cmd":"other"}`, ResultEvents: []ToolResultEvent{{ToolUseID: "c", Source: "stdout", Status: "success", Content: "rare-needle event", EventIndex: 0}}},
		}},
	}))
	// The orphan-copy sanitizer leaves legacy thinking text unchanged.
	_, err := d.getWriter().Exec(ctx, `UPDATE messages SET thinking_text = ? WHERE session_id = 's1' AND ordinal = 2`, "before\x00rare-needle in thought foo\xffbar")
	require.NoError(t, err)
	// The two image occurrence paths join cached text to a call or an event.
	identity, err := ocr.NewIdentity("test", "model", "revision")
	require.NoError(t, err)
	for _, image := range []struct {
		hash, text, occurrenceSQL string
	}{
		{"call-image", "before\x00rare-needle call image foo\xffbar", `INSERT INTO image_text_occurrences(session_id, call_id, image_index, image_hash) SELECT session_id, id, 0, ? FROM tool_calls WHERE session_id = 's1' AND tool_use_id = 'a'`},
		{"event-image", "before\x00rare-needle event image", `INSERT INTO image_text_occurrences(session_id, event_id, image_index, image_hash) SELECT session_id, id, 0, ? FROM tool_result_events WHERE session_id = 's1' AND tool_use_id = 'c'`},
	} {
		result, err := d.getWriter().Exec(ctx, image.occurrenceSQL, image.hash)
		require.NoError(t, err)
		count, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		require.NoError(t, d.SaveImageText(ctx, image.hash, identity, image.text, false))
	}

	f := ContentSearchFilter{Pattern: `\brare-needle`, Mode: "regex", Limit: 50, Sources: []string{"messages", "thinking", "tool_input", "tool_result"}}
	countMessagesForSubstringIndex(t, d, "s1")
	// Observe the queries prepared by the public search path on its reader.
	reader := d.rawReader()
	reader.SetMaxOpenConns(1)
	conn, err := reader.Conn(ctx)
	require.NoError(t, err)
	var indexQueries atomic.Int64
	var sqlite *sqlite3.SQLiteConn
	require.NoError(t, conn.Raw(func(raw any) error {
		sqlite = raw.(*diagnosticReaderConn).SQLiteConn
		sqlite.RegisterAuthorizer(func(op int, _, name, _ string) int {
			if op == sqlite3.SQLITE_FUNCTION && strings.EqualFold(name, "match") {
				indexQueries.Add(1)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	require.NoError(t, conn.Close())
	defer sqlite.RegisterAuthorizer(nil)
	page, err := d.SearchContent(ctx, ContentSearchFilter{Pattern: f.Pattern, Mode: "regex", Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Positive(t, indexQueries.Load(), "search must query the substring index")
	re := regexp.MustCompile(f.Pattern)
	literals := regexBodyLiterals(re, requiredLiterals(f.Pattern))
	snapshot, err := d.BeginMessageSnapshot(ctx)
	require.NoError(t, err)
	defer snapshot.Close()
	scope, scopeArgs := sessionScopeSubquery(f)
	count := func(source string, literal []regexLiteral) int {
		filter := f
		filter.Sources = []string{source}
		rows, err := regexCandidateRows(ctx, snapshot.tx, filter, literal, scope, scopeArgs, substringCandidateFilters{})
		require.NoError(t, err)
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		require.NoError(t, rows.Err())
		return n
	}
	for _, source := range []string{"tool_input", "tool_result"} {
		assert.Less(t, count(source, literals), count(source, nil), source)
	}
	require.NoError(t, snapshot.Close())
	locations := make(map[string]int)
	for _, hit := range page.Matches {
		locations[hit.Location]++
	}
	assert.Equal(t, map[string]int{"message": 1, "thinking": 1, "tool_input": 1, "tool_result": 4}, locations)
	patterns := []string{f.Pattern, `(?i)PANIC`, `(?i)search`, `Foo.*(?i)bar`, `foo\x{FFFD}bar`}
	pages := make(map[string]ContentSearchPage)
	for _, pattern := range patterns {
		page, err := d.SearchContent(ctx, ContentSearchFilter{Pattern: pattern, Mode: "regex", Limit: 50})
		require.NoError(t, err)
		require.NotEmpty(t, page.Matches, pattern)
		pages[pattern] = page
	}
	require.NoError(t, d.DropFTS(ctx))
	for _, pattern := range patterns {
		f.Pattern = pattern
		fullScan, err := d.regexSearchRows(ctx, f, regexp.MustCompile(pattern), nil, substringCandidateFilters{})
		require.NoError(t, err)
		scanPage := f.Page(fullScan)
		require.NoError(t, d.deriveLexicalUnits(ctx, scanPage.Matches))
		assert.Equal(t, scanPage, pages[pattern], pattern)
	}
}
