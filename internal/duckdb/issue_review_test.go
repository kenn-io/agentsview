//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestIssueReviewRowsChunksSessionsAndAggregatesEvents(t *testing.T) {
	ctx := context.Background()
	syncer := newInMemoryTestSync(t, newLocalDB(t), SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	store := NewStoreFromDB(syncer.DB())

	sessions := make([]db.IssueReviewSession, 405)
	for i := range sessions {
		sessions[i].ID = fmt.Sprintf("session-%03d", i)
	}
	last := sessions[len(sessions)-1].ID
	_, err := syncer.DB().ExecContext(ctx, `INSERT INTO messages (id,session_id,ordinal,role,content,timestamp) VALUES (1,?,7,'assistant','Root cause confirmed: command failed because the dependency is missing','2026-08-09T10:00:00Z')`, last)
	require.NoError(t, err)
	_, err = syncer.DB().ExecContext(ctx, `INSERT INTO tool_calls (id,message_id,session_id,tool_name,category,call_index,tool_use_id,input_json,result_content) VALUES (2,1,?,'shell_command','shell',0,'call-1','{"command":"run"}','fallback'),(5,1,?,'shell_command','shell',1,'call-2','{"command":"check"}','fallback')`, last, last)
	require.NoError(t, err)
	failure := "Script failed\n" + strings.Repeat("progress output ", 200) + "\nParserError: stable tail failure"
	success := strings.Repeat("completed output ", 200) + "\nSUCCESS_TAIL_SENTINEL"
	_, err = syncer.DB().ExecContext(ctx, `INSERT INTO tool_result_events (id,session_id,tool_call_message_ordinal,call_index,source,status,content,timestamp,event_index) VALUES (3,?,7,0,'tool_execution','started','','2026-08-09T10:00:00Z',0),(4,?,7,0,'tool_execution','completed',?,'2026-08-09T10:00:02Z',1),(6,?,7,1,'tool_execution','completed',?,'2026-08-09T10:00:03Z',0)`, last, last, failure, last, success)
	require.NoError(t, err)

	messages, calls, err := store.issueReviewRows(ctx, sessions)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, calls, 2)
	assert.Equal(t, last, messages[0].SessionID)
	byID := map[string]db.IssueReviewToolCall{calls[0].ToolUseID: calls[0], calls[1].ToolUseID: calls[1]}
	assert.Equal(t, "completed", byID["call-1"].EventStatus)
	assert.Contains(t, byID["call-1"].Result, "ParserError: stable tail failure")
	assert.NotContains(t, byID["call-2"].Result, "SUCCESS_TAIL_SENTINEL")
	require.NotNil(t, byID["call-1"].DurationMS)
	assert.Equal(t, int64(2000), *byID["call-1"].DurationMS)
}
