//go:build pgtest

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

const issueReviewSchema = "agentsview_issue_review_test"

func setupIssueReviewStore(t *testing.T) *Store {
	t.Helper()
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, issueReviewSchema, true)
	require.NoError(t, err)
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + issueReviewSchema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(context.Background(), pg, issueReviewSchema))
	require.NoError(t, pg.Close())
	store, err := NewStore(pgURL, issueReviewSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestIssueReviewRowsConditionallyLoadsResultTail(t *testing.T) {
	store := setupIssueReviewStore(t)
	const sessionID = "issue-review-tail"
	failure := "Script failed\n" + strings.Repeat("progress output ", 200) + "\nParserError: stable tail failure"
	success := strings.Repeat("completed output ", 200) + "\nSUCCESS_TAIL_SENTINEL"
	_, err := store.DB().Exec(`
		INSERT INTO sessions (id,machine,project,agent,first_message,started_at,message_count,user_message_count)
		VALUES ($1,'test-machine','test-project','codex','Run the build','2026-08-09T10:00:00Z'::timestamptz,1,0);
		INSERT INTO messages (session_id,ordinal,role,content,timestamp,content_length)
		VALUES ($1,1,'assistant','running','2026-08-09T10:00:00Z'::timestamptz,7);
		INSERT INTO tool_calls (session_id,message_ordinal,call_index,tool_name,category,tool_use_id,input_json,result_content)
		VALUES ($1,1,0,'shell_command','shell','call-1','{"command":"run"}','fallback'),
		       ($1,1,1,'shell_command','shell','call-2','{"command":"check"}','fallback');
		INSERT INTO tool_result_events (session_id,tool_call_message_ordinal,call_index,tool_use_id,source,status,content,timestamp,event_index)
		VALUES ($1,1,0,'call-1','tool_execution','completed',$2,'2026-08-09T10:00:02Z'::timestamptz,0),
		       ($1,1,1,'call-2','tool_execution','completed',$3,'2026-08-09T10:00:03Z'::timestamptz,0)`,
		sessionID, failure, success,
	)
	require.NoError(t, err)

	_, calls, err := store.issueReviewRows(context.Background(), []db.IssueReviewSession{{ID: sessionID}})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	byID := map[string]db.IssueReviewToolCall{calls[0].ToolUseID: calls[0], calls[1].ToolUseID: calls[1]}
	assert.Contains(t, byID["call-1"].Result, "ParserError: stable tail failure")
	assert.NotContains(t, byID["call-2"].Result, "SUCCESS_TAIL_SENTINEL")
}
