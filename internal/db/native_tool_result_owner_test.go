package db

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExistingToolResultOwnersStaySessionScopedAndIndexed(t *testing.T) {
	for _, cardinality := range []int{1, 1000} {
		t.Run(strconv.Itoa(cardinality), func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "target", "project")
			insertSession(t, d, "other", "project")
			require.NoError(t, d.InsertMessages(t.Context(), []Message{{
				SessionID: "target", Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ToolUseID: "known"}},
			}}))
			_, err := d.getWriter().ExecContext(t.Context(), `WITH RECURSIVE ordinals(n) AS (
				SELECT 0 UNION ALL SELECT n+1 FROM ordinals WHERE n+1 < ?
			) INSERT INTO messages(session_id, ordinal, role, content)
			SELECT 'other', n, 'assistant', '' FROM ordinals`, cardinality)
			require.NoError(t, err)
			_, err = d.getWriter().ExecContext(t.Context(), `INSERT INTO tool_calls(message_id, session_id, tool_name, category, tool_use_id)
			SELECT id, session_id, 'Read', 'Read', 'other-' || ordinal FROM messages WHERE session_id='other'`)
			require.NoError(t, err)
			owners, err := d.ExistingToolResultOwners(t.Context(), "target", []string{"known", "other-0", "missing", "known", ""})
			require.NoError(t, err)
			assert.Equal(t, map[string]bool{"known": true, "other-0": false, "missing": false}, owners)

			// Protect our indexed SQL boundary as cold archive cardinality grows.
			rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+existingToolResultOwnerSQL, "target", "known")
			require.NoError(t, err)
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			assert.Contains(t, strings.Join(details, "\n"), "SEARCH tc USING INDEX idx_tool_calls_session_tool_use (session_id=? AND tool_use_id=?)")
		})
	}
}
