package server_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

type sessionToolSequencesResponse struct {
	SessionID          string                `json:"session_id"`
	TranscriptRevision string                `json:"transcript_revision"`
	TotalToolCalls     int                   `json:"total_tool_calls"`
	TotalSequences     int                   `json:"total_sequences"`
	OmittedSequences   int                   `json:"omitted_sequences"`
	TotalSequenceCalls int                   `json:"total_sequence_calls"`
	OmittedCalls       int                   `json:"omitted_calls"`
	Sequences          []sessionToolSequence `json:"sequences"`
}

type sessionToolSequence struct {
	Ending        string                    `json:"ending"`
	Identical     bool                      `json:"identical"`
	NearIdentical bool                      `json:"near_identical"`
	ToolChanged   bool                      `json:"tool_changed"`
	TotalCalls    int                       `json:"total_calls"`
	OmittedCalls  int                       `json:"omitted_calls"`
	Calls         []sessionToolSequenceCall `json:"calls"`
}

type sessionToolSequenceCall struct {
	Ordinal              int    `json:"ordinal"`
	CallIndex            int    `json:"call_index"`
	ToolUseID            string `json:"tool_use_id"`
	ToolName             string `json:"tool_name"`
	Outcome              string `json:"outcome"`
	Repeat               string `json:"repeat"`
	ToolChanged          bool   `json:"tool_changed"`
	DurationMs           *int64 `json:"duration_ms"`
	InputPreview         string `json:"input_preview"`
	InputBytes           int    `json:"input_bytes"`
	InputOmittedBytes    int    `json:"input_omitted_bytes"`
	ResultPreview        string `json:"result_preview"`
	ResultBytes          *int   `json:"result_bytes"`
	ResultOmittedBytes   *int   `json:"result_omitted_bytes"`
	ResultContentUnknown bool   `json:"result_content_unknown"`
	AwaitingSubagent     bool   `json:"awaiting_subagent"`
}

func TestHandleToolSequences_Example(t *testing.T) {
	te := setup(t)
	const sessionID = "tool-sequences-example"
	dbtest.SeedToolSequencesExample(t, te.db, sessionID)

	w := te.get(t, "/api/v1/sessions/"+sessionID+"/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	got := decode[sessionToolSequencesResponse](t, w)
	assert.Equal(t, sessionID, got.SessionID)
	stored, err := te.db.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored.TranscriptRevision)
	assert.Equal(t, *stored.TranscriptRevision, got.TranscriptRevision)
	assert.Equal(t, 3, got.TotalToolCalls)
	assert.Equal(t, 1, got.TotalSequences)
	assert.Zero(t, got.OmittedSequences)
	assert.Equal(t, 3, got.TotalSequenceCalls)
	assert.Zero(t, got.OmittedCalls)
	require.Len(t, got.Sequences, 1)
	sequence := got.Sequences[0]
	assert.Equal(t, "recovered", sequence.Ending)
	assert.True(t, sequence.Identical)
	assert.False(t, sequence.NearIdentical)
	assert.True(t, sequence.ToolChanged)
	assert.Equal(t, 3, sequence.TotalCalls)
	require.Len(t, sequence.Calls, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{
		sequence.Calls[0].Ordinal, sequence.Calls[1].Ordinal, sequence.Calls[2].Ordinal,
	})
	assert.Equal(t, []string{"grep-1", "grep-2", "read-1"}, []string{
		sequence.Calls[0].ToolUseID, sequence.Calls[1].ToolUseID, sequence.Calls[2].ToolUseID,
	})
	assert.Equal(t, "identical", sequence.Calls[1].Repeat)
	assert.True(t, sequence.Calls[2].ToolChanged)
	assert.Equal(t, "No matches found", sequence.Calls[0].ResultPreview)
	assert.Equal(t, new(int64(2000)), sequence.Calls[0].DurationMs)
	assert.Nil(t, sequence.Calls[1].DurationMs)
	assert.Equal(t, new(int64(2000)), sequence.Calls[2].DurationMs)

	var raw any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.False(t, containsCostKey(raw))
	call := raw.(map[string]any)["sequences"].([]any)[0].(map[string]any)["calls"].([]any)[1].(map[string]any)
	assert.Contains(t, call, "duration_ms")
	assert.Nil(t, call["duration_ms"])
	assert.Contains(t, call, "result_bytes")
	assert.InDelta(t, float64(16), call["result_bytes"], 0)
	assert.Contains(t, call, "result_omitted_bytes")
	assert.InDelta(t, float64(0), call["result_omitted_bytes"], 0)
}

func TestHandleToolSequences_NoSequences(t *testing.T) {
	te := setup(t)
	dbtest.SeedSession(t, te.db, "tool-sequences-none", "test")
	got := fetchSessionToolSequences(t, te, "tool-sequences-none")
	assert.Equal(t, 0, got.TotalToolCalls)
	assert.Equal(t, 0, got.TotalSequences)
	assert.NotNil(t, got.Sequences)

	seedSequenceSession(t, te.db, "tool-sequences-isolated", nil, []db.ToolCall{
		{ToolName: "Bash", Category: "Bash", ToolUseID: "unknown", InputJSON: `{}`},
		{
			ToolName: "Read", Category: "Read", ToolUseID: "content",
			InputJSON: `{"path":"x"}`, ResultContent: "text", ResultContentLength: 4,
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "content", Source: "tool_execution", Status: "completed",
				Content: "text", ContentLength: 4, EventIndex: 0,
			}},
		},
	})
	got = fetchSessionToolSequences(t, te, "tool-sequences-isolated")
	assert.Equal(t, 2, got.TotalToolCalls)
	assert.Equal(t, 0, got.TotalSequences)
	assert.Empty(t, got.Sequences)

	parallel := []db.ToolCall{
		{ToolName: "Grep", Category: "Grep", ToolUseID: "parallel-error", InputJSON: `{}`, ResultEvents: []db.ToolResultEvent{{ToolUseID: "parallel-error", Source: "tool_execution", Status: "errored", EventIndex: 0}}},
		{ToolName: "Read", Category: "Read", ToolUseID: "parallel-content", InputJSON: `{}`, ResultContent: "text", ResultContentLength: 4, ResultEvents: []db.ToolResultEvent{{ToolUseID: "parallel-content", Source: "tool_execution", Status: "completed", Content: "text", ContentLength: 4, EventIndex: 1}}},
	}
	const parallelID = "tool-sequences-parallel"
	dbtest.SeedSession(t, te.db, parallelID, "tool-sequences-test", dbtest.WithMessageCounts(2, 1))
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), parallelID, []db.Message{
		{SessionID: parallelID, Ordinal: 0, Role: "user", Content: "parallel", ContentLength: 8, Timestamp: "2026-04-26T10:00:00Z"},
		{SessionID: parallelID, Ordinal: 1, Role: "assistant", Content: "parallel calls", ContentLength: 15, Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true, ToolCalls: parallel},
	}))
	got = fetchSessionToolSequences(t, te, "tool-sequences-parallel")
	require.Len(t, got.Sequences, 1)
	assert.Equal(t, "open", got.Sequences[0].Ending, "parallel calls in one message do not recover each other")

	unknown := []db.ToolCall{
		{
			ToolName: "Grep", Category: "Grep", ToolUseID: "known-empty",
			InputJSON: `{}`, ResultContent: "No matches found",
			ResultContentLength: len("No matches found"),
			ResultEvents: []db.ToolResultEvent{{
				ToolUseID: "known-empty", Source: "tool_execution", Status: "completed",
				Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0,
			}},
		},
		{ToolName: "Bash", Category: "Bash", ToolUseID: "no-result", InputJSON: `{}`},
	}
	seedSequenceSession(t, te.db, "tool-sequences-result-size-unknown", nil, unknown)
	w := te.get(t, "/api/v1/sessions/tool-sequences-result-size-unknown/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	var raw any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	call := raw.(map[string]any)["sequences"].([]any)[0].(map[string]any)["calls"].([]any)[1].(map[string]any)
	assert.Contains(t, call, "result_bytes")
	assert.Nil(t, call["result_bytes"])
	assert.Contains(t, call, "result_omitted_bytes")
	assert.Nil(t, call["result_omitted_bytes"])
}

func TestHandleToolSequences_Termination(t *testing.T) {
	te := setup(t)
	tests := []struct {
		name       string
		status     *string
		endedAt    *string
		wantEnding string
	}{
		{name: "clean despite running timing", status: dbtest.Ptr("clean"), wantEnding: "abandoned"},
		{name: "awaiting user", status: dbtest.Ptr("awaiting_user"), endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "abandoned"},
		{name: "pending despite ended timing", status: dbtest.Ptr("tool_call_pending"), endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "open"},
		{name: "truncated", status: dbtest.Ptr("truncated"), endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "open"},
		{name: "unrecognized", status: dbtest.Ptr("future-status"), endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "open"},
		{name: "empty", endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "open"},
		{name: "empty status", status: dbtest.Ptr(""), endedAt: dbtest.Ptr("2026-04-26T10:00:08Z"), wantEnding: "open"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := fmt.Sprintf("tool-sequences-termination-%d", i)
			call := db.ToolCall{
				ToolName: "Bash", Category: "Bash", ToolUseID: "failed",
				ResultContent: "failed", ResultContentLength: 6,
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "failed", Source: "tool_execution", Status: "errored", Content: "failed", ContentLength: 6, EventIndex: 0}},
			}
			seedSequenceSession(t, te.db, id, tt.status, []db.ToolCall{call}, func(s *db.Session) {
				s.EndedAt = tt.endedAt
			})
			got := fetchSessionToolSequences(t, te, id)
			require.Len(t, got.Sequences, 1)
			assert.Equal(t, tt.wantEnding, got.Sequences[0].Ending)
		})
	}
}

func TestHandleToolSequences_Bounds(t *testing.T) {
	te := setup(t)
	longSequence := make([]db.ToolCall, 12)
	for i := range longSequence {
		name, result := "Grep", "No matches found"
		if i == len(longSequence)-1 {
			name, result = "Read", "recovered content"
		}
		longSequence[i] = db.ToolCall{
			ToolName: name, Category: name, ToolUseID: fmt.Sprintf("attempt-%02d", i),
			InputJSON: strings.Repeat("x", 600), ResultContent: result,
			ResultContentLength: len(result),
			ResultEvents:        []db.ToolResultEvent{{ToolUseID: fmt.Sprintf("attempt-%02d", i), Source: "tool_execution", Status: "completed", Content: result, ContentLength: len(result), EventIndex: 0}},
		}
	}
	seedSequenceSession(t, te.db, "tool-sequences-call-cap", nil, longSequence)
	got := fetchSessionToolSequences(t, te, "tool-sequences-call-cap")
	require.Len(t, got.Sequences, 1)
	sequence := got.Sequences[0]
	assert.Equal(t, 12, sequence.TotalCalls)
	assert.Equal(t, 2, sequence.OmittedCalls)
	assert.Equal(t, 2, got.OmittedCalls)
	assert.Equal(t, "recovered", sequence.Ending)
	require.Len(t, sequence.Calls, 10)
	assert.Equal(t, "attempt-08", sequence.Calls[8].ToolUseID)
	assert.Equal(t, "attempt-11", sequence.Calls[9].ToolUseID)
	assert.Equal(t, 600, sequence.Calls[0].InputBytes)
	assert.Len(t, sequence.Calls[0].InputPreview, 512)
	assert.Equal(t, 88, sequence.Calls[0].InputOmittedBytes)

	sequenceStarts := make([]db.ToolCall, 42)
	for i := range sequenceStarts {
		name, result := "Grep", "No matches found"
		if i%2 == 1 {
			name, result = "Read", "found"
		}
		id := fmt.Sprintf("sequence-%02d", i)
		sequenceStarts[i] = db.ToolCall{
			ToolName: name, Category: name, ToolUseID: id, InputJSON: `{}`,
			ResultContent: result, ResultContentLength: len(result),
			ResultEvents: []db.ToolResultEvent{{ToolUseID: id, Source: "tool_execution", Status: "completed", Content: result, ContentLength: len(result), EventIndex: 0}},
		}
	}
	seedSequenceSession(t, te.db, "tool-sequences-sequence-cap", nil, sequenceStarts)
	got = fetchSessionToolSequences(t, te, "tool-sequences-sequence-cap")
	assert.Equal(t, 21, got.TotalSequences)
	assert.Equal(t, 1, got.OmittedSequences)
	assert.Equal(t, 42, got.TotalSequenceCalls)
	assert.Equal(t, 2, got.OmittedCalls)
	assert.Len(t, got.Sequences, 20)
}

func TestHandleToolSequences_Timing(t *testing.T) {
	t.Run("repeated provider IDs keep each occurrence interval", func(t *testing.T) {
		te := setup(t)
		const id = "tool-sequences-repeated-id"
		const start = "2026-04-26T10:00:00Z"
		calls := make([]db.ToolCall, 12)
		for i := range calls {
			calls[i] = db.ToolCall{ToolName: "Grep", Category: "Grep", ToolUseID: "same", InputJSON: `{}`, ResultContent: "No matches found"}
		}
		for i, end := range []string{"2026-04-26T10:00:02Z", "2026-04-26T10:00:05Z"} {
			calls[i].ResultEvents = []db.ToolResultEvent{
				{ToolUseID: "same", Source: "tool_execution", Status: "started", Timestamp: start, EventIndex: 0},
				{ToolUseID: "same", Source: "tool_execution", Status: "completed", Timestamp: end, Content: "No matches found", EventIndex: 1},
			}
		}
		dbtest.SeedSession(t, te.db, id, "tool-sequences-test", dbtest.WithMessageCounts(1, 0))
		require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Ordinal: 7, Role: "assistant", Timestamp: start, HasToolUse: true, ToolCalls: calls}}))
		got := fetchSessionToolSequences(t, te, id)
		require.Len(t, got.Sequences, 1)
		require.Len(t, got.Sequences[0].Calls, 10)
		assert.Equal(t, 2, got.Sequences[0].OmittedCalls)
		assert.Equal(t, new(int64(2000)), got.Sequences[0].Calls[0].DurationMs)
		assert.Equal(t, new(int64(5000)), got.Sequences[0].Calls[1].DurationMs)
		assert.Equal(t, 11, got.Sequences[0].Calls[9].CallIndex)
	})

	t.Run("blank IDs use same-message occurrence timing", func(t *testing.T) {
		te := setup(t)
		const sessionID = "tool-sequences-blank-id-timing"
		const start = "2026-04-26T10:00:00Z"
		calls := make([]db.ToolCall, 3)
		for i := range calls {
			calls[i] = db.ToolCall{ToolName: "Grep", Category: "Grep", InputJSON: `{}`, ResultContent: "No matches found"}
		}
		for i, end := range []string{"2026-04-26T10:00:02Z", start} {
			calls[i].ResultEvents = []db.ToolResultEvent{
				{Source: "tool_execution", Status: "started", Timestamp: start, EventIndex: 0},
				{Source: "tool_execution", Status: "completed", Timestamp: end, Content: "No matches found", EventIndex: 1},
			}
		}
		dbtest.SeedSession(t, te.db, sessionID, "tool-sequences-test", func(s *db.Session) {
			s.MessageCount = 1
			s.TerminationStatus = new("clean")
		})
		require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), sessionID, []db.Message{{
			SessionID: sessionID, Ordinal: 7, Role: "assistant", Timestamp: start, HasToolUse: true, ToolCalls: calls,
		}}))
		response := fetchSessionToolSequences(t, te, sessionID)
		require.Len(t, response.Sequences, 1)
		require.Len(t, response.Sequences[0].Calls, 3)
		for i, call := range response.Sequences[0].Calls {
			assert.Equal(t, 7, call.Ordinal)
			assert.Equal(t, i, call.CallIndex)
			assert.Empty(t, call.ToolUseID)
		}
		assert.Equal(t, new(int64(2000)), response.Sequences[0].Calls[0].DurationMs)
		assert.Equal(t, new(int64(0)), response.Sequences[0].Calls[1].DurationMs)
		assert.Nil(t, response.Sequences[0].Calls[2].DurationMs)
	})

	t.Run("measured zero stays distinct from null", func(t *testing.T) {
		te := setup(t)
		const timestamp = "2026-04-26T10:00:00Z"
		seedSequenceSession(t, te.db, "tool-sequences-zero", new("clean"), []db.ToolCall{
			{ToolName: "Grep", Category: "Grep", ToolUseID: "zero", ResultContent: "No matches found", ResultEvents: []db.ToolResultEvent{
				{ToolUseID: "zero", Source: "tool_execution", Status: "started", Timestamp: timestamp, EventIndex: 0},
				{ToolUseID: "zero", Source: "tool_execution", Status: "completed", Timestamp: timestamp, Content: "No matches found", EventIndex: 1},
			}},
			{ToolName: "Grep", Category: "Grep", ToolUseID: "unmeasured", ResultContent: "No matches found"},
		})
		response := fetchSessionToolSequences(t, te, "tool-sequences-zero")
		require.Len(t, response.Sequences, 1)
		require.Len(t, response.Sequences[0].Calls, 2)
		assert.Equal(t, new(int64(0)), response.Sequences[0].Calls[0].DurationMs)
		assert.Nil(t, response.Sequences[0].Calls[1].DurationMs)
	})
}

func TestHandleToolSequences_GeneratedClientValidation(t *testing.T) {
	te := setup(t)
	seedSequenceSession(t, te.db, "tool-sequences-empty-evidence", new("clean"), []db.ToolCall{{
		ResultEvents: []db.ToolResultEvent{{Source: "tool_execution", Status: "errored"}},
	}})
	w := te.get(t, "/api/v1/sessions/tool-sequences-empty-evidence/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	var response apiclient.GetAPIV1SessionsIDToolSequencesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Sequences, 1)
	require.Len(t, response.Sequences[0].Calls, 1)
	call := &response.Sequences[0].Calls[0]
	assert.Empty(t, call.InputPreview)
	assert.Empty(t, call.ResultPreview)
	assert.Empty(t, call.ToolUseID)
	assert.Empty(t, call.ToolName)
	require.NoError(t, response.Validate())
	t.Run("required nullable fields survive round trip", func(t *testing.T) {
		var response apiclient.GetAPIV1SessionsIDToolSequencesResponse
		require.NoError(t, json.Unmarshal([]byte(`{"sequences":[{"calls":[{"duration_ms":null,"result_bytes":null,"result_omitted_bytes":null}]}]}`), &response))
		encoded, err := json.Marshal(response)
		require.NoError(t, err)
		var roundTrip struct {
			Sequences []struct {
				Calls []map[string]any `json:"calls"`
			} `json:"sequences"`
		}
		require.NoError(t, json.Unmarshal(encoded, &roundTrip))
		require.Len(t, roundTrip.Sequences, 1)
		require.Len(t, roundTrip.Sequences[0].Calls, 1)
		for _, key := range []string{"duration_ms", "result_bytes", "result_omitted_bytes"} {
			value, present := roundTrip.Sequences[0].Calls[0][key]
			assert.True(t, present, "required nullable key %s", key)
			assert.Nil(t, value, "nullable key %s", key)
		}
	})
	call.Outcome = "invalid"
	require.Error(t, response.Validate())
}

func TestHandleToolSequences_ChildClosureAddsOnlyMeasuredDuration(t *testing.T) {
	te := setup(t)
	const parentID, childID = "tool-sequences-parent-timing", "tool-sequences-child-timing"
	childStart := "2026-04-26T10:00:00Z"
	dbtest.SeedSession(t, te.db, childID, "tool-sequences-test", func(s *db.Session) {
		s.StartedAt = &childStart
		s.MessageCount = 1
		s.ParentSessionID = dbtest.Ptr(parentID)
		s.ParentSessionIDs = []string{parentID}
		s.RelationshipType = "subagent"
	})
	dbtest.SeedSession(t, te.db, parentID, "tool-sequences-test", func(s *db.Session) {
		s.MessageCount = 2
		s.StartedAt = &childStart
	})
	// The spawn itself is timed, as Grok's spawn_subagent is, so the call has a duration before its child closes.
	call := db.ToolCall{
		ToolName: "Task", Category: "Tool", ToolUseID: "delegated", SubagentSessionID: childID,
		ResultEvents: []db.ToolResultEvent{
			{ToolUseID: "delegated", Source: "tool_execution", Status: "started", Timestamp: childStart, EventIndex: 0},
			{ToolUseID: "delegated", Source: "tool_execution", Status: "errored", Timestamp: "2026-04-26T10:00:01Z", EventIndex: 1},
		},
	}
	msgs := []db.Message{
		{SessionID: parentID, Ordinal: 0, Role: "user", Content: "Delegate", ContentLength: 8, Timestamp: childStart},
		{SessionID: parentID, Ordinal: 1, Role: "assistant", Content: "tool call", ContentLength: 9, Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true, ToolCalls: []db.ToolCall{call}},
	}
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), parentID, msgs))
	parentBefore, err := te.db.GetSession(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, parentBefore)
	parentRevision := parentBefore.TranscriptRevision
	require.NotNil(t, parentRevision)

	first := fetchSessionToolSequences(t, te, parentID)
	require.Len(t, first.Sequences, 1)
	assert.Equal(t, "open", first.Sequences[0].Ending)
	assert.Equal(t, new(int64(1000)), first.Sequences[0].Calls[0].DurationMs)
	assert.True(t, first.Sequences[0].Calls[0].AwaitingSubagent)

	child, err := te.db.GetSession(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	childEnded := "2026-04-26T10:00:04Z"
	child.EndedAt = &childEnded
	require.NoError(t, te.db.UpsertSession(t.Context(), *child))

	second := fetchSessionToolSequences(t, te, parentID)
	require.Len(t, second.Sequences, 1)
	assert.Equal(t, new(int64(4000)), second.Sequences[0].Calls[0].DurationMs)
	assert.False(t, second.Sequences[0].Calls[0].AwaitingSubagent)
	parentAfter, err := te.db.GetSession(t.Context(), parentID)
	require.NoError(t, err)
	assert.Equal(t, parentRevision, parentAfter.TranscriptRevision)
}

func TestHandleToolSequences_ScopeAndPresence(t *testing.T) {
	te := setup(t)
	dbtest.SeedToolSequencesExample(t, te.db, "sequence-root")
	const childID = "sequence-child"
	dbtest.SeedSession(t, te.db, childID, "tool-sequences-test", func(s *db.Session) {
		s.MessageCount = 3
		s.ParentSessionID = dbtest.Ptr("sequence-root")
		s.ParentSessionIDs = []string{"sequence-root"}
		s.RelationshipType = "subagent"
	})
	childEmpty := db.ToolCall{
		ToolName: "Grep", Category: "Grep", ToolUseID: "grep-1", InputJSON: `{"pattern":"private child"}`,
		ResultContent: "No matches found", ResultContentLength: len("No matches found"),
		ResultEvents: []db.ToolResultEvent{{ToolUseID: "grep-1", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}},
	}
	childCall := db.ToolCall{
		ToolName: "Read", Category: "Read", ToolUseID: "child-read", InputJSON: `{"file_path":"private child"}`,
		ResultContent: "child-only result", ResultContentLength: len("child-only result"),
		ResultEvents: []db.ToolResultEvent{{ToolUseID: "child-read", Source: "tool_execution", Status: "completed", Content: "child-only result", ContentLength: len("child-only result"), EventIndex: 1}},
	}
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), childID, []db.Message{
		{SessionID: childID, Ordinal: 0, Role: "user", Content: "child", ContentLength: 5, Timestamp: "2026-04-26T10:00:00Z"},
		{SessionID: childID, Ordinal: 1, Role: "assistant", Content: "tool call", ContentLength: 9, Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true, ToolCalls: []db.ToolCall{childEmpty}},
		{SessionID: childID, Ordinal: 2, Role: "assistant", Content: "tool call", ContentLength: 9, Timestamp: "2026-04-26T10:00:02Z", HasToolUse: true, ToolCalls: []db.ToolCall{childCall}},
	}))
	for id, relationship := range map[string]string{"sequence-fork": "fork", "sequence-continuation": "continuation", "sequence-import": "import"} {
		dbtest.SeedToolSequencesExample(t, te.db, id, func(s *db.Session) {
			s.RelationshipType = relationship
			s.FilePath = dbtest.Ptr("/archives/" + id + ".jsonl")
		})
	}
	dbtest.SeedToolSequencesExample(t, te.db, "sequence-source-missing", func(s *db.Session) {
		s.SourceMissingAt = dbtest.Ptr("2026-04-26T12:00:00Z")
	})
	for _, id := range []string{"sequence-root", childID, "sequence-fork", "sequence-continuation", "sequence-import", "sequence-source-missing"} {
		got := fetchSessionToolSequences(t, te, id)
		assert.Equal(t, id, got.SessionID)
		if id == childID {
			assert.Equal(t, 2, got.TotalToolCalls)
			require.Len(t, got.Sequences, 1)
			assert.Equal(t, "child-only result", got.Sequences[0].Calls[1].ResultPreview)
			continue
		}
		assert.Equal(t, 3, got.TotalToolCalls)
	}

	w := te.get(t, "/api/v1/sessions/missing/tool-sequences")
	assertStatus(t, w, http.StatusNotFound)
	dbtest.SeedSession(t, te.db, "sequence-trash", "test")
	require.NoError(t, te.db.SoftDeleteSession(t.Context(), "sequence-trash"))
	w = te.get(t, "/api/v1/sessions/sequence-trash/tool-sequences")
	assertStatus(t, w, http.StatusNotFound)
	dbtest.SeedSession(t, te.db, "sequence-permanently-deleted", "test")
	require.NoError(t, te.db.DeleteSession(t.Context(), "sequence-permanently-deleted"))
	w = te.get(t, "/api/v1/sessions/sequence-permanently-deleted/tool-sequences")
	assertStatus(t, w, http.StatusNotFound)
}

func TestHandleToolSequences_RetainedEvidence(t *testing.T) {
	te := setup(t)
	unknownMarkers := []struct {
		name    string
		content string
	}{
		{name: "image", content: "[image]"},
		{name: "dropped image", content: "[binary content]"},
		{name: "offloaded image", content: "![Image: screenshot](asset://sha256/abc)"},
		{name: "staged", content: "staged:42"},
	}
	for i, marker := range unknownMarkers {
		t.Run(marker.name, func(t *testing.T) {
			id := fmt.Sprintf("tool-sequences-marker-%d", i)
			empty := db.ToolCall{
				ToolName: "Grep", Category: "Grep", ToolUseID: "start", InputJSON: `{}`,
				ResultContent: "No matches found", ResultContentLength: len("No matches found"),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "start", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}},
			}
			call := db.ToolCall{
				ToolName: "Read", Category: "Read", ToolUseID: "marker", InputJSON: `{}`,
				ResultContent: marker.content, ResultContentLength: len(marker.content),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "marker", Source: "tool_execution", Status: "completed", Content: marker.content, ContentLength: len(marker.content), EventIndex: 1}},
			}
			seedSequenceSession(t, te.db, id, nil, []db.ToolCall{empty, call})
			messages, err := te.db.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			rows := ingest.ExtractToolCallRows(messages)
			require.Len(t, rows, 2)
			assert.True(t, rows[1].ResultContentUnknown)
			got := fetchSessionToolSequences(t, te, id)
			require.Len(t, got.Sequences, 1)
			assert.Equal(t, "unknown", got.Sequences[0].Calls[1].Outcome)
			assert.True(t, got.Sequences[0].Calls[1].ResultContentUnknown)
			assert.Equal(t, new(len(marker.content)), got.Sequences[0].Calls[1].ResultBytes)
		})
	}

	t.Run("labelled image and staged summary stays unknown", func(t *testing.T) {
		id := "tool-sequences-labelled-summary"
		start := db.ToolCall{
			ToolName: "Grep", Category: "Grep", ToolUseID: "start", InputJSON: `{}`,
			ResultContent: "No matches found", ResultContentLength: len("No matches found"),
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "start", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}},
		}
		call := db.ToolCall{
			ToolName: "Task", Category: "Tool", ToolUseID: "labelled", InputJSON: `{}`,
			ResultContent:       "agent-a: [image]\n\nagent-b: staged:42",
			ResultContentLength: len("agent-a: [image]\n\nagent-b: staged:42"),
			ResultEvents: []db.ToolResultEvent{
				{ToolUseID: "labelled", AgentID: "agent-a", Source: "tool_execution", Status: "completed", Content: "[image]", ContentLength: len("[image]"), EventIndex: 1},
				{ToolUseID: "labelled", AgentID: "agent-b", Source: "tool_execution", Status: "completed", Content: "staged:42", ContentLength: len("staged:42"), EventIndex: 2},
			},
		}
		seedSequenceSession(t, te.db, id, nil, []db.ToolCall{start, call})
		messages, err := te.db.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		rows := ingest.ExtractToolCallRows(messages)
		require.Len(t, rows, 2)
		assert.True(t, rows[1].ResultContentUnknown)
		assert.Contains(t, rows[1].ResultContent, "agent-a: [image]")
		assert.Contains(t, rows[1].ResultContent, "agent-b: staged:42")
		got := fetchSessionToolSequences(t, te, id)
		require.Len(t, got.Sequences, 1)
		assert.True(t, got.Sequences[0].Calls[1].ResultContentUnknown)
		assert.Equal(t, new(len(rows[1].ResultContent)), got.Sequences[0].Calls[1].ResultBytes)
	})

	t.Run("withheld positive length stays known and deduplicated summary is restored", func(t *testing.T) {
		id := "tool-sequences-retained-length"
		calls := []db.ToolCall{
			{
				ToolName: "Grep", Category: "Grep", ToolUseID: "start", InputJSON: `{}`, ResultContent: "No matches found", ResultContentLength: len("No matches found"),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "start", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}},
			},
			{
				ToolName: "Read", Category: "Read", ToolUseID: "withheld", InputJSON: `{}`, ResultContentLength: 42,
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "withheld", Source: "tool_execution", Status: "completed", ContentLength: 42, EventIndex: 1}},
			},
			{
				ToolName: "Read", Category: "Read", ToolUseID: "dedup", InputJSON: `{}`, ResultContent: "retained summary", ResultContentLength: len("retained summary"),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "dedup", Source: "tool_execution", Status: "completed", Content: "retained summary", ContentLength: len("retained summary"), EventIndex: 2}},
			},
		}
		seedSequenceSession(t, te.db, id, dbtest.Ptr("tool_call_pending"), calls)
		messages, err := te.db.GetAllMessages(t.Context(), id)
		require.NoError(t, err)
		rows := ingest.ExtractToolCallRows(messages)
		require.Len(t, rows, 3)
		assert.Equal(t, 42, db.ResolveResultContentLength(rows[1].ResultContent, rows[1].ResultContentLength))
		assert.False(t, rows[1].ResultContentUnknown)
		assert.Equal(t, "retained summary", rows[2].ResultContent)
		got := fetchSessionToolSequences(t, te, id)
		require.Len(t, got.Sequences, 1)
		assert.Equal(t, new(42), got.Sequences[0].Calls[1].ResultBytes)
		assert.Equal(t, "retained summary", got.Sequences[0].Calls[2].ResultPreview)
	})

	t.Run("empty and late result events retain their outcomes", func(t *testing.T) {
		emptyID := "tool-sequences-known-empty"
		seedSequenceSession(t, te.db, emptyID, dbtest.Ptr("clean"), []db.ToolCall{{
			ToolName: "Grep", Category: "Grep", ToolUseID: "empty", InputJSON: `{}`,
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "empty", Source: "tool_execution", Status: "completed", EventIndex: 0}},
		}})
		empty := fetchSessionToolSequences(t, te, emptyID)
		require.Len(t, empty.Sequences, 1)
		assert.Equal(t, "abandoned", empty.Sequences[0].Ending)
		assert.Equal(t, "empty", empty.Sequences[0].Calls[0].Outcome)
		assert.Equal(t, new(0), empty.Sequences[0].Calls[0].ResultBytes)

		lateID := "tool-sequences-late-content"
		seedSequenceSession(t, te.db, lateID, dbtest.Ptr("clean"), []db.ToolCall{{
			ToolName: "Grep", Category: "Grep", ToolUseID: "late", InputJSON: `{}`,
			ResultContent: "later retained result", ResultContentLength: len("later retained result"),
			ResultEvents: []db.ToolResultEvent{
				{ToolUseID: "late", Source: "tool_execution", Status: "errored", EventIndex: 0},
				{ToolUseID: "late", Source: "tool_execution", Status: "completed", Content: "later retained result", ContentLength: len("later retained result"), EventIndex: 1},
			},
		}})
		late := fetchSessionToolSequences(t, te, lateID)
		assert.Equal(t, 0, late.TotalSequences)
		assert.Equal(t, 1, late.TotalToolCalls)
	})

	t.Run("missing result event and orphan-like call remain unknown", func(t *testing.T) {
		id := "tool-sequences-missing-result"
		seedSequenceSession(t, te.db, id, nil, []db.ToolCall{{
			ToolName: "Bash", Category: "Bash", ToolUseID: "unmatched", InputJSON: `{"cmd":"ls"}`,
		}})
		got := fetchSessionToolSequences(t, te, id)
		assert.Equal(t, 1, got.TotalToolCalls)
		assert.Equal(t, 0, got.TotalSequences)
	})

	t.Run("orphan result event without a call is ignored", func(t *testing.T) {
		id := "tool-sequences-orphan-result"
		seedSequenceSession(t, te.db, id, nil, nil)
		err := te.db.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO tool_result_events
					(session_id, tool_call_message_ordinal, call_index, tool_use_id,
					 source, status, content, content_length, event_index)
				VALUES (?, 0, 0, 'missing-call', 'tool_execution', 'completed',
					 'orphan result', 13, 0)
			`, id)
			return err
		})
		require.NoError(t, err)

		got := fetchSessionToolSequences(t, te, id)
		assert.Equal(t, 0, got.TotalToolCalls)
		assert.Equal(t, 0, got.TotalSequences)
	})
}

func TestHandleToolSequences_ReadOnly(t *testing.T) {
	te := setup(t)
	dbtest.SeedToolSequencesExample(t, te.db, "tool-sequences-read-only")
	before, err := te.db.GetSession(t.Context(), "tool-sequences-read-only")
	require.NoError(t, err)
	w := te.get(t, "/api/v1/sessions/tool-sequences-read-only/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	after, err := te.db.GetSession(t.Context(), "tool-sequences-read-only")
	require.NoError(t, err)
	assert.Equal(t, before.ToolFailureSignalCount, after.ToolFailureSignalCount)
	assert.Equal(t, before.ToolRetryCount, after.ToolRetryCount)
	assert.Equal(t, before.HealthScore, after.HealthScore)
	assert.Equal(t, before.HealthGrade, after.HealthGrade)
	assert.Equal(t, before.TranscriptRevision, after.TranscriptRevision)
}

func TestHandleToolSequences_DuckDBParity(t *testing.T) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		t.Skip("duckdb-go-bindings does not ship a windows/arm64 library")
	}
	te := setup(t)
	sessionIDs := dbtest.SeedToolSequencesParity(t, te.db)
	source := make(map[string]sessionToolSequencesResponse, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		source[sessionID] = fetchSessionToolSequences(t, te, sessionID)
	}
	evidence := source["tool-sequences-parity-evidence"]
	require.Len(t, evidence.Sequences, 2)
	assert.Equal(t, "empty", evidence.Sequences[0].Calls[0].Outcome)
	assert.Equal(t, "single-event summary", evidence.Sequences[0].Calls[1].ResultPreview)
	assert.True(t, evidence.Sequences[1].Calls[1].ResultContentUnknown)
	assert.Equal(t, 4096, *evidence.Sequences[1].Calls[2].ResultBytes)
	incomplete := source["tool-sequences-parity-incomplete"]
	require.Len(t, incomplete.Sequences, 1)
	assert.Equal(t, "open", incomplete.Sequences[0].Ending)
	assert.Empty(t, incomplete.Sequences[0].Calls[0].ToolUseID)
	child := source["tool-sequences-parity-parent"]
	require.Len(t, child.Sequences, 1)
	assert.Equal(t, int64(3000), *child.Sequences[0].Calls[0].DurationMs)
	duplicates := source["tool-sequences-parity-duplicate"]
	require.Len(t, duplicates.Sequences, 1)
	require.Len(t, duplicates.Sequences[0].Calls, 2)
	assert.Equal(t, new(int64(2000)), duplicates.Sequences[0].Calls[0].DurationMs)
	assert.Equal(t, new(int64(5000)), duplicates.Sequences[0].Calls[1].DurationMs)
	streamed := source["tool-sequences-parity-streamed"]
	assert.Equal(t, 154, streamed.TotalToolCalls)
	assert.Equal(t, 144, streamed.OmittedCalls)
	require.Len(t, streamed.Sequences, 1)
	require.Len(t, streamed.Sequences[0].Calls, 10)
	assert.Equal(t, new(int64(2000)), streamed.Sequences[0].Calls[0].DurationMs)
	assert.Equal(t, new(int64(0)), streamed.Sequences[0].Calls[1].DurationMs)
	assert.Equal(t, 260, streamed.Sequences[0].Calls[9].Ordinal)
	assert.Equal(t, new(int64(2000)), streamed.Sequences[0].Calls[9].DurationMs)

	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := duckdb.Push(t.Context(), path, te.db, "test-installation", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	store, err := duckdb.NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	for sessionID, sqlite := range source {
		mirror := fetchSessionToolSequences(t, te, sessionID)
		assert.Equal(t, sqlite, mirror, sessionID)
	}
}

func TestHandleToolSequences_ReadErrors(t *testing.T) {
	for _, failure := range []string{"session", "messages", "timing"} {
		t.Run(failure, func(t *testing.T) {
			te := setup(t)
			dbtest.SeedToolSequencesExample(t, te.db, "tool-sequences-error")
			store := &toolSequenceFailureStore{Store: te.db, fail: failure}
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
			te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
			w := te.get(t, "/api/v1/sessions/tool-sequences-error/tool-sequences")
			assertStatus(t, w, http.StatusInternalServerError)
			assert.GreaterOrEqual(t, store.called["session"], 1)
			if failure != "session" {
				assert.Equal(t, 1, store.called["messages"])
			}
			if failure == "timing" {
				assert.Equal(t, 1, store.called["timing"])
			}
		})
	}

	te := setup(t)
	dbtest.SeedToolSequencesExample(t, te.db, "tool-sequences-cancelled")
	store := &toolSequenceFailureStore{Store: te.db}
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	ctx, cancel := context.WithCancel(t.Context())
	store.cancel = cancel
	_ = te.getWithContext(t, ctx, "/api/v1/sessions/tool-sequences-cancelled/tool-sequences")
	assert.True(t, store.cancelled)
}

func fetchSessionToolSequences(
	t *testing.T, te *testEnv, sessionID string,
) sessionToolSequencesResponse {
	t.Helper()
	w := te.get(t, "/api/v1/sessions/"+sessionID+"/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	return decode[sessionToolSequencesResponse](t, w)
}

func seedSequenceSession(
	t *testing.T,
	d *db.DB,
	sessionID string,
	termination *string,
	calls []db.ToolCall,
	options ...func(*db.Session),
) {
	t.Helper()
	options = append([]func(*db.Session){func(s *db.Session) {
		s.MessageCount = len(calls) + 1
		s.UserMessageCount = 1
		s.StartedAt = dbtest.Ptr("2026-04-26T10:00:00Z")
		s.EndedAt = dbtest.Ptr("2026-04-26T10:01:00Z")
		s.TerminationStatus = termination
	}}, options...)
	dbtest.SeedSession(t, d, sessionID, "tool-sequences-test", options...)
	msgs := []db.Message{{
		SessionID: sessionID, Ordinal: 0, Role: "user", Content: "inspect", ContentLength: 7,
		Timestamp: "2026-04-26T10:00:00Z",
	}}
	for i, call := range calls {
		msg := db.Message{
			SessionID: sessionID, Ordinal: i + 1, Role: "assistant", Content: "tool call",
			ContentLength: 9, Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true,
			ToolCalls: []db.ToolCall{call},
		}
		msgs = append(msgs, msg)
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), sessionID, msgs))
}

func containsCostKey(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, nested := range value {
			if key == "cost" || key == "cost_usd" || key == "cost_source" {
				return true
			}
			if containsCostKey(nested) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, containsCostKey)
	}
	return false
}

type toolSequenceFailureStore struct {
	db.Store
	fail      string
	called    map[string]int
	cancelled bool
	cancel    context.CancelFunc
}

func (s *toolSequenceFailureStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	s.count("session")
	if s.cancel != nil {
		s.cancel()
		s.cancelled = ctx.Err() != nil
		return nil, ctx.Err()
	}
	if s.fail == "session" {
		return nil, errors.New("session read failed")
	}
	return s.Store.GetSession(ctx, id)
}

func (s *toolSequenceFailureStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	s.count("messages")
	if s.fail == "messages" {
		return nil, errors.New("message read failed")
	}
	return s.Store.GetAllMessages(ctx, id)
}

func (s *toolSequenceFailureStore) GetSessionTiming(ctx context.Context, id string) (*db.SessionTiming, error) {
	s.count("timing")
	if s.fail == "timing" {
		return nil, errors.New("timing read failed")
	}
	return s.Store.GetSessionTiming(ctx, id)
}

func (s *toolSequenceFailureStore) count(key string) {
	if s.called == nil {
		s.called = make(map[string]int)
	}
	s.called[key]++
}

// toolSequenceChangingStore runs change after the transcript read, the way a sync lands mid-read.
type toolSequenceChangingStore struct {
	db.Store
	change   func(read int)
	metadata func(*db.Session)
	reads    int
}

func (s *toolSequenceChangingStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	value, err := s.Store.GetSession(ctx, id)
	if s.metadata != nil && value != nil {
		s.metadata(value)
	}
	return value, err
}

func (s *toolSequenceChangingStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	value, err := s.Store.GetAllMessages(ctx, id)
	s.reads++
	if err == nil && s.change != nil {
		s.change(s.reads)
	}
	return value, err
}

func TestHandleToolSequences_SyncDuringRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes int
		status  int
	}{
		{"one sync is retried", 1, http.StatusOK},
		{"a sync on every read conflicts", 2, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := setup(t)
			const id = "tool-sequences-replaced"
			seedSequenceSession(t, te.db, id, new("clean"), []db.ToolCall{
				{ToolName: "Grep", Category: "Grep", ToolUseID: "a", ResultContent: "No matches found"},
				{ToolName: "Grep", Category: "Grep", ToolUseID: "b", ResultContent: "No matches found"},
			})
			store := &toolSequenceChangingStore{Store: te.db, change: func(read int) {
				if read > tc.changes {
					return
				}
				messages, err := te.db.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				messages[2].ToolCalls[0].ResultContent = fmt.Sprint("found it on read ", read)
				require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), id, messages))
			}}
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "test"}
			te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
			response := te.get(t, "/api/v1/sessions/"+id+"/tool-sequences")
			assertStatus(t, response, tc.status)
			assert.Equal(t, 2, store.reads)
			if tc.status == http.StatusConflict {
				assert.Contains(t, response.Body.String(), `"code":"source_changed"`)
				assert.NotContains(t, response.Body.String(), `"sequences"`)
				return
			}
			got := decode[sessionToolSequencesResponse](t, response)
			require.Len(t, got.Sequences, 1)
			assert.Equal(t, "recovered", got.Sequences[0].Ending)
		})
	}
}

func TestHandleToolSequences_ReadBinding(t *testing.T) {
	for _, tc := range []struct {
		change string
		status int
	}{
		{"termination", http.StatusConflict},
		{"disappearance", http.StatusNotFound},
		{"nil revision", http.StatusNotImplemented},
		{"empty revision", http.StatusNotImplemented},
	} {
		t.Run(tc.change, func(t *testing.T) {
			te := setup(t)
			const id = "tool-sequences-binding"
			dbtest.SeedToolSequencesExample(t, te.db, id)
			store := &toolSequenceChangingStore{Store: te.db}
			switch tc.change {
			case "nil revision", "empty revision":
				store.metadata = func(session *db.Session) {
					session.TranscriptRevision = nil
					if tc.change == "empty revision" {
						session.TranscriptRevision = new("")
					}
				}
			case "termination":
				store.change = func(read int) {
					status := []string{"clean", "truncated"}[read%2]
					require.NoError(t, te.db.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `UPDATE sessions SET termination_status=? WHERE id=?`, status, id)
						return err
					}))
				}
			case "disappearance":
				store.change = func(int) { require.NoError(t, te.db.SoftDeleteSession(t.Context(), id)) }
			}
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "test"}
			te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
			response := te.get(t, "/api/v1/sessions/"+id+"/tool-sequences")
			assertStatus(t, response, tc.status)
			assert.NotContains(t, response.Body.String(), `"sequences"`)
			if tc.status == http.StatusNotImplemented {
				assert.Contains(t, response.Body.String(), `"code":"revision_unavailable"`)
			}
		})
	}
}

// toolSequenceBinderStore resolves its session to a source that moves on the listed lookups.
type toolSequenceBinderStore struct {
	db.Store
	moves   map[int]bool
	fail    bool
	lookups int
}

var errToolSequenceMoved = errors.New("source moved")

func (s *toolSequenceBinderStore) SessionSourceBinding(context.Context, string) (string, error) {
	s.lookups++
	if s.fail {
		return "", errToolSequenceMoved
	}
	if s.moves[s.lookups] {
		return "moved", nil
	}
	return "binding", nil
}

func (s *toolSequenceBinderStore) SessionSourceChanged(err error) bool {
	return errors.Is(err, errToolSequenceMoved)
}

func TestHandleToolSequences_SourceBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		moves   map[int]bool
		fail    bool
		status  int
		lookups int
	}{
		{"steady source", nil, false, http.StatusOK, 2},
		{"one move is retried", map[int]bool{2: true}, false, http.StatusOK, 4},
		{"moving on every read conflicts", map[int]bool{2: true, 4: true}, false, http.StatusConflict, 4},
		{"a store-reported move conflicts", nil, true, http.StatusConflict, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := setup(t)
			const id = "tool-sequences-source-binding"
			dbtest.SeedToolSequencesExample(t, te.db, id)
			store := &toolSequenceBinderStore{Store: te.db, moves: tc.moves, fail: tc.fail}
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "test"}
			te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
			response := te.get(t, "/api/v1/sessions/"+id+"/tool-sequences")
			assertStatus(t, response, tc.status)
			assert.Equal(t, tc.lookups, store.lookups)
			if tc.status == http.StatusConflict {
				assert.Contains(t, response.Body.String(), `"code":"source_changed"`)
			}
		})
	}
}
