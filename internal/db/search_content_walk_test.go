package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Substring, word, and regex search read sessions in batches, newest first.
// Each session here claims 20,000 messages, so the walk reads session a
// alone, then b and c, then d and e. Sessions a and b end at the same time,
// so the first batch boundary falls between rows whose recency ties. Session
// a holds exactly the first page's two matches, so the walk must read past it
// to find the row that shows another page follows. Paging two rows at a time
// stops some walks early and runs others to the end, and every page must
// continue the order of the one before.
func TestSearchContentWalksSessionBatchesInOrder(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	seed := func(id, endedAt string, msgs []Message) {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "claude"
			s.UserMessageCount = 2
			s.StartedAt = new(endedAt)
			s.EndedAt = new(endedAt)
		})
		for i := range msgs {
			msgs[i].SessionID = id
			msgs[i].Ordinal = i
		}
		require.NoError(t, d.ReplaceSessionMessages(ctx, id, msgs))
		_, err := d.getWriter().Exec(ctx, `UPDATE sessions SET message_count = 20000 WHERE id = ?`, id)
		require.NoError(t, err)
	}
	seed("a", "2026-05-20T10:00:00Z", []Message{
		{Role: "user", Content: "check the deploy window"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu1",
			InputJSON: `{"cmd":"deploy window"}`, ResultContent: "done",
		}}},
	})
	seed("b", "2026-05-20T10:00:00Z", []Message{{Role: "user", Content: "deploy window again"}})
	seed("c", "2026-05-20T09:00:00Z", []Message{
		{Role: "assistant", Content: "nothing", ThinkingText: "the deploy window closes"},
		{Role: "user", Content: "window, then deploy"},
	})
	seed("d", "2026-05-20T08:00:00Z", []Message{
		{Role: "user", Content: "Deploy Window caps"},
		{Role: "assistant", Content: "ok", ToolCalls: []ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "tu2",
			InputJSON: `{"cmd":"close"}`, ResultContent: "deploy window shut",
		}}},
	})
	seed("e", "2026-05-20T07:00:00Z", []Message{{Role: "user", Content: "deploy the window later"}})

	exact := []string{
		"a/message/0", "a/tool_input/1", "b/message/0",
		"c/thinking/0", "d/message/0", "d/tool_result/1",
	}
	for _, tc := range []struct {
		name    string
		pattern string
		mode    string
		want    []string
	}{
		{"quoted substring", `"deploy window"`, "substring", exact},
		{
			"words after exact matches", "deploy window", "substring",
			append(append([]string(nil), exact...), "c/message/1", "e/message/0"),
		},
		{"regex", `deploy\s+window`, "regex", []string{
			"a/message/0", "a/tool_input/1", "b/message/0",
			"c/thinking/0", "d/tool_result/1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for cursor := 0; ; {
				page, err := d.SearchContent(ctx, ContentSearchFilter{
					Pattern: tc.pattern, Mode: tc.mode, Limit: 2, Cursor: cursor,
				})
				require.NoError(t, err)
				for _, m := range page.Matches {
					got = append(got, fmt.Sprintf("%s/%s/%d", m.SessionID, m.Location, m.Ordinal))
				}
				if page.NextCursor == 0 {
					break
				}
				require.Equal(t, cursor+2, page.NextCursor)
				cursor = page.NextCursor
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
