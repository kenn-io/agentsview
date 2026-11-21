package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rank walk must return the pages that ranking every match returns.
// Project-filtered searches always rank every match, so a single-project
// archive compares the two paths on the same data: sessions that tie on
// rank with each other and between their own messages, sessions with
// distinct ranks, enough ties to pass the walk's row bound, and title-only
// matches that follow every message match.
func TestSearchRankWalkMatchesFullRanking(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seed := func(id, title string, contents ...string) {
		t.Helper()
		insertSession(t, d, id, "proj", func(s *Session) {
			s.FirstMessage = new(title)
			s.EndedAt = new(fmt.Sprintf("2026-05-%02dT12:00:00Z", 1+len(id)%28))
		})
		var msgs []Message
		for i, content := range contents {
			msgs = append(msgs, userMsg(id, i, content))
		}
		insertMessages(t, d, msgs...)
	}
	for i := range 100 {
		seed(fmt.Sprintf("tie-%03d", i), "untitled", "intro", "deploy the handler", "middle", "deploy the handler")
	}
	for i := range 10 {
		seed(fmt.Sprintf("varied-%02d", i), "untitled",
			"handler "+strings.Repeat("word ", i), strings.Repeat("filler ", 3)+"handler")
	}
	for i := range 3 {
		seed(fmt.Sprintf("title-%d", i), "the handler title", "nothing relevant")
	}
	// Equal ranks resolve to the lowest ordinal even when that message was
	// stored, and so read, after its tie.
	for i := range 3 {
		id := fmt.Sprintf("late-%d", i)
		insertSession(t, d, id, "proj", func(s *Session) { s.FirstMessage = new("untitled") })
		insertMessages(t, d, userMsg(id, 5, "handler"))
		insertMessages(t, d, userMsg(id, 2, "handler"))
	}

	walk, err := walkSearchRanks(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"handler"`}, 2)
	require.NoError(t, err)
	require.NotNil(t, walk, "the walk must serve a page with distinct ranks")
	assert.False(t, walk.complete)

	// Fresh readers prevent cached statements from hiding which ranking query ran.
	observe := func(filter SearchFilter) (SearchPage, int) {
		t.Helper()
		reader, err := OpenReadOnly(ctx, d.Path())
		require.NoError(t, err)
		defer func() { require.NoError(t, reader.Close()) }()
		reader.rawReader().SetMaxOpenConns(1)
		conn, err := reader.getReader().Conn(ctx)
		require.NoError(t, err)
		var rankingWindows int
		require.NoError(t, conn.Raw(func(raw any) error {
			raw.(interface {
				RegisterAuthorizer(func(int, string, string, string) int)
			}).RegisterAuthorizer(func(op int, _, arg2, _ string) int {
				if op == sqlite3.SQLITE_FUNCTION && arg2 == "row_number" {
					rankingWindows++
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}))
		require.NoError(t, conn.Close())
		page, err := reader.Search(ctx, filter)
		require.NoError(t, err)
		return page, rankingWindows
	}
	filter := SearchFilter{Query: "handler", DateFrom: "2026-05-10", Limit: 1, Project: "proj"}
	full, fullWindows := observe(filter)
	filter.Project = ""
	page, walkWindows := observe(filter)
	require.Equal(t, full, page)
	require.Len(t, page.Results, 1)
	assert.Less(t, walkWindows, fullWindows, "dated relevance search must use the rank walk")

	for _, from := range []string{"", "2026-05-08", "2026-05-10", "2027-01-01"} {
		for _, query := range []string{"handler", "deploy"} {
			for _, limit := range []int{1, 2, 7, 50} {
				var walked, ranked []SearchResult
				for cursor := 0; ; cursor += limit {
					got, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor, DateFrom: from})
					require.NoError(t, err)
					want, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor, Project: "proj", DateFrom: from})
					require.NoError(t, err)
					require.Equal(t, want, got, "%s from %s limit %d cursor %d", query, from, limit, cursor)
					walked = append(walked, got.Results...)
					ranked = append(ranked, want.Results...)
					if got.NextCursor == 0 {
						break
					}
				}
				assert.Equal(t, ranked, walked)
				if from == "2027-01-01" {
					assert.Empty(t, walked)
				}
				if query == "handler" && from == "" {
					require.Len(t, walked, 116, "limit %d", limit)
					assert.Equal(t, -1, walked[len(walked)-1].Ordinal, "title-only rows come last")
				}
			}
		}
	}
}
