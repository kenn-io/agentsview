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

func suffixFixture(t *testing.T, total int) (*Sync, *db.DB, []db.Message) {
	t.Helper()
	const id = "suffix"
	const schema = "agentsview_suffix_test"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: total}))
	messages := make([]db.Message, total)
	for i := range messages {
		messages[i] = db.Message{SessionID: id, Ordinal: i, Role: "assistant", Content: fmt.Sprintf("message %d", i), SourceUUID: fmt.Sprintf("source-%d", i), HasToolUse: true,
			ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: fmt.Sprintf("call-%d", i), InputJSON: `{"path":"file"}`, ResultContent: "initial", ResultContentLength: 7,
				ResultEvents: []db.ToolResultEvent{{EventIndex: 0, ToolUseID: fmt.Sprintf("call-%d", i), Source: "tool_result", Status: "completed", Content: "initial", ContentLength: 7}},
			}},
		}
	}
	if len(messages) > 0 {
		messages[0].ToolCalls[0].ResultContent = "initial\n\nsecond"
		messages[0].ToolCalls[0].ResultContentLength = 15
		messages[0].ToolCalls[0].ResultEvents = append(messages[0].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 1, ToolUseID: "call-0", Source: "tool_result", Status: "completed", Content: "second", ContentLength: 6})
		messages[0].ToolCalls = append(messages[0].ToolCalls, db.ToolCall{ToolName: "Bash", Category: "shell", ToolUseID: "second-call", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, ToolUseID: "second-call", Source: "tool_result", Status: "empty"}}})
	}
	require.NoError(t, local.InsertMessages(t.Context(), messages))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	return syncer, local, messages
}

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
		name     string
		boundary int
		pins     int
		change   func([]db.Message) []db.Message
	}{
		{"append", 5, 2, func(m []db.Message) []db.Message {
			return append(m, db.Message{SessionID: "suffix", Ordinal: 5, Role: "user", Content: "appended"})
		}},
		{"late result", 4, 2, func(m []db.Message) []db.Message {
			m[4].ToolCalls[0].ResultContent = "initial\n\nlate result"
			m[4].ToolCalls[0].ResultContentLength = 20
			m[4].ToolCalls[0].ResultEvents = append(m[4].ToolCalls[0].ResultEvents, db.ToolResultEvent{EventIndex: 1, ToolUseID: "call-4", Source: "tool_result", Status: "completed", Content: "late result", ContentLength: 11})
			return m
		}},
		{"truncate", 3, 1, func(m []db.Message) []db.Message { return m[:3] }},
		{"edit", 2, 2, func(m []db.Message) []db.Message { m[2].Content = "changed"; return m }},
	} {
		t.Run(test.name, func(t *testing.T) {
			syncer, local, messages := suffixFixture(t, 5)
			store := &Store{pg: syncer.pg}
			for _, ordinal := range []int{0, 4} {
				_, err := store.PinMessage(t.Context(), "suffix", int64(ordinal), new(fmt.Sprintf("note %d", ordinal)))
				require.NoError(t, err)
			}
			_, err := syncer.pg.ExecContext(t.Context(), `UPDATE sessions SET updated_at='2000-01-01T00:00:00Z' WHERE id='suffix'`)
			require.NoError(t, err)
			_, beforeVersion, found := store.GetSessionVersion(t.Context(), "suffix")
			require.True(t, found)
			before := suffixPrefixVersions(t, syncer, test.boundary)
			messages = test.change(messages)
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "suffix", messages))
			result, err := syncer.Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			assert.False(t, result.Full)
			_, afterVersion, found := store.GetSessionVersion(t.Context(), "suffix")
			require.True(t, found)
			assert.NotEqual(t, beforeVersion, afterVersion, "suffix changes must wake remote session viewers")
			assert.Equal(t, len(messages)-test.boundary, result.MessagesPushed)
			assert.Equal(t, before, suffixPrefixVersions(t, syncer, test.boundary), "unchanged messages, child IDs, and pins must retain their row versions")
			got, err := store.GetMessages(t.Context(), "suffix", 0, 100, true)
			require.NoError(t, err)
			require.Len(t, got, len(messages))
			for i, want := range messages {
				assert.Equal(t, want.Content, got[i].Content)
				assert.Equal(t, want.Model, got[i].Model)
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
