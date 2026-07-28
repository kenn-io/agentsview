package db

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func smallArtifactExportLoadLimits() ArtifactExportLoadLimits {
	return ArtifactExportLoadLimits{
		Messages: 2, UsageEvents: 2,
		MessageToolCalls: 2, ToolResultEvents: 2,
		SessionToolCalls: 3, SessionResultEvents: 3,
		MessageBytes: 1 << 20, UsageBytes: 1 << 20,
	}
}

func TestLoadArtifactExportDataCardinalityBoundaries(t *testing.T) {
	t.Run("messages", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{
			artifactExportLoadMessage(0),
			artifactExportLoadMessage(1),
		}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		assert.Len(t, data.Messages, 2)

		require.NoError(t, database.ReplaceSessionMessages("session", []Message{
			artifactExportLoadMessage(0),
			artifactExportLoadMessage(1),
			artifactExportLoadMessage(2),
		}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("usage events", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionUsageEvents("session", []UsageEvent{
			artifactExportLoadUsageEvent(0),
			artifactExportLoadUsageEvent(1),
		}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		assert.Len(t, data.UsageEvents, 2)

		require.NoError(t, database.ReplaceSessionUsageEvents("session", []UsageEvent{
			artifactExportLoadUsageEvent(0),
			artifactExportLoadUsageEvent(1),
			artifactExportLoadUsageEvent(2),
		}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("tool calls per message", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "calls",
			ToolCalls: []ToolCall{
				artifactExportLoadToolCall(0),
				artifactExportLoadToolCall(1),
			},
		}}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		assert.Len(t, data.Messages[0].ToolCalls, 2)

		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "calls",
			ToolCalls: []ToolCall{
				artifactExportLoadToolCall(0),
				artifactExportLoadToolCall(1),
				artifactExportLoadToolCall(2),
			},
		}}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("tool calls per session", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{
			{
				SessionID: "session", Ordinal: 0, Role: "assistant", Content: "first",
				ToolCalls: []ToolCall{
					artifactExportLoadToolCall(0),
					artifactExportLoadToolCall(1),
				},
			},
			{
				SessionID: "session", Ordinal: 1, Role: "assistant", Content: "second",
				ToolCalls: []ToolCall{artifactExportLoadToolCall(0)},
			},
		}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		require.Len(t, data.Messages, 2)
		assert.Len(t, data.Messages[0].ToolCalls, 2)
		assert.Len(t, data.Messages[1].ToolCalls, 1)

		require.NoError(t, database.ReplaceSessionMessages("session", []Message{
			{
				SessionID: "session", Ordinal: 0, Role: "assistant", Content: "first",
				ToolCalls: []ToolCall{
					artifactExportLoadToolCall(0),
					artifactExportLoadToolCall(1),
				},
			},
			{
				SessionID: "session", Ordinal: 1, Role: "assistant", Content: "second",
				ToolCalls: []ToolCall{
					artifactExportLoadToolCall(0),
					artifactExportLoadToolCall(1),
				},
			},
		}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("result events per call", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		call := artifactExportLoadToolCall(0)
		call.ResultEvents = []ToolResultEvent{
			artifactExportLoadResultEvent(0),
			artifactExportLoadResultEvent(1),
		}
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "results",
			ToolCalls: []ToolCall{call},
		}}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		require.Len(t, data.Messages[0].ToolCalls, 1)
		assert.Len(t, data.Messages[0].ToolCalls[0].ResultEvents, 2)

		call.ResultEvents = append(
			call.ResultEvents, artifactExportLoadResultEvent(2),
		)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "results",
			ToolCalls: []ToolCall{call},
		}}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("result events per session", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		first := artifactExportLoadToolCall(0)
		first.ResultEvents = []ToolResultEvent{
			artifactExportLoadResultEvent(0),
			artifactExportLoadResultEvent(1),
		}
		second := artifactExportLoadToolCall(1)
		second.ResultEvents = []ToolResultEvent{artifactExportLoadResultEvent(0)}
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "results",
			ToolCalls: []ToolCall{first, second},
		}}))
		data, err := database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		require.Len(t, data.Messages[0].ToolCalls, 2)
		assert.Len(t, data.Messages[0].ToolCalls[0].ResultEvents, 2)
		assert.Len(t, data.Messages[0].ToolCalls[1].ResultEvents, 1)

		second.ResultEvents = append(
			second.ResultEvents, artifactExportLoadResultEvent(1),
		)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "assistant", Content: "results",
			ToolCalls: []ToolCall{first, second},
		}}))
		_, err = database.LoadArtifactExportData(
			t.Context(), "session", smallArtifactExportLoadLimits(),
		)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})
}

func TestLoadArtifactExportDataRawByteBoundaries(t *testing.T) {
	t.Run("message data", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "user", Content: "abcd",
		}}))
		limits := smallArtifactExportLoadLimits()
		limits.MessageBytes = 8
		data, err := database.LoadArtifactExportData(t.Context(), "session", limits)
		require.NoError(t, err)
		assert.Len(t, data.Messages, 1)

		limits.MessageBytes = 7
		_, err = database.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("usage data", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionUsageEvents("session", []UsageEvent{{
			SessionID: "session", Source: "src", Model: "model",
		}}))
		limits := smallArtifactExportLoadLimits()
		limits.UsageBytes = 8
		data, err := database.LoadArtifactExportData(t.Context(), "session", limits)
		require.NoError(t, err)
		assert.Len(t, data.UsageEvents, 1)

		limits.UsageBytes = 7
		_, err = database.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("tool call data", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0,
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "file", InputJSON: "abcd",
			}},
		}}))
		limits := smallArtifactExportLoadLimits()
		limits.MessageBytes = 12
		data, err := database.LoadArtifactExportData(t.Context(), "session", limits)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		assert.Len(t, data.Messages[0].ToolCalls, 1)

		limits.MessageBytes = 11
		_, err = database.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("result event data", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0,
			ToolCalls: []ToolCall{{
				ResultEvents: []ToolResultEvent{{
					Source: "src", Status: "ok", Content: "abc",
				}},
			}},
		}}))
		limits := smallArtifactExportLoadLimits()
		limits.MessageBytes = 8
		data, err := database.LoadArtifactExportData(t.Context(), "session", limits)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		require.Len(t, data.Messages[0].ToolCalls, 1)
		assert.Len(t, data.Messages[0].ToolCalls[0].ResultEvents, 1)

		limits.MessageBytes = 7
		_, err = database.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})

	t.Run("message and nested aggregate", func(t *testing.T) {
		database := artifactExportLoadTestDB(t)
		require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
			SessionID: "session", Ordinal: 0, Role: "user", Content: "a",
			ToolCalls: []ToolCall{{
				ToolName: "Read", Category: "file",
				ResultEvents: []ToolResultEvent{{
					Source: "src", Status: "ok", Content: "zz",
				}},
			}},
		}}))
		limits := smallArtifactExportLoadLimits()
		limits.MessageBytes = 20
		data, err := database.LoadArtifactExportData(t.Context(), "session", limits)
		require.NoError(t, err)
		require.Len(t, data.Messages, 1)
		require.Len(t, data.Messages[0].ToolCalls, 1)
		assert.Len(t, data.Messages[0].ToolCalls[0].ResultEvents, 1)

		limits.MessageBytes = 19
		_, err = database.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})
}

func TestLoadArtifactExportDataDoesNotHydrateMismatchedNestedRows(t *testing.T) {
	database := artifactExportLoadTestDB(t)
	require.NoError(t, database.UpsertSession(Session{
		ID: "foreign", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.ReplaceSessionMessages("session", []Message{{
		SessionID: "session", Ordinal: 0, Role: "user", Content: "message",
	}}))
	message, err := database.GetMessageByOrdinal("session", 0)
	require.NoError(t, err)
	require.NotNil(t, message)
	_, err = database.getWriter().Exec(`
		INSERT INTO tool_calls(
			message_id, session_id, tool_name, category, input_json, call_index
		) VALUES (?, 'foreign', 'Read', 'file', ?, 0)`,
		message.ID, strings.Repeat("x", 2<<20),
	)
	require.NoError(t, err)

	data, err := database.LoadArtifactExportData(
		t.Context(), "session", smallArtifactExportLoadLimits(),
	)
	require.NoError(t, err)
	require.Len(t, data.Messages, 1)
	assert.Empty(t, data.Messages[0].ToolCalls,
		"nested rows from a different stored session must not bypass preflight")
}

func TestLoadArtifactExportDataAllocationIsBoundedByLimit(t *testing.T) {
	limits := smallArtifactExportLoadLimits()
	makeDatabase := func(t *testing.T, count int) *DB {
		t.Helper()
		database := artifactExportLoadTestDB(t)
		messages := make([]Message, count)
		for i := range messages {
			messages[i] = artifactExportLoadMessage(i)
		}
		require.NoError(t, database.ReplaceSessionMessages("session", messages))
		return database
	}
	small := makeDatabase(t, 3)
	large := makeDatabase(t, 10_000)

	smallAllocs := testing.AllocsPerRun(5, func() {
		_, err := small.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})
	largeAllocs := testing.AllocsPerRun(5, func() {
		_, err := large.LoadArtifactExportData(t.Context(), "session", limits)
		require.ErrorIs(t, err, ErrArtifactExportLimit)
	})
	assert.LessOrEqual(t, largeAllocs, smallAllocs+50,
		"preflight allocation must remain bounded by limit, not session cardinality")
}

func artifactExportLoadTestDB(t *testing.T) *DB {
	t.Helper()
	database := testDB(t)
	require.NoError(t, database.UpsertSession(Session{
		ID: "session", Project: "project", Machine: "local", Agent: "claude",
	}))
	return database
}

func artifactExportLoadMessage(ordinal int) Message {
	return Message{
		SessionID: "session", Ordinal: ordinal, Role: "user", Content: "message",
	}
}

func artifactExportLoadUsageEvent(index int) UsageEvent {
	return UsageEvent{
		SessionID: "session", Source: "source", Model: "model",
		DedupKey: "event-" + strconv.Itoa(index),
	}
}

func artifactExportLoadToolCall(index int) ToolCall {
	return ToolCall{
		SessionID: "session", ToolName: "Read", Category: "file",
		ToolUseID: "call-" + strconv.Itoa(index), CallIndex: index,
	}
}

func artifactExportLoadResultEvent(index int) ToolResultEvent {
	return ToolResultEvent{
		Source: "tool", Status: "completed", Content: "result",
		EventIndex: index,
	}
}
