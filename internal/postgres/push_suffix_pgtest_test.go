//go:build pgtest

package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func suffixPrefixVersions(t *testing.T, syncer *Sync, boundary int) []string {
	t.Helper()
	versions := make([]string, 4)
	for i, statement := range []string{
		`SELECT COALESCE(string_agg(xmin::text, ',' ORDER BY ordinal),'') FROM messages WHERE session_id='suffix' AND ordinal < $1`,
		`SELECT COALESCE(string_agg(id::text || ':' || xmin::text, ',' ORDER BY message_ordinal, call_index),'') FROM tool_calls WHERE session_id='suffix' AND message_ordinal < $1`,
		`SELECT COALESCE(string_agg(id::text || ':' || xmin::text, ',' ORDER BY tool_call_message_ordinal, call_index, event_index),'') FROM tool_result_events WHERE session_id='suffix' AND tool_call_message_ordinal < $1`,
		`SELECT COALESCE(string_agg(id::text || ':' || xmin::text, ',' ORDER BY ordinal),'') FROM pinned_messages WHERE session_id='suffix' AND ordinal < $1`,
	} {
		require.NoError(t, syncer.pg.QueryRowContext(t.Context(), statement, boundary).Scan(&versions[i]))
	}
	return versions
}

func TestPushSuffixPublication(t *testing.T) {
	for _, test := range []struct {
		name      string
		boundary  int
		pins      int
		calls     int
		events    int
		laterEdit int
		legacyPin bool
		change    func([]db.Message) []db.Message
	}{
		{"append", 5, 2, 6, 7, 0, false, func(m []db.Message) []db.Message {
			return append(m, db.Message{SessionID: "suffix", Ordinal: 5, Role: "user", Content: "appended"})
		}},
		{"duplicate UUID after legacy pin", 5, 2, 6, 7, 0, true, func(m []db.Message) []db.Message {
			return append(m, db.Message{SessionID: "suffix", Ordinal: 5, Role: m[0].Role, Content: m[0].Content, SourceUUID: m[0].SourceUUID})
		}},
		{"late result", 4, 2, 6, 8, 0, false, func(m []db.Message) []db.Message {
			m[4].ToolCalls[0].ResultContent = "initial\n\nlate result"
			m[4].ToolCalls[0].ResultContentLength = 20
			m[4].ToolCalls[0].ResultEvents = append(m[4].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 1, ToolUseID: "call-4", Source: "tool_result", Status: "completed", Content: "late result", ContentLength: 11})
			return m
		}},
		{"truncate", 3, 1, 4, 5, 0, false, func(m []db.Message) []db.Message { return m[:3] }},
		{"emptied", 0, 0, 0, 0, 0, false, func([]db.Message) []db.Message { return nil }},
		{"edit", 2, 2, 6, 7, 0, false, func(m []db.Message) []db.Message { m[2].Content = "changed"; return m }},
		{"dropped result at zero with later edit", 0, 2, 6, 6, 1, false, func(m []db.Message) []db.Message {
			m[0].ToolCalls[1].ResultEvents = nil
			return m
		}},
		{"dropped call at two with later edit", 2, 2, 5, 6, 3, false, func(m []db.Message) []db.Message {
			m[2].ToolCalls = nil
			return m
		}},
		{"ordinal gap", 2, 2, 5, 6, 0, false, func(m []db.Message) []db.Message {
			return append(m[:2], m[3:]...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const id = "suffix"
			const schema = "agentsview_suffix_test"
			url := testPGURL(t)
			cleanNamedPGSchema(t, url, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			local := testDB(t)
			session := db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 5}
			require.NoError(t, local.UpsertSession(t.Context(), session))
			messages := make([]db.Message, 5)
			for i := range messages {
				messages[i] = db.Message{SessionID: id, Ordinal: i, Role: "assistant", Content: fmt.Sprintf("message %d", i), SourceUUID: fmt.Sprintf("source-%d", i), HasToolUse: true,
					ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: fmt.Sprintf("call-%d", i), InputJSON: `{"path":"file"}`, ResultContent: "initial", ResultContentLength: 7,
						ResultEvents: []db.ToolResultEvent{{EventIndex: 0, ToolUseID: fmt.Sprintf("call-%d", i), Source: "tool_result", Status: "completed", Content: "initial", ContentLength: 7}},
					}},
				}
			}
			messages[0].ToolCalls[0].ResultContent = "initial\n\nsecond"
			messages[0].ToolCalls[0].ResultContentLength = 15
			messages[0].ToolCalls[0].ResultEvents = append(messages[0].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 1, ToolUseID: "call-0", Source: "tool_result", Status: "completed", Content: "second", ContentLength: 6})
			messages[0].ToolCalls = append(messages[0].ToolCalls, db.ToolCall{ToolName: "Bash", Category: "shell", ToolUseID: "second-call", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, ToolUseID: "second-call", Source: "tool_result", Status: "empty"}}})
			require.NoError(t, local.InsertMessages(t.Context(), messages))
			require.NoError(t, local.ReplaceSessionUsageEvents(t.Context(), id, []db.UsageEvent{{SessionID: id, Source: "provider", Model: "sample", InputTokens: 22, DedupKey: "usage"}}))
			syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			result, err := syncer.Push(t.Context(), true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store := &Store{pg: syncer.pg}
			for _, ordinal := range []int{0, 4} {
				_, err := store.PinMessage(t.Context(), "suffix", int64(ordinal), new(fmt.Sprintf("note %d", ordinal)))
				require.NoError(t, err)
			}
			if test.legacyPin {
				_, err = syncer.pg.ExecContext(t.Context(), `UPDATE pinned_messages SET source_uuid='' WHERE session_id='suffix' AND ordinal=0`)
				require.NoError(t, err)
			}
			_, err = syncer.pg.ExecContext(t.Context(), `UPDATE sessions SET updated_at='2000-01-01T00:00:00Z' WHERE id='suffix'`)
			require.NoError(t, err)
			_, beforeVersion, found := store.GetSessionVersion(t.Context(), "suffix")
			require.True(t, found)
			before := suffixPrefixVersions(t, syncer, test.boundary)
			var usageBefore string
			require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT xmin::text FROM usage_events WHERE session_id='suffix'`).Scan(&usageBefore))
			messages = test.change(messages)
			// A later edit must not hide children removed at an earlier ordinal.
			if test.laterEdit > 0 {
				messages[test.laterEdit].Content = "later edit"
			}
			if len(messages) != session.MessageCount {
				session.MessageCount = len(messages)
				require.NoError(t, local.UpsertSession(t.Context(), session))
			}
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "suffix", messages))
			result, err = syncer.Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			var usageAfter string
			require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT xmin::text FROM usage_events WHERE session_id='suffix'`).Scan(&usageAfter))
			assert.Equal(t, usageBefore, usageAfter, "unchanged usage must preserve usage row versions")
			assert.False(t, result.Full)
			_, afterVersion, found := store.GetSessionVersion(t.Context(), "suffix")
			require.True(t, found)
			assert.NotEqual(t, beforeVersion, afterVersion, "suffix changes must wake remote session viewers")
			assert.Equal(t, len(messages)-test.boundary, result.MessagesPushed)
			assert.Equal(t, before, suffixPrefixVersions(t, syncer, test.boundary), "unchanged messages, child IDs, and pins must retain their row versions")
			var calls, events int
			require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM tool_calls WHERE session_id='suffix'`).Scan(&calls))
			require.NoError(t, syncer.pg.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM tool_result_events WHERE session_id='suffix'`).Scan(&events))
			assert.Equal(t, test.calls, calls)
			assert.Equal(t, test.events, events)
			got, err := store.GetMessages(t.Context(), "suffix", 0, 100, true)
			require.NoError(t, err)
			require.Len(t, got, len(messages))
			for i, want := range messages {
				assert.Equal(t, want.Ordinal, got[i].Ordinal)
				assert.Equal(t, want.Content, got[i].Content)
				require.Len(t, got[i].ToolCalls, len(want.ToolCalls))
				for j, call := range want.ToolCalls {
					assert.Equal(t, call.InputJSON, got[i].ToolCalls[j].InputJSON)
					assert.Equal(t, call.ResultContent, got[i].ToolCalls[j].ResultContent)
					require.Len(t, got[i].ToolCalls[j].ResultEvents, len(call.ResultEvents))
					for k, event := range call.ResultEvents {
						assert.Equal(t, event.Content, got[i].ToolCalls[j].ResultEvents[k].Content)
					}
				}
			}
			pins, err := store.ListPinnedMessages(t.Context(), "suffix", "")
			require.NoError(t, err)
			require.Len(t, pins, test.pins)
			ordinals := make([]int, 0, len(pins))
			for _, pin := range pins {
				ordinals = append(ordinals, pin.Ordinal)
				require.NotNil(t, pin.Note)
				assert.Equal(t, fmt.Sprintf("note %d", pin.Ordinal), *pin.Note)
			}
			assert.ElementsMatch(t, []int{0, 4}[:test.pins], ordinals)
		})
	}
}

func TestPushLegacyVSCodeReplyPin(t *testing.T) {
	for _, agent := range []string{"vscode-copilot", "positron"} {
		t.Run(agent, func(t *testing.T) {
			const schema = "agentsview_legacy_reply_pin_test"
			url := testPGURL(t)
			cleanNamedPGSchema(t, url, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
			local := testDB(t)
			require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "s1", Agent: agent, Project: "project", Machine: "machine", MessageCount: 2}))
			messages := []db.Message{
				{SessionID: "s1", Ordinal: 0, Role: "user", Content: "Which release?", Timestamp: "2026-08-06T05:51:31.425Z"},
				{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "Let me check.", Timestamp: "2026-08-06T05:51:31.425Z"},
			}
			require.NoError(t, local.InsertMessages(t.Context(), messages))
			syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			result, err := syncer.Push(t.Context(), true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store := &Store{pg: syncer.pg}
			_, err = store.PinMessage(t.Context(), "s1", 1, new("Keep the release notes"))
			require.NoError(t, err)
			messages[0].SourceUUID = agent + ":user:r1"
			messages[1].SourceUUID = agent + ":assistant:r1"
			messages[1].Content = "[Tool: fetch]\nThe newest release is v2."
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "s1", messages))
			result, err = syncer.Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			pins, err := store.ListPinnedMessages(t.Context(), "s1", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			require.Equal(t, 1, pins[0].Ordinal)
			assert.Equal(t, new("Keep the release notes"), pins[0].Note)
			grown, err := store.GetMessages(t.Context(), "s1", 0, 100, true)
			require.NoError(t, err)
			require.Len(t, grown, 2)
			assert.Equal(t, "[Tool: fetch]\nThe newest release is v2.", grown[pins[0].Ordinal].Content)
		})
	}
}
