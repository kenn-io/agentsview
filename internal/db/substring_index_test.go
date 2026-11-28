package db

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
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

func dropToolSubstringIndexForTest(t *testing.T, d *DB) {
	t.Helper()
	for _, stmt := range dropToolSubstringSQL() {
		_, err := d.getWriter().ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	_, err := d.getWriter().ExecContext(t.Context(), "DELETE FROM stats WHERE key=?", toolSubstringStateKey)
	require.NoError(t, err)
}

func TestToolSubstringSynchronousBuildReportsProgress(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
	_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
 SELECT id,session_id,'Read','Read','needle' FROM messages WHERE session_id='s'`)
	require.NoError(t, err)
	dropToolSubstringIndexForTest(t, d)
	require.NoError(t, d.Close())

	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	var details []string
	d, err = OpenWithProgress(ctx, path, config.ArchiveContentFull, func(progress OpenProgress) {
		details = append(details, progress.Detail)
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assert.Contains(t, details, "Building tool search index")
	assert.Contains(t, output.String(), "building SQLite tool search index")
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "needle", Sources: []string{"tool_input"}, Limit: 10}), 1)
}

func TestToolSubstringReadOnlyScansWhenIndexModuleUnavailable(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	t.Cleanup(func() { _ = d.Close() })
	seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
	_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json,result_content)
 SELECT id,session_id,'Read','Read','needle input','needle result' FROM messages WHERE session_id='s'`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,tool_use_id,source,status,content,content_length,event_index)
 VALUES('s',0,'','stdout','success','needle event',12,0)`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	// Simulate an unavailable tool index module; the message FTS tables remain available.
	raw, err := sql.Open("sqlite3", makeDSN(path, false))
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	for _, stmt := range []string{
		"PRAGMA writable_schema=ON",
		`UPDATE sqlite_master SET sql=replace(sql, 'USING fts5(', 'USING fts5_absent(') WHERE name IN ('tool_input_trigram_fts','tool_result_trigram_fts','tool_event_trigram_fts')`,
		"PRAGMA writable_schema=OFF",
	} {
		_, err = raw.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())
	reader, err := OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	hits := substringHits(t, reader, ContentSearchFilter{Pattern: "needle", Sources: []string{"tool_input", "tool_result"}, Limit: 10})
	assert.Len(t, hits, 3)
}

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
	require.True(t, hasToolSubstringIndex(ctx, d.getReader()))
	assert.False(t, d.SearchRepairPending())
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

func TestToolSubstringIndexMatchesScanAndTracksWrites(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "tools", "proj", [][2]string{{"assistant", "running"}})
	countMessagesForSubstringIndex(t, d, "tools")
	_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id, session_id, tool_name, category, tool_use_id, input_json, result_content)
 SELECT id, session_id, 'Read', 'Read', '', 'RareNeedle input', 'RareNeedle result' FROM messages WHERE session_id='tools'`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_result_events(session_id, tool_call_message_ordinal, tool_use_id, source, status, content, content_length, event_index)
 VALUES('tools', 0, '', 'stdout', 'success', 'RareNeedle event', 16, 0)`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3)) INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json,result_content) SELECT m.id,m.session_id,'Read','Read','other','other' FROM messages m,n WHERE m.session_id='tools'`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3)) INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,tool_use_id,source,status,content,content_length,event_index) SELECT 'tools',0,'','stdout','success','other',5,i FROM n`)
	require.NoError(t, err)
	f := ContentSearchFilter{Pattern: "rareneedle", Limit: 50, Sources: []string{"tool_input", "tool_result"}}
	candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
	require.NoError(t, err)
	require.NotEmpty(t, candidates.toolInput.sql)
	require.NotEmpty(t, candidates.toolResult.sql)
	require.NotEmpty(t, candidates.toolEvent.sql)
	rows, err := d.getReader().Query(ctx, "EXPLAIN QUERY PLAN SELECT tc.id FROM tool_calls tc WHERE tc.session_id=?"+candidates.toolInput.sql, append([]any{"tools"}, candidates.toolInput.args...)...)
	require.NoError(t, err)
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Contains(t, plan.String(), "VIRTUAL TABLE INDEX 0:M")
	indexed := substringHits(t, d, f)
	require.Len(t, indexed, 3)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET input_json='changedtext', result_content='changedtext' WHERE session_id='tools' AND input_json='RareNeedle input'`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_result_events SET content='changedtext' WHERE session_id='tools' AND content='RareNeedle event'`)
	require.NoError(t, err)
	changed := ContentSearchFilter{Pattern: "changedtext", Sources: []string{"tool_input", "tool_result"}, Limit: 50}
	candidates, err = d.substringCandidates(ctx, changed, []string{changed.Pattern})
	require.NoError(t, err)
	require.NotEmpty(t, candidates.toolInput.sql)
	require.NotEmpty(t, candidates.toolResult.sql)
	require.NotEmpty(t, candidates.toolEvent.sql)
	assert.Len(t, substringHits(t, d, changed), 3)
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM tool_result_events WHERE session_id='tools'`)
	require.NoError(t, err)
	updated := substringHits(t, d, changed)
	require.Len(t, updated, 2)
	_, err = d.getWriter().ExecContext(ctx, `DELETE FROM stats WHERE key=?`, toolSubstringStateKey)
	require.NoError(t, err)
	assert.Equal(t, updated, substringHits(t, d, changed))
}

func TestToolSubstringBuildScansUntilReady(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s", "proj", [][2]string{{"assistant", "running"}})
	countMessagesForSubstringIndex(t, d, "s")
	_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id, session_id, tool_name, category, input_json)
 SELECT id, session_id, 'Read', 'Read', 'unindexedneedle' FROM messages WHERE session_id='s'`)
	require.NoError(t, err)
	_, err = d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3)) INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json) SELECT m.id,m.session_id,'Read','Read','other' FROM messages m,n WHERE m.session_id='s'`)
	require.NoError(t, err)
	f := ContentSearchFilter{Pattern: "unindexedneedle", Sources: []string{"tool_input"}, Limit: 10}
	candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
	require.NoError(t, err)
	require.NotEmpty(t, candidates.toolInput.sql)
	dropToolSubstringIndexForTest(t, d)
	pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
	require.NoError(t, err)
	require.True(t, pending)
	snapshot, err := d.BeginMessageSnapshot(ctx)
	require.NoError(t, err)
	hits, _, err := exactContentRows(ctx, snapshot.tx, f, candidates, 0)
	require.NoError(t, err)
	require.NoError(t, snapshot.Close())
	require.Len(t, hits, 1)
	done, err := buildToolSubstringBatch(ctx, d.getWriter())
	require.NoError(t, err)
	require.False(t, done)
	_, err = d.getWriter().ExecContext(ctx, `UPDATE tool_calls SET input_json='updatedneedle' WHERE session_id='s' AND input_json='unindexedneedle'`)
	require.NoError(t, err)
	for !done {
		done, err = buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
	}
	assert.Empty(t, substringHits(t, d, f))
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "updatedneedle", Sources: []string{"tool_input"}, Limit: 10}), 1)
}

func TestToolSubstringCursorSurvivesAnotherHandleRebuild(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
	_, err := d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3),(4))
 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
 SELECT m.id,m.session_id,'Read','Read',CASE WHEN i=1 THEN ? ELSE 'later' END FROM messages m,n WHERE m.session_id='s'`, "earlyneedle"+strings.Repeat("x", 1<<20))
	require.NoError(t, err)
	other, err := OpenDaemonWithProgress(ctx, path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	dropToolSubstringIndexForTest(t, d)
	pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
	require.NoError(t, err)
	require.True(t, pending)
	done, err := buildToolSubstringBatch(ctx, d.getWriter())
	require.NoError(t, err)
	require.False(t, done)
	var progress string
	require.NoError(t, d.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&progress))
	firstProgress := progress
	pending, err = prepareToolSubstringIndex(ctx, other.getWriter())
	require.NoError(t, err)
	require.True(t, pending)
	require.NoError(t, other.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&progress))
	assert.Equal(t, firstProgress, progress)
	dropToolSubstringIndexForTest(t, other)
	pending, err = prepareToolSubstringIndex(ctx, other.getWriter())
	require.NoError(t, err)
	require.True(t, pending)
	for !done {
		done, err = buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
	}
	var matches int
	require.NoError(t, d.getReader().QueryRow(ctx, `SELECT count(*) FROM tool_input_trigram_fts WHERE tool_input_trigram_fts MATCH '"ear" AND "arl"'`).Scan(&matches))
	assert.Equal(t, 1, matches)
	assert.True(t, hasToolSubstringIndex(ctx, d.getReader()))
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "earlyneedle", Sources: []string{"tool_input"}, Limit: 10}), 1)
}

func TestToolSubstringConcurrentBuilds(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
	_, err := d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<20000)
 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
 SELECT m.id,m.session_id,'Read','Read',CASE WHEN i=1 THEN 'earlyneedle' ELSE 'other' END FROM messages m,n WHERE m.session_id='s'`)
	require.NoError(t, err)
	other, err := OpenDaemonWithProgress(ctx, path, config.ArchiveContentFull, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	dropToolSubstringIndexForTest(t, d)
	pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
	require.NoError(t, err)
	require.True(t, pending)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, store := range []*DB{d, other} {
		go func() {
			<-start
			if _, err := prepareToolSubstringIndex(ctx, store.getWriter()); err != nil {
				errs <- err
				return
			}
			for {
				done, err := buildToolSubstringBatch(ctx, store.getWriter())
				if err != nil || done {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	for range 2 {
		assert.NoError(t, <-errs)
	}
	assert.True(t, hasToolSubstringIndex(ctx, d.getReader()))
	assert.Len(t, substringHits(t, d, ContentSearchFilter{Pattern: "earlyneedle", Sources: []string{"tool_input"}, Limit: 10}), 1)
}

func TestToolSubstringUnknownVersionsRemainUntouched(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
	_, err := d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3),(4)) INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json) SELECT m.id,m.session_id,'Read','Read',CASE WHEN i=1 THEN 'futureword' ELSE 'other' END FROM messages m,n WHERE m.session_id='s'`)
	require.NoError(t, err)
	for _, state := range []string{"1", "2:ready", "2:0:7"} {
		t.Run(state, func(t *testing.T) {
			_, err := d.getWriter().ExecContext(ctx, "INSERT OR REPLACE INTO stats(key,value) VALUES(?,?)", toolSubstringStateKey, state)
			require.NoError(t, err)
			assert.False(t, hasToolSubstringIndex(ctx, d.getReader()))
			f := ContentSearchFilter{Pattern: "futureword", Sources: []string{"tool_input"}, Limit: 10}
			candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
			require.NoError(t, err)
			assert.Empty(t, candidates.toolInput.sql)
			assert.Len(t, substringHits(t, d, f), 1)
			pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
			require.NoError(t, err)
			assert.False(t, pending)
		})
	}
}

func TestToolSubstringSelectivity(t *testing.T) {
	t.Run("scope and fields", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seedSearchSession(t, d, "local", "local", [][2]string{{"assistant", "running"}})
		seedSearchSession(t, d, "remote", "remote", [][2]string{{"assistant", "running"}})
		countMessagesForSubstringIndex(t, d, "local")
		countMessagesForSubstringIndex(t, d, "remote")
		_, err := d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1025)
	 INSERT INTO tool_calls(message_id, session_id, tool_name, category, input_json, result_content)
	 SELECT m.id, m.session_id, 'Read', 'Read', 'scopeword', 'scopeword' FROM messages m, n WHERE m.session_id='remote'`)
		require.NoError(t, err)
		_, err = d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<30)
	 INSERT INTO tool_calls(message_id, session_id, tool_name, category, input_json, result_content)
	 SELECT m.id, m.session_id, 'Read', 'Read', CASE WHEN i=1 THEN 'scopeword' ELSE 'other' END, 'scopeword' FROM messages m, n WHERE m.session_id='local'`)
		require.NoError(t, err)
		f := ContentSearchFilter{Pattern: "scopeword", Project: "local", Sources: []string{"tool_input", "tool_result"}, Limit: 50}
		candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
		require.NoError(t, err)
		assert.Empty(t, candidates.toolInput.sql)
		assert.Empty(t, candidates.toolResult.sql)
		archive := f
		archive.Project = ""
		archiveCandidates, err := d.substringCandidates(ctx, archive, []string{f.Pattern})
		require.NoError(t, err)
		assert.Empty(t, archiveCandidates.toolInput.sql)
		excluded := archive
		excluded.ExcludeProject = "remote"
		excludedCandidates, err := d.substringCandidates(ctx, excluded, []string{f.Pattern})
		require.NoError(t, err)
		assert.Empty(t, excludedCandidates.toolInput.sql)
		indexed := substringHits(t, d, f)
		assert.Equal(t, indexed, substringHits(t, d, excluded))
		require.Len(t, indexed, 31)
	})
	t.Run("index within project", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seedSearchSession(t, d, "local", "local", [][2]string{{"assistant", "running"}})
		seedSearchSession(t, d, "remote", "remote", [][2]string{{"assistant", "running"}})
		_, err := d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<12000)
	 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
	 SELECT m.id,m.session_id,'Read','Read',CASE WHEN n.i=1 THEN 'scopedneedle' ELSE 'other' END FROM messages m,n WHERE m.session_id='local'`)
		require.NoError(t, err)
		_, err = d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1025)
	 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
	 SELECT m.id,m.session_id,'Read','Read','scopedneedle' FROM messages m,n WHERE m.session_id='remote'`)
		require.NoError(t, err)
		f := ContentSearchFilter{Pattern: "scopedneedle", Project: "local", Sources: []string{"tool_input"}, Limit: 20}
		candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
		require.NoError(t, err)
		require.NotEmpty(t, candidates.toolInput.sql)
		indexed := substringHits(t, d, f)
		require.Len(t, indexed, 1)
		_, err = d.getWriter().ExecContext(ctx, "DELETE FROM stats WHERE key=?", toolSubstringStateKey)
		require.NoError(t, err)
		assert.Equal(t, indexed, substringHits(t, d, f))
	})
	for _, tc := range []struct {
		name       string
		remoteRows int
		allMatch   bool
	}{
		{"small scope scans global matches", 30000, true},
		{"scoped share differs from global", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			seedSearchSession(t, d, "local", "local", [][2]string{{"assistant", "running"}})
			seedSearchSession(t, d, "remote", "remote", [][2]string{{"assistant", "running"}})
			_, err := d.getWriter().ExecContext(ctx, `WITH n(i) AS (VALUES(1),(2),(3),(4))
	 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
	 SELECT m.id,m.session_id,'Read','Read',CASE WHEN n.i=1 THEN 'scopedneedle' ELSE 'other' END FROM messages m,n WHERE m.session_id='local'`)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
	 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
	 SELECT m.id,m.session_id,'Read','Read',CASE WHEN i=1 OR ? THEN 'scopedneedle' ELSE 'other' END FROM messages m,n WHERE m.session_id='remote'`, tc.remoteRows, tc.allMatch)
			require.NoError(t, err)
			f := ContentSearchFilter{Pattern: "scopedneedle", Project: "local", Sources: []string{"tool_input"}, Limit: 20}
			candidates, err := d.substringCandidates(ctx, f, []string{f.Pattern})
			require.NoError(t, err)
			assert.Empty(t, candidates.toolInput.sql)
			page, err := d.SearchContent(ctx, f)
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Equal(t, "tool_input", page.Matches[0].Location)
		})
	}
}

func TestToolSubstringBuildBatchBounds(t *testing.T) {
	t.Run("large row", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seedSearchSession(t, d, "s", "proj", [][2]string{{"assistant", "running"}})
		_, err := d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id, session_id, tool_name, category, input_json)
	 SELECT id, session_id, 'Read', 'Read', ? FROM messages WHERE session_id='s'`, "small")
		require.NoError(t, err)
		_, err = d.getWriter().ExecContext(ctx, `INSERT INTO tool_calls(message_id, session_id, tool_name, category, input_json)
	 SELECT id, session_id, 'Read', 'Read', ? FROM messages WHERE session_id='s'`, strings.Repeat("x", (1<<20)+1))
		require.NoError(t, err)
		dropToolSubstringIndexForTest(t, d)
		pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
		require.NoError(t, err)
		require.True(t, pending)
		done, err := buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
		require.False(t, done)
		var cursor string
		require.NoError(t, d.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&cursor))
		assert.Equal(t, "1:0:1", cursor)
		done, err = buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
		require.False(t, done)
		require.NoError(t, d.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&cursor))
		assert.Equal(t, "1:0:2", cursor)
	})
	t.Run("empty rows", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seedSearchSession(t, d, "s", "project", [][2]string{{"assistant", "running"}})
		_, err := d.getWriter().ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
	 INSERT INTO tool_calls(message_id,session_id,tool_name,category,input_json)
	 SELECT m.id,m.session_id,'Read','Read',CASE WHEN i%2=0 THEN NULL ELSE '' END FROM messages m,n WHERE m.session_id='s'`, toolSubstringBatchRows+1)
		require.NoError(t, err)
		dropToolSubstringIndexForTest(t, d)
		pending, err := prepareToolSubstringIndex(ctx, d.getWriter())
		require.NoError(t, err)
		require.True(t, pending)
		done, err := buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
		require.False(t, done)
		var cursor string
		require.NoError(t, d.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&cursor))
		assert.Equal(t, fmt.Sprintf("1:0:%d", toolSubstringBatchRows), cursor)
		done, err = buildToolSubstringBatch(ctx, d.getWriter())
		require.NoError(t, err)
		require.False(t, done)
		require.NoError(t, d.getReader().QueryRow(ctx, "SELECT CAST(value AS TEXT) FROM stats WHERE key=?", toolSubstringStateKey).Scan(&cursor))
		assert.Equal(t, fmt.Sprintf("1:0:%d", toolSubstringBatchRows+1), cursor)
	})
}
