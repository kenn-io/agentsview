package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolCallResultSummaryStorage pins both halves of the tool-result
// dedup: what the archive stores for a call, and what a loaded call looks
// like to every consumer. The loaded ResultContent must match what the
// column held back when the summary was written twice.
func TestToolCallResultSummaryStorage(t *testing.T) {
	tests := []struct {
		name          string
		call          ToolCall
		wantStored    string
		wantStoredLen int
		wantLoaded    string
	}{
		{
			name: "single event summary is not stored",
			call: ToolCall{
				ToolName:            "Bash",
				Category:            "Bash",
				ToolUseID:           "call_one",
				ResultContent:       "total 4\ndrwxr-xr-x",
				ResultContentLength: len("total 4\ndrwxr-xr-x"),
				ResultEvents: []ToolResultEvent{{
					ToolUseID:     "call_one",
					Source:        "function_call_output",
					Status:        "completed",
					Content:       "total 4\ndrwxr-xr-x",
					ContentLength: len("total 4\ndrwxr-xr-x"),
				}},
			},
			wantStored:    "",
			wantStoredLen: len("total 4\ndrwxr-xr-x"),
			wantLoaded:    "total 4\ndrwxr-xr-x",
		},
		{
			name: "multi event summary is stored",
			call: ToolCall{
				ToolName:            "Task",
				Category:            "Task",
				ToolUseID:           "call_many",
				ResultContent:       "agent-1:\nfirst\n\nagent-2:\nsecond",
				ResultContentLength: len("agent-1:\nfirst\n\nagent-2:\nsecond"),
				ResultEvents: []ToolResultEvent{
					{
						ToolUseID:     "call_many",
						AgentID:       "agent-1",
						Source:        "subagent_notification",
						Status:        "completed",
						Content:       "first",
						ContentLength: len("first"),
						EventIndex:    0,
					},
					{
						ToolUseID:     "call_many",
						AgentID:       "agent-2",
						Source:        "subagent_notification",
						Status:        "completed",
						Content:       "second",
						ContentLength: len("second"),
						EventIndex:    1,
					},
				},
			},
			wantStored:    "agent-1:\nfirst\n\nagent-2:\nsecond",
			wantStoredLen: len("agent-1:\nfirst\n\nagent-2:\nsecond"),
			wantLoaded:    "agent-1:\nfirst\n\nagent-2:\nsecond",
		},
		{
			name: "single event summary that differs is stored",
			call: ToolCall{
				ToolName:            "Task",
				Category:            "Task",
				ToolUseID:           "call_diff",
				ResultContent:       "agent-1:\nonly",
				ResultContentLength: len("agent-1:\nonly"),
				ResultEvents: []ToolResultEvent{{
					ToolUseID:     "call_diff",
					AgentID:       "agent-1",
					Source:        "subagent_notification",
					Status:        "completed",
					Content:       "only",
					ContentLength: len("only"),
				}},
			},
			wantStored:    "agent-1:\nonly",
			wantStoredLen: len("agent-1:\nonly"),
			wantLoaded:    "agent-1:\nonly",
		},
		{
			name: "blocked category keeps its blanked shape",
			call: ToolCall{
				ToolName:            "Read",
				Category:            "Read",
				ToolUseID:           "call_blocked",
				ResultContent:       "",
				ResultContentLength: 4096,
				ResultEvents: []ToolResultEvent{{
					ToolUseID:     "call_blocked",
					Source:        "function_call_output",
					Status:        "completed",
					Content:       "",
					ContentLength: 4096,
				}},
			},
			wantStored:    "",
			wantStoredLen: 4096,
			wantLoaded:    "",
		},
		{
			name: "empty summary over a blank event stays empty",
			call: ToolCall{
				ToolName:            "Bash",
				Category:            "Bash",
				ToolUseID:           "call_blank",
				ResultContent:       "",
				ResultContentLength: 0,
				ResultEvents: []ToolResultEvent{{
					ToolUseID:     "call_blank",
					Source:        "function_call_output",
					Status:        "completed",
					Content:       "   ",
					ContentLength: 3,
				}},
			},
			wantStored:    "",
			wantStoredLen: 0,
			wantLoaded:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "s-dedup", "proj")
			call := tt.call
			call.SessionID = "s-dedup"
			require.NoError(t, d.InsertMessages([]Message{{
				SessionID:  "s-dedup",
				Ordinal:    0,
				Role:       "assistant",
				Content:    "running a tool",
				HasToolUse: true,
				ToolCalls:  []ToolCall{call},
			}}))

			var stored string
			var storedLen int
			require.NoError(t, d.Reader().QueryRow(`
				SELECT COALESCE(result_content, ''),
				       COALESCE(result_content_length, 0)
				FROM tool_calls
				WHERE session_id = ? AND tool_use_id = ?`,
				"s-dedup", call.ToolUseID,
			).Scan(&stored, &storedLen))
			assert.Equal(t, tt.wantStored, stored, "stored result_content")
			assert.Equal(t, tt.wantStoredLen, storedLen,
				"stored result_content_length")

			msgs, err := d.GetMessages(
				context.Background(), "s-dedup", 0, 10, true,
			)
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			require.Len(t, msgs[0].ToolCalls, 1)
			assert.Equal(t, tt.wantLoaded,
				msgs[0].ToolCalls[0].ResultContent,
				"loaded ToolCall.ResultContent")
			assert.Len(t, msgs[0].ToolCalls[0].ResultEvents,
				len(call.ResultEvents), "result events survive")
		})
	}
}

// TestSearchSessionFindsDedupedResultContent covers the in-session find bar,
// which reads the column in SQL instead of loading tool calls.
func TestSearchSessionFindsDedupedResultContent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-find", "proj")
	require.NoError(t, d.InsertMessages([]Message{{
		SessionID:  "s-find",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "running a tool",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			SessionID:           "s-find",
			ToolName:            "Bash",
			Category:            "Bash",
			ToolUseID:           "call_find",
			ResultContent:       "needle in the output",
			ResultContentLength: len("needle in the output"),
			ResultEvents: []ToolResultEvent{{
				ToolUseID:     "call_find",
				Source:        "function_call_output",
				Status:        "completed",
				Content:       "needle in the output",
				ContentLength: len("needle in the output"),
			}},
		}},
	}}))

	ordinals, err := d.SearchSession(context.Background(), "s-find", "needle")
	require.NoError(t, err)
	assert.Equal(t, []int{0}, ordinals)
}

func TestRestoreToolCallResultContent(t *testing.T) {
	tests := []struct {
		name string
		call ToolCall
		want string
	}{
		{
			name: "stored summary wins",
			call: ToolCall{
				ResultContent:       "summary",
				ResultContentLength: 7,
				ResultEvents:        []ToolResultEvent{{Content: "event"}},
			},
			want: "summary",
		},
		{
			name: "single event refills a cleared summary",
			call: ToolCall{
				ResultContentLength: 5,
				ResultEvents:        []ToolResultEvent{{Content: "event"}},
			},
			want: "event",
		},
		{
			name: "zero length is a genuinely empty summary",
			call: ToolCall{
				ResultEvents: []ToolResultEvent{{Content: "  "}},
			},
			want: "",
		},
		{
			name: "multiple events never refill",
			call: ToolCall{
				ResultContentLength: 5,
				ResultEvents: []ToolResultEvent{
					{Content: "a"}, {Content: "b"},
				},
			},
			want: "",
		},
		{
			name: "no events never refill",
			call: ToolCall{ResultContentLength: 5},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := tt.call
			RestoreToolCallResultContent(&call)
			assert.Equal(t, tt.want, call.ResultContent)
		})
	}
}

// TestSubagentLinkKeepsDedupedSummary pins the incremental link path: a
// linked result that repeats the call's single stored event must not
// re-inflate result_content, while a summary the event does not carry is
// stored as before.
func TestSubagentLinkKeepsDedupedSummary(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-link", "proj")
	require.NoError(t, d.InsertMessages([]Message{{
		SessionID:  "s-link",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "spawning an agent",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			SessionID:           "s-link",
			ToolName:            "Agent",
			Category:            "Task",
			ToolUseID:           "call_link",
			ResultContent:       "agent finished",
			ResultContentLength: len("agent finished"),
			// The event carries no ToolUseID of its own; the insert path
			// copies the call's id onto it, so the link path could find it
			// either way. The test pins the stored shape, not the key.
			ResultEvents: []ToolResultEvent{{
				Source:        "subagent_notification",
				Status:        "completed",
				Content:       "agent finished",
				ContentLength: len("agent finished"),
			}},
		}},
	}}))

	stored := func() (string, int) {
		var content string
		var length int
		require.NoError(t, d.Reader().QueryRow(`
			SELECT COALESCE(result_content, ''),
			       COALESCE(result_content_length, 0)
			FROM tool_calls WHERE session_id = ? AND tool_use_id = ?`,
			"s-link", "call_link",
		).Scan(&content, &length))
		return content, length
	}

	content, length := stored()
	require.Empty(t, content, "insert must dedup the single-event summary")
	require.Equal(t, len("agent finished"), length)

	link := func(result string) {
		require.NoError(t, d.WriteSessionIncremental("s-link", nil,
			IncrementalSessionUpdate{
				MsgCount:    1,
				NextOrdinal: 1,
				SubagentLinks: []ToolCallSubagentLink{{
					ToolUseID:         "call_link",
					SubagentSessionID: "agent-child",
					ResultContent:     result,
					ResultContentLen:  len(result),
					HasResult:         true,
				}},
			}))
	}

	link("agent finished")
	content, length = stored()
	assert.Empty(t, content, "link repeating the event must stay deduped")
	assert.Equal(t, len("agent finished"), length)

	msgs, err := d.GetMessages(context.Background(), "s-link", 0, 10, true)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "agent finished", msgs[0].ToolCalls[0].ResultContent)
	assert.Equal(t, "agent-child", msgs[0].ToolCalls[0].SubagentSessionID)

	link("agent finished with a longer final report")
	content, length = stored()
	assert.Equal(t, "agent finished with a longer final report", content,
		"a summary the event does not carry is stored")
	assert.Equal(t, len("agent finished with a longer final report"), length)
}

// TestOmittedResultContentLengthRoundTrips pins the public write API: the
// stored length of a non-empty summary or event is always the text's
// length, whatever the caller supplied, and a supplied length survives
// only for withheld text. The length is what tells the loader to refill a
// summary the dedup dropped, so a caller cannot break the round trip.
func TestOmittedResultContentLengthRoundTrips(t *testing.T) {
	const summary = "output without a declared length"
	call := func(id string) ToolCall {
		return ToolCall{
			ToolName:      "Bash",
			Category:      "Bash",
			ToolUseID:     id,
			ResultContent: summary,
			ResultEvents: []ToolResultEvent{{
				Source:  "function_call_output",
				Status:  "completed",
				Content: summary,
			}},
		}
	}
	// loaded reads the single call back and requires exactly wantEvents
	// result events, so the event-length check below can never pass by
	// looping over nothing.
	loaded := func(
		t *testing.T, d *DB, sessionID string, wantEvents int,
	) ToolCall {
		t.Helper()
		msgs, err := d.GetMessages(context.Background(), sessionID, 0, 10, true)
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		require.Len(t, msgs[0].ToolCalls, 1)
		tc := msgs[0].ToolCalls[0]
		require.Len(t, tc.ResultEvents, wantEvents)
		for _, ev := range tc.ResultEvents {
			assert.Equal(t, len(ev.Content), ev.ContentLength,
				"event length is inferred when omitted")
		}
		return tc
	}

	t.Run("InsertMessages", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s-nolen", "proj")
		require.NoError(t, d.InsertMessages([]Message{{
			SessionID: "s-nolen", Ordinal: 0, Role: "assistant",
			Content: "running", HasToolUse: true,
			ToolCalls: []ToolCall{call("call_nolen")},
		}}))
		got := loaded(t, d, "s-nolen", 1)
		assert.Equal(t, summary, got.ResultContent)
		assert.Equal(t, len(summary), got.ResultContentLength)
	})

	t.Run("WriteSessionBatch", func(t *testing.T) {
		d := testDB(t)
		_, err := d.WriteSessionBatch([]SessionBatchWrite{{
			Session: Session{
				ID: "s-batch-nolen", Project: "proj",
				Machine: defaultMachine, Agent: defaultAgent,
				MessageCount: 1,
			},
			Messages: []Message{{
				SessionID: "s-batch-nolen", Ordinal: 0, Role: "assistant",
				Content: "running", HasToolUse: true,
				ToolCalls: []ToolCall{call("call_batch_nolen")},
			}},
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
		got := loaded(t, d, "s-batch-nolen", 1)
		assert.Equal(t, summary, got.ResultContent)
		assert.Equal(t, len(summary), got.ResultContentLength)
	})

	t.Run("wrong length is corrected", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s-wrong", "proj")
		wrong := call("call_wrong")
		wrong.ResultContentLength = len(summary) - 1
		wrong.ResultEvents[0].ContentLength = len(summary) + 7
		require.NoError(t, d.InsertMessages([]Message{{
			SessionID: "s-wrong", Ordinal: 0, Role: "assistant",
			Content: "running", HasToolUse: true,
			ToolCalls: []ToolCall{wrong},
		}}))
		got := loaded(t, d, "s-wrong", 1)
		assert.Equal(t, summary, got.ResultContent)
		assert.Equal(t, len(summary), got.ResultContentLength)
	})

	t.Run("withheld text keeps the supplied length", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s-withheld", "proj")
		require.NoError(t, d.InsertMessages([]Message{{
			SessionID: "s-withheld", Ordinal: 0, Role: "assistant",
			Content: "running", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "Read", ToolUseID: "call_withheld",
				ResultContentLength: 4096,
			}},
		}}))
		got := loaded(t, d, "s-withheld", 0)
		assert.Empty(t, got.ResultContent)
		assert.Equal(t, 4096, got.ResultContentLength)
	})

	t.Run("WriteSessionIncremental link", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "s-link-nolen", "proj")
		require.NoError(t, d.InsertMessages([]Message{{
			SessionID: "s-link-nolen", Ordinal: 0, Role: "assistant",
			Content: "spawning", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", ToolUseID: "call_link_nolen",
			}},
		}}))
		require.NoError(t, d.WriteSessionIncremental("s-link-nolen", nil,
			IncrementalSessionUpdate{
				MsgCount: 1, NextOrdinal: 1,
				SubagentLinks: []ToolCallSubagentLink{{
					ToolUseID:         "call_link_nolen",
					SubagentSessionID: "agent-child",
					ResultContent:     summary,
					HasResult:         true,
				}},
			}))
		got := loaded(t, d, "s-link-nolen", 0)
		assert.Equal(t, summary, got.ResultContent)
		assert.Equal(t, len(summary), got.ResultContentLength)
	})
}
