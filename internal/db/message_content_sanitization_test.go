package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveMessageWritesSanitizeContent(t *testing.T) {
	for _, mode := range []string{"batch", "incremental"} {
		t.Run(mode, func(t *testing.T) {
			database := testDB(t)
			const sessionID = "sanitized-content"
			messages := []Message{{
				SessionID: sessionID, Ordinal: 0, Role: "assistant",
				Content: "a\x00b\u0085c", ThinkingText: "x\x01y", ContentLength: 10,
				TokenUsage: []byte("{\"output_tokens\":2}\x00"),
			}}
			if mode == "batch" {
				result, err := database.WriteSessionBatchContext(t.Context(), []SessionBatchWrite{{
					Session:  Session{ID: sessionID, Project: "project-a", Machine: defaultMachine, Agent: defaultAgent},
					Messages: messages, DataVersion: CurrentDataVersion(),
				}})
				require.NoError(t, err)
				require.Equal(t, 1, result.WrittenSessions)
			} else {
				insertSession(t, database, sessionID, "project-a")
				require.NoError(t, database.WriteSessionIncremental(sessionID, messages, IncrementalSessionUpdate{MsgCount: 1, NextOrdinal: 1}))
			}

			stored, err := database.GetMessages(t.Context(), sessionID, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			assert.Equal(t, "abc", stored[0].Content)
			assert.Equal(t, "xy", stored[0].ThinkingText)
			assert.JSONEq(t, `{"output_tokens":2}`, string(stored[0].TokenUsage))
			if mode == "batch" {
				assert.Equal(t, 6, stored[0].ContentLength)
			}
			assert.Equal(t, "a\x00b\u0085c", messages[0].Content)
			assert.Equal(t, "x\x01y", messages[0].ThinkingText)
		})
	}
}
