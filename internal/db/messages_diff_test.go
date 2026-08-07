package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptMessagesEqualUsesCanonicalTimestampPrecision(t *testing.T) {
	stored := []Message{{
		SessionID: "session", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-04T01:02:03.123456Z",
		ToolCalls: []ToolCall{{ResultEvents: []ToolResultEvent{{
			Timestamp: "2026-08-04T01:02:04.654321Z",
		}}}},
	}}
	incoming := []Message{{
		SessionID: "session", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-04T01:02:03.123456789Z",
		ToolCalls: []ToolCall{{ResultEvents: []ToolResultEvent{{
			Timestamp: "2026-08-04T01:02:04.654321987Z",
		}}}},
	}}
	assert.True(t, transcriptMessagesEqual(stored, incoming))
}

func TestReplaceSessionMessagesCanonicalNoOpDoesNotRepairRows(t *testing.T) {
	d := testDB(t)
	stored := []Message{
		{
			SessionID: "canonical-noop", Ordinal: 0, Role: "assistant",
			Content: "answer", Timestamp: "2026-08-04T01:02:03.123456Z",
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "file", ToolUseID: "tool-1",
				InputJSON: `{"path":"a.go"}`, SkillName: "inspect",
				ResultContentLength: 4, ResultContent: "done",
				SubagentSessionID: "sub-1", FilePath: "a.go",
				ResultEvents: []ToolResultEvent{{
					EventIndex: 3,
					ToolUseID:  "tool-1", AgentID: "agent-1",
					SubagentSessionID: "sub-1", Source: "result",
					Status: "started", Content: "working", ContentLength: 7,
					Timestamp: "2026-08-04T01:02:04.123456Z",
				}, {
					EventIndex: 7,
					ToolUseID:  "tool-1", AgentID: "agent-1",
					SubagentSessionID: "sub-1", Source: "result",
					Status: "ok", Content: "done", ContentLength: 4,
					Timestamp: "2026-08-04T01:02:04.654321Z",
				}},
			}},
		},
		{
			SessionID: "canonical-noop", Ordinal: 1, Role: "user",
			Content: "follow-up", Timestamp: "2026-08-04T01:03:00Z",
		},
	}
	seedDiffSession(t, d, "canonical-noop", stored)

	_, err := d.getWriter().Exec(`
		CREATE TABLE message_update_audit (session_id TEXT NOT NULL);
		CREATE TRIGGER audit_canonical_noop_message_update
		AFTER UPDATE ON messages
		WHEN NEW.session_id = 'canonical-noop'
		BEGIN
			INSERT INTO message_update_audit(session_id) VALUES (NEW.session_id);
		END`)
	require.NoError(t, err, "install message update audit")
	before, err := d.GetSessionFull(t.Context(), "canonical-noop")
	require.NoError(t, err)
	require.NotNil(t, before)

	incoming := []Message{
		{
			SessionID: "canonical-noop", Ordinal: 0, Role: "assistant",
			Content:   "answer",
			Timestamp: "2026-08-03T21:02:03.123456789-04:00",
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "file", ToolUseID: "tool-1",
				InputJSON: `{"path":"a.go"}`, SkillName: "inspect",
				ResultContentLength: 4, ResultContent: "done",
				SubagentSessionID: "sub-1", FilePath: "a.go",
				ResultEvents: []ToolResultEvent{{
					EventIndex: 7,
					ToolUseID:  "tool-1", AgentID: "agent-1",
					SubagentSessionID: "sub-1", Source: "result",
					Status: "ok", Content: "done", ContentLength: 4,
					Timestamp: "2026-08-03T21:02:04.654321987-04:00",
				}, {
					EventIndex: 3,
					ToolUseID:  "tool-1", AgentID: "agent-1",
					SubagentSessionID: "sub-1", Source: "result",
					Status: "started", Content: "working", ContentLength: 7,
					Timestamp: "2026-08-03T21:02:04.123456789-04:00",
				}},
			}},
		},
		stored[1],
	}
	require.NoError(t, d.ReplaceSessionMessages("canonical-noop", incoming))

	var updates int
	require.NoError(t, d.getReader().QueryRow(
		"SELECT count(*) FROM message_update_audit",
	).Scan(&updates))
	assert.Zero(t, updates,
		"canonical-equivalent rows must not be repaired repeatedly")
	after, err := d.GetSessionFull(t.Context(), "canonical-noop")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision,
		"canonical-equivalent event ordering must not bump the transcript revision")
}

func diffTestMsg(
	sessionID string, ord int, role, content string,
	mut ...func(*Message),
) Message {
	m := Message{
		SessionID:     sessionID,
		Ordinal:       ord,
		Role:          role,
		Content:       content,
		Timestamp:     "2026-06-20T10:00:00Z",
		ContentLength: len(content),
	}
	for _, f := range mut {
		f(&m)
	}
	return m
}

func seedDiffSession(
	t *testing.T, d *DB, sessionID string, msgs []Message,
) {
	t.Helper()
	require.NoError(t, d.UpsertSession(Session{
		ID:      sessionID,
		Project: "proj",
		Machine: defaultMachine,
		Agent:   defaultAgent,
	}), "seed session %s", sessionID)
	require.NoError(t, d.InsertMessages(msgs),
		"seed messages for %s", sessionID)
}

func messageIDsByOrdinal(
	t *testing.T, d *DB, sessionID string,
) map[int]int64 {
	t.Helper()
	rows, err := d.getReader().Query(
		"SELECT ordinal, id FROM messages WHERE session_id = ?",
		sessionID,
	)
	require.NoError(t, err)
	defer rows.Close()
	out := make(map[int]int64)
	for rows.Next() {
		var ord int
		var id int64
		require.NoError(t, rows.Scan(&ord, &id))
		out[ord] = id
	}
	require.NoError(t, rows.Err())
	return out
}

// TestReplaceSessionMessagesUpdatesChangedRowsInPlace verifies the
// streaming chunk-merge shape: a replace whose new message set only
// extends the stored tail and appends must keep the rowids of every
// stored message — unchanged rows untouched, the merged tail updated
// in place — instead of delete+reinserting the whole session.
func TestReplaceSessionMessagesUpdatesChangedRowsInPlace(t *testing.T) {
	d := testDB(t)

	v1 := []Message{
		diffTestMsg("diff-a", 0, "user", "hello there"),
		diffTestMsg("diff-a", 1, "assistant", "partial chunk",
			func(m *Message) {
				m.ClaudeMessageID = "m1"
				m.ToolCalls = []ToolCall{{
					ToolName: "Bash",
					Category: "execution",
					ResultEvents: []ToolResultEvent{{
						Source: "result", Status: "ok",
						Content: "one",
					}},
				}}
			}),
	}
	seedDiffSession(t, d, "diff-a", v1)
	// A second session claims MAX(id), so a delete+reinsert of
	// diff-a would visibly reassign its rowids.
	seedDiffSession(t, d, "diff-b", []Message{
		diffTestMsg("diff-b", 0, "user", "other session"),
	})
	before := messageIDsByOrdinal(t, d, "diff-a")
	require.Len(t, before, 2)

	v2 := []Message{
		v1[0],
		diffTestMsg("diff-a", 1, "assistant",
			"partial chunk plus merged tail zqmergetoken",
			func(m *Message) {
				m.ClaudeMessageID = "m1"
				m.Timestamp = "2026-06-20T06:00:01.123456789-04:00"
				m.ToolCalls = []ToolCall{{
					ToolName: "Bash", Category: "execution",
					ResultEvents: []ToolResultEvent{{
						EventIndex: 6, Source: "result", Status: "ok",
						Content: "updated",
					}},
				}}
			}),
		diffTestMsg("diff-a", 2, "assistant", "follow-up",
			func(m *Message) {
				m.ToolCalls = []ToolCall{{
					ToolName: "Read", Category: "file",
					ResultEvents: []ToolResultEvent{{
						EventIndex: 9, Source: "result", Status: "ok",
						Content: "appended",
					}},
				}}
			}),
	}
	require.NoError(t, d.ReplaceSessionMessages("diff-a", v2))

	after := messageIDsByOrdinal(t, d, "diff-a")
	require.Len(t, after, 3)
	assert.Equal(t, before[0], after[0],
		"unchanged row must keep its rowid")
	assert.Equal(t, before[1], after[1],
		"merged tail row must be updated in place, not reinserted")

	msgs, err := d.GetAllMessages(context.Background(), "diff-a")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Contains(t, msgs[1].Content, "zqmergetoken",
		"merged content must be persisted")
	assert.Equal(t, "2026-06-20T10:00:01.123456Z", msgs[1].Timestamp)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, 6, msgs[1].ToolCalls[0].ResultEvents[0].EventIndex)
	require.Len(t, msgs[2].ToolCalls, 1)
	require.Len(t, msgs[2].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, 9, msgs[2].ToolCalls[0].ResultEvents[0].EventIndex)

	if d.HasFTS() {
		var n int
		require.NoError(t, d.getReader().QueryRow(
			`SELECT count(*) FROM messages_fts
			 WHERE messages_fts MATCH 'zqmergetoken'`,
		).Scan(&n))
		assert.Equal(t, 1, n,
			"FTS index must cover the updated row content")
	}
}

// TestReplaceSessionMessagesDiffMatchesFullReplace checks that the
// stored state after replacing v1 with v2 is identical (modulo
// rowids) to inserting v2 from scratch, across shapes that exercise
// the in-place diff, the append path, and the full-replace
// fallbacks (truncation, wholesale rewrites).
func TestReplaceSessionMessagesDiffMatchesFullReplace(t *testing.T) {
	base := func(sid string) []Message {
		return []Message{
			diffTestMsg(sid, 0, "user", "question",
				func(m *Message) {
					m.ToolCalls = []ToolCall{{
						ToolName:  "Read",
						Category:  "file",
						InputJSON: `{"path":"a.go"}`,
					}}
				}),
			diffTestMsg(sid, 1, "assistant", "partial answer",
				func(m *Message) {
					m.ClaudeMessageID = "m1"
					m.ContextTokens = 100
					m.HasContextTokens = true
					m.ToolCalls = []ToolCall{{
						ToolName:  "Task",
						Category:  "agent",
						ToolUseID: "tu1",
						ResultEvents: []ToolResultEvent{{
							Source: "progress", Status: "running",
							Content: "spawning",
						}},
					}}
				}),
		}
	}

	cases := []struct {
		name string
		v2   func(sid string) []Message
	}{
		{"identical no-op", func(sid string) []Message {
			return base(sid)
		}},
		{"chunk merge tail plus append", func(sid string) []Message {
			msgs := base(sid)
			msgs[1].Content = "partial answer now completed"
			msgs[1].ContentLength = len(msgs[1].Content)
			return append(msgs, diffTestMsg(sid, 2, "user", "thanks"))
		}},
		{"subagent linkage event appended", func(sid string) []Message {
			msgs := base(sid)
			tc := &msgs[1].ToolCalls[0]
			tc.SubagentSessionID = "sub-1"
			tc.ResultEvents = append(tc.ResultEvents, ToolResultEvent{
				Source: "result", Status: "ok",
				Content: "done", AgentID: "agent-9",
			})
			return msgs
		}},
		{"token fields updated", func(sid string) []Message {
			msgs := base(sid)
			msgs[1].OutputTokens = 42
			msgs[1].HasOutputTokens = true
			return msgs
		}},
		{"tool input changed", func(sid string) []Message {
			msgs := base(sid)
			msgs[0].ToolCalls[0].InputJSON = `{"path":"b.go"}`
			return msgs
		}},
		{"truncated", func(sid string) []Message {
			return base(sid)[:1]
		}},
		{"wholesale rewrite", func(sid string) []Message {
			msgs := base(sid)
			for i := range msgs {
				msgs[i].Content = "rewritten " + msgs[i].Content
				msgs[i].ContentLength = len(msgs[i].Content)
			}
			return msgs
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testDB(t)
			seedDiffSession(t, got, "par", base("par"))
			require.NoError(t,
				got.ReplaceSessionMessages("par", tc.v2("par")))

			want := testDB(t)
			seedDiffSession(t, want, "par", tc.v2("par"))

			gotMsgs, err := got.GetAllMessages(
				context.Background(), "par",
			)
			require.NoError(t, err)
			wantMsgs, err := want.GetAllMessages(
				context.Background(), "par",
			)
			require.NoError(t, err)
			assert.Equal(t,
				stripRowIdentity(wantMsgs), stripRowIdentity(gotMsgs),
				"replaced state must match a from-scratch insert")
		})
	}
}

// stripRowIdentity zeroes rowid-derived fields so state comparisons
// ignore legitimately different id allocations.
func stripRowIdentity(msgs []Message) []Message {
	out := append([]Message(nil), msgs...)
	for i := range out {
		out[i].ID = 0
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			out[i].ToolCalls[j].MessageID = 0
		}
	}
	return out
}

// TestReplaceSessionMessagesKeepsPinOnMergedRow guards pin survival
// through a chunk-merge replace: the pinned tail row is updated, not
// deleted, so its pin must remain.
func TestReplaceSessionMessagesKeepsPinOnMergedRow(t *testing.T) {
	d := testDB(t)
	v1 := []Message{
		diffTestMsg("pin-s", 0, "user", "hello"),
		diffTestMsg("pin-s", 1, "assistant", "partial"),
	}
	seedDiffSession(t, d, "pin-s", v1)
	ids := messageIDsByOrdinal(t, d, "pin-s")
	note := "keep me"
	pinID, err := d.PinMessage("pin-s", ids[1], &note)
	require.NoError(t, err)
	require.NotZero(t, pinID)

	v2 := []Message{
		v1[0],
		diffTestMsg("pin-s", 1, "assistant", "partial now complete"),
		diffTestMsg("pin-s", 2, "user", "more"),
	}
	require.NoError(t, d.ReplaceSessionMessages("pin-s", v2))

	var n int
	require.NoError(t, d.getReader().QueryRow(
		`SELECT count(*) FROM pinned_messages
		 WHERE session_id = 'pin-s' AND ordinal = 1 AND note = ?`,
		note,
	).Scan(&n))
	assert.Equal(t, 1, n, "pin on the merged row must survive")
}
