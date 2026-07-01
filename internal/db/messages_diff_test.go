package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
				m.ToolCalls = v1[1].ToolCalls
			}),
		diffTestMsg("diff-a", 2, "assistant", "follow-up"),
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
