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
	d := testDB(t)
	ctx := t.Context()
	tx, err := d.getWriter().BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
	INSERT INTO sessions(id,project) VALUES ('session-0','proj'),('session-1','proj'),('session-2','proj'),('session-3','proj'),('session-4','proj'),('session-5','proj'),('session-6','proj'),('title','proj'),('boundary','proj'),('eligible','proj'),('eligible-old','proj'),('eligible-edge','proj'),('deleted','proj'),('image','proj'),('message-0','proj'),('message-1','proj'),('message-2','proj');
 UPDATE sessions SET first_message='tiers title' WHERE id='title';
 UPDATE sessions SET started_at='2026-06-15T00:00:00Z', ended_at='2026-06-15T01:00:00Z';
 UPDATE sessions SET deleted_at='2026-01-01' WHERE id='deleted';
 INSERT INTO messages(session_id,ordinal,role,content) VALUES
 ('session-0',0,'user','boundaryword tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers'),
 ('session-1',0,'user','boundaryword tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers tiers'),
 ('session-2',0,'user','boundaryword tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler');
 WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<65532)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT 'session-6',i,'user','boundaryword tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler' FROM n;
 INSERT INTO messages(session_id,ordinal,role,content) VALUES
 ('session-3',0,'user','boundaryword tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler'),
 ('session-4',0,'user','tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler'),
 ('session-5',0,'user','tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler'),
 ('session-6',0,'user','tiers filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler'),
 ('title',0,'user','other');
	INSERT INTO messages(session_id,ordinal,role,content) VALUES ('eligible-old',0,'user','eligibleword eligibleword eligibleword eligibleword eligibleword');
	WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<4)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT 'eligible',i,'user','eligibleword' FROM n;
	INSERT INTO messages(session_id,ordinal,role,content) VALUES ('eligible-edge',0,'user','eligibleword eligibleword eligibleword eligibleword eligibleword');
	WITH RECURSIVE n(i) AS (SELECT 5 UNION ALL SELECT i+1 FROM n WHERE i<65536)
	INSERT INTO messages(session_id,ordinal,role,content) SELECT 'eligible',i,'user','eligibleword' FROM n;
 WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<5)
 INSERT INTO messages(session_id,ordinal,role,content,is_system) SELECT CASE WHEN i<2 THEN 'deleted' ELSE 'eligible' END,65537+i,'user','eligibleword',CASE WHEN i>=2 THEN 1 ELSE 0 END FROM n;
 WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<4104)
 INSERT INTO sessions(id,project) SELECT printf('s%04d',i),'proj' FROM n;
 WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i<4100)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT printf('s%04d',i),0,'user','deepword' FROM n;
 WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<65532)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT 's4104',i,'user','deepword' FROM n;
 WITH RECURSIVE n(i) AS (SELECT 4101 UNION ALL SELECT i+1 FROM n WHERE i<4104)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT printf('s%04d',i),0,'user','deepword' FROM n;
 INSERT INTO messages(session_id,ordinal,role,content) VALUES ('image',0,'user','other'),('message-0',0,'user','imageword');
 WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<65534)
 INSERT INTO messages(session_id,ordinal,role,content) SELECT 'message-2',i,'user','imageword' FROM n;
 INSERT INTO messages(session_id,ordinal,role,content) VALUES ('message-1',0,'user','imageword'),('message-2',0,'user','imageword');
 INSERT INTO tool_calls(message_id,session_id,tool_name,category) SELECT id,'image','tool','other' FROM messages WHERE session_id='image';
 INSERT INTO image_text_cache(id,image_hash,processor,provider,model,revision,text,succeeded) VALUES(-1,'hash','processor','provider','model','revision','imageword imageword imageword',1);
 INSERT INTO image_text_occurrences(session_id,call_id,image_index,image_hash) SELECT 'image',id,0,'hash' FROM tool_calls WHERE session_id='image';`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	t.Run("pages", func(t *testing.T) {
		for _, limit := range []int{1, 2, 7} {
			var results []SearchResult
			for cursor := 0; ; cursor += limit {
				page, err := d.Search(ctx, SearchFilter{Query: "tiers", Limit: limit, Cursor: cursor})
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
	})
	t.Run("filtered tiers", func(t *testing.T) {
		for _, filter := range []SearchFilter{
			{Query: "tiers", Limit: 1, Project: "proj"},
			{Query: "tiers", Limit: 1, DateFrom: "2026-06-01", DateTo: "2026-06-30"},
		} {
			page, err := d.Search(ctx, filter)
			require.NoError(t, err)
			require.Len(t, page.Results, 1)
			assert.Contains(t, []string{"session-3", "session-4", "session-5", "session-6"}, page.Results[0].SessionID)
		}
	})
	t.Run("boundary", func(t *testing.T) {
		floor, err := searchTierFloor(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"boundaryword"`})
		require.NoError(t, err)
		assert.Equal(t, int64(math.MinInt64), floor)
		_, err = d.getWriter().ExecContext(ctx, `INSERT INTO messages(session_id,ordinal,role,content) VALUES ('boundary',65536,'user','boundaryword')`)
		require.NoError(t, err)
		floor, err = searchTierFloor(ctx, d.getReader(), "messages_fts", "messages_fts.rendered MATCH ?", []any{`"boundaryword"`})
		require.NoError(t, err)
		assert.Greater(t, floor, int64(math.MinInt64))
	})
	t.Run("eligible", func(t *testing.T) {
		var want int64
		require.NoError(t, d.getReader().QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id='eligible' AND ordinal=2`).Scan(&want))
		predicate := "messages_fts.rendered MATCH ? AND s2.deleted_at IS NULL AND m2.is_system = 0 AND " + SystemPrefixSQL("m2.content", "m2.role")
		floor, err := searchTierFloor(ctx, d.getReader(), "messages_fts", predicate, []any{`"eligibleword"`})
		require.NoError(t, err)
		assert.Equal(t, want, floor)
		page, err := d.Search(ctx, SearchFilter{Query: "eligibleword", Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, "eligible-edge", page.Results[0].SessionID)
	})
	t.Run("deep page", func(t *testing.T) {
		page, err := d.Search(ctx, SearchFilter{Query: "deepword", Cursor: 4096, Limit: 7})
		require.NoError(t, err)
		require.Len(t, page.Results, 7)
		for i, result := range page.Results {
			assert.Equal(t, fmt.Sprintf("s%04d", 4092+i), result.SessionID)
		}
		assert.Equal(t, 4103, page.NextCursor)
	})
	t.Run("image tier", func(t *testing.T) {
		page, err := d.Search(ctx, SearchFilter{Query: "imageword", Limit: 1})
		require.NoError(t, err)
		require.Len(t, page.Results, 1)
		assert.Equal(t, "message-1", page.Results[0].SessionID)
		page, err = d.Search(ctx, SearchFilter{Query: "imageword", Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Results, 4)
		var ids []string
		for _, result := range page.Results {
			ids = append(ids, result.SessionID)
		}
		assert.Equal(t, []string{"message-1", "message-2", "image", "message-0"}, ids)
	})
}
