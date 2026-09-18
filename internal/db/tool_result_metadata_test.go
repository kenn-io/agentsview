package db

import (
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLateResultBatchWithinSQLiteVariableLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "exec_command", Category: "Bash"}},
	})
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(err)
	require.NoError(conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
		return nil
	}))
	require.NoError(conn.Close())

	// All events belong to one call and one summary participant. Chunking
	// must preserve the first occurrence and the latest result across batches.
	events := make([]ToolResultEvent, 350)
	for i := range events {
		events[i] = ToolResultEvent{Content: fmt.Sprintf("result %d", i), Source: "function_call_output"}
	}
	_, err = d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount: 1, NextOrdinal: 1,
		ToolCallResultUpdates: []ToolCallResultUpdate{{
			ToolUseID: "call", Position: ToolCallPosition{}, Events: events,
		}},
	})
	require.NoError(err)
	msgs, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Len(call.ResultEvents, 350)
	assert.Equal("result 349", call.ResultContent)
	var first, latest int
	require.NoError(d.getReader().QueryRow(`
		SELECT first_event_index, latest_event_index
		FROM tool_call_occurrence_agent_state
		WHERE session_id = 's1' AND message_ordinal = 0 AND call_index = 0 AND agent_id = ''`,
	).Scan(&first, &latest))
	assert.Equal(0, first)
	assert.Equal(349, latest)
}

func TestLateResultAgentBackfillWithinSQLiteVariableLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "s1", "project-a")
	events := make([]ToolResultEvent, 350)
	for i := range events {
		events[i] = ToolResultEvent{
			AgentID: fmt.Sprintf("agent-%03d", i), Content: "original",
			Source: "wait_output", EventIndex: i,
		}
		PrepareToolResultEvent(&events[i])
	}
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "wait_agent", ResultEvents: events}},
	})
	conn, err := d.getWriter().Conn(t.Context())
	require.NoError(err)
	require.NoError(conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, 999)
		return nil
	}))
	require.NoError(conn.Close())

	_, err = d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount: 1, NextOrdinal: 1,
		ToolCallResultUpdates: []ToolCallResultUpdate{{
			ToolUseID: "call", Position: ToolCallPosition{},
			Events: []ToolResultEvent{{AgentID: "agent-000", Content: "updated", Source: "wait_output"}},
		}},
	})
	require.NoError(err)
	msgs, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Len(call.ResultEvents, 351)
	assert.Contains(call.ResultContent, "agent-000:\nupdated\n\nagent-001:\noriginal")
	var first, latest, participants int
	require.NoError(d.getReader().QueryRow(`
		SELECT first_event_index, latest_event_index
		FROM tool_call_occurrence_agent_state
		WHERE session_id = 's1' AND message_ordinal = 0 AND call_index = 0 AND agent_id = 'agent-000'`,
	).Scan(&first, &latest))
	require.NoError(d.getReader().QueryRow(`
		SELECT COUNT(*) FROM tool_call_occurrence_agent_state WHERE session_id = 's1'`,
	).Scan(&participants))
	assert.Equal(0, first)
	assert.Equal(350, latest)
	assert.Equal(350, participants)
}

func TestWriteSessionIncrementalPreservesRawResultContract(t *testing.T) {
	for _, tt := range []struct {
		name        string
		contents    []string
		blocked     bool
		wantSummary string
		wantLength  int
		wantCount   int
	}{
		{"whitespace", []string{"useful", " \t"}, false, "useful", 6, 2},
		{"control_identity", []string{"x\x01", "x\x02"}, false, "x", 1, 2},
		{"blocked_identity", []string{"yy", "zz"}, true, "", 2, 2},
		{"blocked_whitespace", []string{"useful", " \t"}, true, "", 6, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			d := testDB(t)
			insertSession(t, d, "s1", "proj")
			insertMessages(t, d, Message{
				SessionID: "s1", Ordinal: 0, Role: "assistant",
				HasToolUse: true, ToolCalls: []ToolCall{{SessionID: "s1",
					ToolName: "exec_command", Category: "Bash", ToolUseID: "call",
				}},
			})
			for _, content := range tt.contents {
				_, err := d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
					MsgCount: 1, NextOrdinal: 1,
					BlockedResultCategories: map[string]bool{"Bash": tt.blocked},
					ToolCallResultUpdates: []ToolCallResultUpdate{{
						ToolUseID: "call", Position: ToolCallPosition{},
						Events: []ToolResultEvent{{ToolUseID: "call", Source: "function_call_output", Content: content, ContentLength: len(content)}},
					}},
				})
				require.NoError(err)
			}
			msgs, err := d.GetAllMessages(t.Context(), "s1")
			require.NoError(err)
			require.Len(msgs, 1)
			require.Len(msgs[0].ToolCalls, 1)
			call := msgs[0].ToolCalls[0]
			assert.Equal(tt.wantSummary, call.ResultContent)
			assert.Equal(tt.wantLength, call.ResultContentLength)
			assert.Len(call.ResultEvents, tt.wantCount)
		})
	}
}

func TestLateResultKeepsRawAnonymousParticipation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		HasToolUse: true, ToolCalls: []ToolCall{{SessionID: "s1",
			ToolName: "exec_command", Category: "Bash", ToolUseID: "call",
		}},
	})
	for _, event := range []ToolResultEvent{{AgentID: "a", Content: "named"}, {Content: "\x01"}} {
		_, err := d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
			MsgCount: 1, NextOrdinal: 1,
			ToolCallResultUpdates: []ToolCallResultUpdate{{
				ToolUseID: "call", Position: ToolCallPosition{},
				Events: []ToolResultEvent{event},
			}},
		})
		require.NoError(err)
	}
	msgs, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolCalls, 1)
	assert.Equal("named\n\n", msgs[0].ToolCalls[0].ResultContent)
	assert.Equal(7, msgs[0].ToolCalls[0].ResultContentLength)
}

func TestLateResultRequiresMetadataForArchivedEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		HasToolUse: true, ToolCalls: []ToolCall{{SessionID: "s1",
			ToolName: "exec_command", Category: "Bash", ToolUseID: "call",
			ResultContent: "old",
			ResultEvents:  []ToolResultEvent{{ToolUseID: "call", Content: "old", ContentLength: 3}},
		}},
	})
	missing, err := d.HasMissingToolResultMetadata(t.Context(), "s1", []ToolCallPosition{{}})
	require.NoError(err)
	assert.True(missing, "the source must be reparsed before a late update")
	missing, err = d.HasMissingToolResultMetadata(t.Context(), "s1", []ToolCallPosition{{MessageOrdinal: 1}})
	require.NoError(err)
	assert.False(missing, "other call occurrences do not force a reparse")
	_, err = d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
		MsgCount: 1, NextOrdinal: 1,
		ToolCallResultUpdates: []ToolCallResultUpdate{{
			ToolUseID: "call", Position: ToolCallPosition{},
			Events: []ToolResultEvent{{Content: "new"}},
		}},
	})
	require.ErrorIs(err, ErrToolResultMetadataMissing)
	msgs, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolCalls, 1)
	assert.Equal("old", msgs[0].ToolCalls[0].ResultContent)
	assert.Len(msgs[0].ToolCalls[0].ResultEvents, 1)
}

func TestLateResultIdentitySurvivesTranscriptReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	event := ToolResultEvent{ToolUseID: "call", Content: "x\x01", ContentLength: 2}
	PrepareToolResultEvent(&event)
	call := ToolCall{SessionID: "s1",
		ToolName: "exec_command", Category: "Bash", ToolUseID: "call",
		ResultContent: "x\x01", ResultContentLength: 2,
		ResultEvents: []ToolResultEvent{event},
	}
	SanitizeToolCall(&call)
	require.NoError(d.ReplaceSessionContent("s1", []Message{{
		SessionID: "s1", Ordinal: 0, Role: "assistant",
		HasToolUse: true, ToolCalls: []ToolCall{call},
	}}, SessionSignalUpdate{}, nil))
	missing, err := d.HasMissingToolResultMetadata(t.Context(), "s1", []ToolCallPosition{{}})
	require.NoError(err)
	assert.False(missing, "known raw metadata allows incremental append")
	msgs, err := d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	rev, err := d.TranscriptRevision("s1")
	require.NoError(err)
	require.NoError(d.ReplaceSessionContent("s1", msgs, SessionSignalUpdate{}, nil))
	after, err := d.TranscriptRevision("s1")
	require.NoError(err)
	assert.Equal(rev, after, "replacing the same stored transcript preserves revision")
	for _, content := range []string{"x\x02", "x\x01"} {
		_, err = d.WriteSessionIncremental("s1", nil, IncrementalSessionUpdate{
			MsgCount: 1, NextOrdinal: 1,
			ToolCallResultUpdates: []ToolCallResultUpdate{{
				ToolUseID: "call", Position: ToolCallPosition{},
				Events: []ToolResultEvent{{Content: content}},
			}},
		})
		require.NoError(err)
	}
	msgs, err = d.GetAllMessages(t.Context(), "s1")
	require.NoError(err)
	require.Len(msgs, 1)
	require.Len(msgs[0].ToolCalls, 1)
	assert.Len(msgs[0].ToolCalls[0].ResultEvents, 2, "raw identity survives sanitation, archive loading, and no-op replacement")
	assert.Equal("x", msgs[0].ToolCalls[0].ResultContent)
}
