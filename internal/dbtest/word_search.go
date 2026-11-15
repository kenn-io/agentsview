package dbtest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// SeedWordSearch creates exact, partial, and nonmatching search results.
func SeedWordSearch(t *testing.T, d *db.DB) {
	t.Helper()
	for i, entry := range []struct{ id, text, result string }{
		{"exact", "invoice rounding decision", ""},
		{"two", "rounding of invoices", ""},
		{"phrase", "quarterly; forecast arrived", ""},
		{"case", "ORBIT LEDGER SWIFT", ""},
		{"one", "an invoice arrived", ""},
		{"near", "an xinvoice arrived", ""},
		{"other", "nothing relevant", ""},
		{"tool", "ran the report", "invoice rounding decision recorded"},
	} {
		ended := fmt.Sprintf("2026-05-20T12:00:%02dZ", 59-i)
		SeedSession(t, d, entry.id, "word-search", func(s *db.Session) {
			s.EndedAt = &ended
			s.UserMessageCount = 2
		})
		m := db.Message{SessionID: entry.id, Role: "assistant", Content: entry.text, Timestamp: ended}
		if entry.result != "" {
			m.ToolCalls = []db.ToolCall{{ToolName: "Bash", Category: "Bash", ToolUseID: "word-tool", ResultContent: entry.result}}
		}
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{m}))
	}
	for _, entry := range []struct{ id, project, text string }{
		{"exact-long-snippet", "word-exact-snippet", "invoice " + strings.Repeat("filler ", 80) + "invoice rounding decision"},
		{"invoice-dot", "word-punctuation", "invoice.txt"},
		{"invoice-path", "word-punctuation", "/tmp/invoice"},
		{"invoice-email", "word-punctuation", "a@invoice.example"},
		{"invoice-version", "word-punctuation", "v2.invoice"},
		{"split-punctuation", "word-split-punctuation", strings.Repeat("filler ", 80) + "invoice txt"},
		{"cjk-prefix", "word-cjk-boundary", "漢invoice"},
		{"cjk-suffix", "word-cjk-boundary", "invoice漢"},
		{"star-both", "word-star", "invoice* rounding"},
		{"star-partial", "word-star", "rounding"},
		{"literal-star", "word-literal-star", "invoice*"},
		{"plain-invoice", "word-literal-star", "invoice"},
		{"literal-not", "word-literal-not", `NOT "invoice"`},
		{"plain-not-invoice", "word-literal-not", "invoice"},
	} {
		SeedSession(t, d, entry.id, entry.project, func(s *db.Session) { s.UserMessageCount = 2 })
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{{
			SessionID: entry.id, Role: "assistant", Content: entry.text,
		}}))
	}
	// Newest first, so the sessions whose boundary fails the all-word pass
	// precede the whole-word matches in recency order.
	for i, entry := range []struct{ id, text string }{
		{"page-cjk-prefix", "漢invoice and rounding"},
		{"page-cjk-suffix", "invoice漢 and rounding"},
		{"page-inflected", "we settled the rounding of invoices"},
		{"page-underscore", "invoice_and_rounding"},
		{"page-reordered", "rounding for each invoice"},
	} {
		ended := fmt.Sprintf("2026-05-21T12:00:%02dZ", 59-i)
		SeedSession(t, d, entry.id, "word-page", func(s *db.Session) {
			s.EndedAt = &ended
			s.UserMessageCount = 2
		})
		require.NoError(t, d.ReplaceSessionMessages(t.Context(), entry.id, []db.Message{{
			SessionID: entry.id, Role: "assistant", Content: entry.text, Timestamp: ended,
		}}))
	}
	SeedSession(t, d, "underscore", "word-boundary", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "underscore", []db.Message{{SessionID: "underscore", Role: "assistant", Content: "invoice_rounding_decision"}}))
	SeedSession(t, d, "boundary-near", "word-boundary", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "boundary-near", []db.Message{{SessionID: "boundary-near", Role: "assistant", Content: "xinvoice rounding decisionx"}}))
	SeedSession(t, d, "multiline", "word-multiline", func(s *db.Session) { s.UserMessageCount = 2 })
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "multiline", []db.Message{{SessionID: "multiline", Role: "assistant", Content: "decision\ninvoice_rounding"}}))
	SeedSession(t, d, "long", "word-snippet", func(s *db.Session) { s.UserMessageCount = 2 })
	body := strings.Repeat("filler ", 80) + "invoices " + strings.Repeat("filler ", 80)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "long", []db.Message{{SessionID: "long", Role: "assistant", Content: body}}))
}

// AssertExplicitMirrorFTS checks the mirrors' existing literal expression handling.
func AssertExplicitMirrorFTS(t *testing.T, store db.Store, modes ...string) {
	t.Helper()
	if len(modes) == 0 {
		modes = []string{"fts", "hybrid"}
	}
	for _, tc := range []struct{ pattern, project, want string }{
		{"invoice*", "word-literal-star", "literal-star"},
		{"invoice* rounding", "word-star", "star-both"},
		{`NOT "invoice"`, "word-literal-not", "literal-not"},
	} {
		for _, mode := range modes {
			t.Run("explicit fts/"+tc.pattern+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					Pattern: tc.pattern, Project: tc.project, Mode: mode, KeywordOnly: mode == "hybrid",
				})
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				assert.Equal(t, tc.want, page.Matches[0].SessionID)
			})
		}
	}
}

// AssertWordSearch checks the user-visible ranking and paging contract.
func AssertWordSearch(t *testing.T, store db.Store) {
	t.Helper()
	type hit struct {
		session, location string
		words             int
	}
	project := func(matches []db.ContentMatch) []hit {
		var hits []hit
		for _, m := range matches {
			hits = append(hits, hit{m.SessionID, m.Location, m.MatchedWords})
		}
		return hits
	}
	for _, mode := range []string{"fts", "hybrid"} {
		t.Run("single word matches whole words/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice", Mode: mode, Project: "word-search", KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"exact", "message", 1}, {"two", "message", 1}, {"one", "message", 1}}, project(page.Matches))
		})
		t.Run("stop word leaves one searchable word/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice the", Mode: mode, Project: "word-search", KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"exact", "message", 1}, {"two", "message", 1}, {"one", "message", 1}}, project(page.Matches))
		})
	}
	for _, tc := range []struct {
		name, pattern, project string
		want                   []hit
		snippet                string
	}{
		{"punctuation splits query words", "invoice.txt missingword", "word-split-punctuation", []hit{{"split-punctuation", "message", 1}}, "invoice txt"},
		{"multiline", "invoice rounding decision", "word-multiline", []hit{{"multiline", "message", 3}}, ""},
	} {
		for _, mode := range []string{"substring", "fts", "hybrid"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					Pattern: tc.pattern, Mode: mode, Project: tc.project,
					KeywordOnly: mode == "hybrid",
				})
				require.NoError(t, err)
				assert.Equal(t, tc.want, project(page.Matches))
				if tc.snippet != "" {
					require.Len(t, page.Matches, 1)
					assert.Contains(t, page.Matches[0].Snippet, tc.snippet)
				}
			})
		}
	}
	for _, tc := range []struct {
		name, project string
		want          []hit
	}{
		{"CJK letters are word characters", "word-cjk-boundary", nil},
		{"partial punctuation candidates keep word boundaries", "word-punctuation", []hit{
			{"invoice-dot", "message", 1},
			{"invoice-path", "message", 1},
			{"invoice-email", "message", 1},
			{"invoice-version", "message", 1},
		}},
	} {
		for _, mode := range []string{"substring", "fts", "hybrid"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					Pattern: "invoice absentword", Mode: mode, Project: tc.project,
					KeywordOnly: mode == "hybrid",
				})
				require.NoError(t, err)
				assert.ElementsMatch(t, tc.want, project(page.Matches))
			})
		}
	}

	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("exact phrase snippet/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "invoice rounding decision", Mode: mode, Project: "word-exact-snippet",
				KeywordOnly: mode == "hybrid",
			})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, "invoice rounding decision")
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("underscore/"+mode, func(t *testing.T) {
			phrase, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: `"invoice rounding" decision`, Mode: mode, Project: "word-boundary", KeywordOnly: mode == "hybrid"})
			require.NoError(t, err)
			assert.Equal(t, []hit{{"underscore", "message", 3}}, project(phrase.Matches))
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice rounding decision", Mode: mode, Project: "word-boundary", KeywordOnly: mode == "hybrid"})
			require.NoError(t, err)
			want := []hit{{"underscore", "message", 3}, {"boundary-near", "message", 1}}
			if mode == "substring" {
				want = []hit{{"boundary-near", "message", 3}, {"underscore", "message", 3}}
			}
			assert.Equal(t, want, project(page.Matches))
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run("snippet/"+mode, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "invoice missing", Mode: mode, Project: "word-snippet", KeywordOnly: mode == "hybrid"})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Contains(t, page.Matches[0].Snippet, "invoices")
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			f := db.ContentSearchFilter{Pattern: "invoice rounding decision", Mode: mode, Project: "word-search", Limit: 50, KeywordOnly: mode == "hybrid"}
			page, err := store.SearchContent(t.Context(), f)
			require.NoError(t, err)
			want := []hit{{"exact", "message", 3}, {"two", "message", 2}, {"one", "message", 1}}
			if mode == "substring" {
				want = []hit{{"exact", "message", 3}, {"tool", "tool_result", 3}, {"two", "message", 2}, {"one", "message", 1}}
			}
			assert.Equal(t, want, project(page.Matches))
			if mode == "hybrid" {
				return
			}
			f.Limit = 1
			var paged []hit
			for range 5 {
				page, err = store.SearchContent(t.Context(), f)
				require.NoError(t, err)
				paged = append(paged, project(page.Matches)...)
				if page.NextCursor == 0 {
					break
				}
				f.Cursor = page.NextCursor
			}
			assert.Equal(t, want, paged)
		})
	}
	for _, mode := range []string{"substring", "fts", "hybrid"} {
		name := "one-row pages read past candidates that fail a word boundary"
		if mode == "hybrid" {
			name = "candidates that fail a word boundary rank last"
		}
		t.Run(name+"/"+mode, func(t *testing.T) {
			f := db.ContentSearchFilter{
				Pattern: "invoice rounding", Mode: mode, Project: "word-page",
				Limit: 1, KeywordOnly: mode == "hybrid",
			}
			var paged []hit
			if mode == "hybrid" {
				f.Limit = 50
			}
			for range 10 {
				page, err := store.SearchContent(t.Context(), f)
				require.NoError(t, err)
				paged = append(paged, project(page.Matches)...)
				if page.NextCursor == 0 {
					break
				}
				f.Cursor = page.NextCursor
			}
			// Every whole-word match comes before the two rows that hold
			// only "rounding"; ranking among equal counts differs by mode.
			want := []hit{
				{"page-inflected", "message", 2},
				{"page-underscore", "message", 2},
				{"page-reordered", "message", 2},
				{"page-cjk-prefix", "message", 1},
				{"page-cjk-suffix", "message", 1},
			}
			if mode == "substring" {
				assert.Equal(t, want, paged)
			} else {
				require.Len(t, paged, 5)
				for _, h := range paged[:3] {
					assert.Equal(t, 2, h.words, h.session)
				}
				assert.ElementsMatch(t, want, paged)
			}
		})
	}
	for _, tc := range []struct {
		pattern string
		want    []hit
	}{
		{`"rounding of invoices"`, []hit{{"two", "message", 0}}},
		{`policy "quarterly forecast"`, []hit{{"phrase", "message", 2}}},
		{`policy "forecast quarterly"`, nil},
		{`policy "quarterly missing"`, nil},
	} {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Project: "word-search"})
		require.NoError(t, err)
		assert.Equal(t, tc.want, project(page.Matches), tc.pattern)
	}
	for _, tc := range []struct {
		name   string
		filter db.ContentSearchFilter
		want   []hit
	}{
		{
			name:   "single word remains substring search",
			filter: db.ContentSearchFilter{Pattern: "invoice", Project: "word-search", Sources: []string{"messages"}},
			want:   []hit{{"exact", "message", 0}, {"two", "message", 0}, {"one", "message", 0}, {"near", "message", 0}},
		},
		{
			name:   "word matching folds case",
			filter: db.ContentSearchFilter{Pattern: "orbit ledger swift", Mode: "fts", Project: "word-search"},
			want:   []hit{{"case", "message", 3}},
		},
		{
			name:   "non-message sources stay exact",
			filter: db.ContentSearchFilter{Pattern: "invoice rounding decision", Project: "word-search", Sources: []string{"tool_result"}},
			want:   []hit{{"tool", "tool_result", 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := store.SearchContent(t.Context(), tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, project(page.Matches))
		})
	}
}
