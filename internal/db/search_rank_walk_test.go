package db

import (
	"fmt"
	"math"
	"strings"
	"testing"

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

	walk, err := walkSearchRanks(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"handler"`}, 2, math.MinInt64)
	require.NoError(t, err)
	require.NotNil(t, walk, "the walk must serve a page with distinct ranks")
	assert.False(t, walk.complete)

	for _, query := range []string{"handler", "deploy"} {
		for _, limit := range []int{1, 2, 7, 50} {
			var walked, ranked []SearchResult
			for cursor := 0; ; cursor += limit {
				got, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor})
				require.NoError(t, err)
				want, err := d.Search(ctx, SearchFilter{Query: query, Limit: limit, Cursor: cursor, Project: "proj"})
				require.NoError(t, err)
				require.Equal(t, want, got, "%s limit %d cursor %d", query, limit, cursor)
				walked = append(walked, got.Results...)
				ranked = append(ranked, want.Results...)
				if got.NextCursor == 0 {
					break
				}
			}
			assert.Equal(t, ranked, walked)
			if query == "handler" {
				require.Len(t, walked, 116, "limit %d", limit)
				assert.Equal(t, -1, walked[len(walked)-1].Ordinal, "title-only rows come last")
			}
		}
	}
}

func TestSearchRankWalkTiers(t *testing.T) {
	original := searchTierMatches
	searchTierMatches = 4
	t.Cleanup(func() { searchTierMatches = original })
	d := testDB(t)
	ctx := t.Context()
	for i := range 7 {
		id := fmt.Sprintf("session-%d", i)
		insertSession(t, d, id, "proj")
		content := "needle " + strings.Repeat("filler ", 20)
		if i < 2 {
			content = strings.Repeat("needle ", 20)
		}
		insertMessages(t, d, userMsg(id, 0, content))
	}
	insertSession(t, d, "title", "proj", func(s *Session) { s.FirstMessage = new("needle title") })
	insertMessages(t, d, userMsg("title", 0, "other"))
	for _, limit := range []int{1, 2, 7} {
		var results []SearchResult
		for cursor := 0; ; cursor += limit {
			page, err := d.Search(ctx, SearchFilter{Query: "needle", Limit: limit, Cursor: cursor})
			require.NoError(t, err)
			results = append(results, page.Results...)
			if page.NextCursor == 0 {
				break
			}
		}
		require.Len(t, results, 8)
		seen := map[string]bool{}
		for i, result := range results {
			assert.False(t, seen[result.SessionID])
			seen[result.SessionID] = true
			if i < 4 {
				assert.Contains(t, []string{"session-3", "session-4", "session-5", "session-6"}, result.SessionID)
			}
		}
		assert.Equal(t, "title", results[7].SessionID)
	}
	walk, err := walkSearchRanks(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"needle"`}, 1000000, 1)
	require.NoError(t, err)
	assert.Nil(t, walk)
}

func TestSearchRankWalkConcentratedTier(t *testing.T) {
	original := searchTierMatches
	searchTierMatches = 4
	t.Cleanup(func() { searchTierMatches = original })
	d := testDB(t)
	insertSession(t, d, "older", "proj")
	insertMessages(t, d, userMsg("older", 0, "needle"))
	insertSession(t, d, "recent", "proj")
	for i := range 5 {
		insertMessages(t, d, userMsg("recent", i, "needle"))
	}
	page, err := d.Search(t.Context(), SearchFilter{Query: "needle", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 2)
	assert.Equal(t, "recent", page.Results[0].SessionID)
	assert.Equal(t, "older", page.Results[1].SessionID)
}

func TestSearchRankWalkTierBoundary(t *testing.T) {
	original := searchTierMatches
	searchTierMatches = 4
	t.Cleanup(func() { searchTierMatches = original })
	d := testDB(t)
	insertSession(t, d, "session", "proj")
	for i := range 5 {
		insertMessages(t, d, userMsg("session", i, "needle"))
		floor, err := searchTierFloor(t.Context(), d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"needle"`})
		require.NoError(t, err)
		if i < 4 {
			assert.Equal(t, int64(math.MinInt64), floor)
		} else {
			assert.Greater(t, floor, int64(math.MinInt64))
		}
	}
}

func TestSearchRankWalkDeepPage(t *testing.T) {
	original := searchTierMatches
	searchTierMatches = 4
	t.Cleanup(func() { searchTierMatches = original })
	d := testDB(t)
	_, err := d.getWriter().ExecContext(t.Context(), `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<4104)
		INSERT INTO sessions(id, project) SELECT printf('s%04d', i), 'proj' FROM n;
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<4104)
		INSERT INTO messages(session_id, ordinal, role, content) SELECT printf('s%04d', i), 0, 'user', 'needle' FROM n;`)
	require.NoError(t, err)
	page, err := d.Search(t.Context(), SearchFilter{Query: "needle", Cursor: 4096, Limit: 7})
	require.NoError(t, err)
	require.Len(t, page.Results, 7)
	for i, result := range page.Results {
		assert.Equal(t, fmt.Sprintf("s%04d", 4092+i), result.SessionID)
	}
	assert.Equal(t, 4103, page.NextCursor)
}

func TestSearchRankWalkImageTier(t *testing.T) {
	original := searchTierMatches
	searchTierMatches = 2
	t.Cleanup(func() { searchTierMatches = original })
	d := testDB(t)
	insertSession(t, d, "image", "proj")
	insertMessages(t, d, userMsg("image", 0, "other"))
	_, err := d.getWriter().ExecContext(t.Context(), `
		INSERT INTO tool_calls(message_id,session_id,tool_name,category)
		SELECT id,'image','tool','other' FROM messages WHERE session_id='image';
		INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,text,succeeded)
		VALUES(-1,'hash','processor','provider','model','revision','needle needle needle',1);
		INSERT INTO image_text_occurrences(session_id,call_id,image_index,image_hash)
		SELECT 'image',id,0,'hash' FROM tool_calls WHERE session_id='image';`)
	require.NoError(t, err)
	for i := range 3 {
		id := fmt.Sprintf("message-%d", i)
		insertSession(t, d, id, "proj")
		insertMessages(t, d, userMsg(id, 0, "needle"))
	}
	page, err := d.Search(t.Context(), SearchFilter{Query: "needle", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	assert.Contains(t, []string{"message-1", "message-2"}, page.Results[0].SessionID)
	page, err = d.Search(t.Context(), SearchFilter{Query: "needle", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 4)
	assert.Contains(t, []string{page.Results[2].SessionID, page.Results[3].SessionID}, "image")
}
