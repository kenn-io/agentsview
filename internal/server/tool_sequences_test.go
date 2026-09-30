package server_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
}

func TestHandleToolSequences_Example(t *testing.T) {
	te := setup(t)
	const sessionID = "tool-sequences-example"
	dbtest.SeedToolSequencesExample(t, te.db, sessionID)

	w := te.get(t, "/api/v1/sessions/"+sessionID+"/tool-sequences")
	assertStatus(t, w, http.StatusOK)
	got := decode[sessionToolSequencesResponse](t, w)
	assert.Equal(t, sessionID, got.SessionID)
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
	assert.Equal(t, float64(16), call["result_bytes"])
	assert.Contains(t, call, "result_omitted_bytes")
	assert.Equal(t, float64(0), call["result_omitted_bytes"])
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
		{ToolName: "Read", Category: "Read", ToolUseID: "content", InputJSON: `{"path":"x"}`, ResultContent: "text", ResultContentLength: 4,
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "content", Source: "tool_execution", Status: "completed", Content: "text", ContentLength: 4, EventIndex: 0}}},
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
		{ToolName: "Grep", Category: "Grep", ToolUseID: "known-empty", InputJSON: `{}`, ResultContent: "No matches found", ResultContentLength: len("No matches found"),
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "known-empty", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}}},
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
	assert.Equal(t, 512, len(sequence.Calls[0].InputPreview))
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
	te := setup(t)
	duplicate := []db.ToolCall{
		{ToolName: "Grep", Category: "Grep", ToolUseID: "same", InputJSON: `{}`, ResultContent: "No matches found", ResultContentLength: 15,
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "same", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: 15, EventIndex: 0}}},
		{ToolName: "Grep", Category: "Grep", ToolUseID: "same", InputJSON: `{}`, ResultContent: "No matches found", ResultContentLength: 15,
			ResultEvents: []db.ToolResultEvent{{ToolUseID: "same", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: 15, EventIndex: 1}}},
		{ToolName: "Read", Category: "Read", ToolUseID: "", InputJSON: `{}`, ResultContent: "text", ResultContentLength: 4,
			ResultEvents: []db.ToolResultEvent{{Source: "tool_execution", Status: "completed", Content: "text", ContentLength: 4, EventIndex: 0}}},
	}
	seedSequenceSession(t, te.db, "tool-sequences-ambiguous-timing", dbtest.Ptr("clean"), duplicate)
	got := fetchSessionToolSequences(t, te, "tool-sequences-ambiguous-timing")
	require.Len(t, got.Sequences, 1)
	require.Len(t, got.Sequences[0].Calls, 3)
	assert.Nil(t, got.Sequences[0].Calls[0].DurationMs)
	assert.Nil(t, got.Sequences[0].Calls[1].DurationMs)
	assert.Nil(t, got.Sequences[0].Calls[2].DurationMs)
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
	call := db.ToolCall{
		ToolName: "Task", Category: "Tool", ToolUseID: "delegated", SubagentSessionID: childID,
		ResultEvents: []db.ToolResultEvent{{ToolUseID: "delegated", Source: "tool_execution", Status: "errored", EventIndex: 0}},
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
	assert.Nil(t, first.Sequences[0].Calls[0].DurationMs)

	child, err := te.db.GetSession(t.Context(), childID)
	require.NoError(t, err)
	require.NotNil(t, child)
	childEnded := "2026-04-26T10:00:04Z"
	child.EndedAt = &childEnded
	require.NoError(t, te.db.UpsertSession(t.Context(), *child))

	second := fetchSessionToolSequences(t, te, parentID)
	require.Len(t, second.Sequences, 1)
	assert.Equal(t, new(int64(4000)), second.Sequences[0].Calls[0].DurationMs)
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
			{ToolName: "Grep", Category: "Grep", ToolUseID: "start", InputJSON: `{}`, ResultContent: "No matches found", ResultContentLength: len("No matches found"),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "start", Source: "tool_execution", Status: "completed", Content: "No matches found", ContentLength: len("No matches found"), EventIndex: 0}}},
			{ToolName: "Read", Category: "Read", ToolUseID: "withheld", InputJSON: `{}`, ResultContentLength: 42,
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "withheld", Source: "tool_execution", Status: "completed", ContentLength: 42, EventIndex: 1}}},
			{ToolName: "Read", Category: "Read", ToolUseID: "dedup", InputJSON: `{}`, ResultContent: "retained summary", ResultContentLength: len("retained summary"),
				ResultEvents: []db.ToolResultEvent{{ToolUseID: "dedup", Source: "tool_execution", Status: "completed", Content: "retained summary", ContentLength: len("retained summary"), EventIndex: 2}}},
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
	dbtest.SeedToolSequencesExample(t, te.db, "tool-sequences-duckdb")
	source := fetchSessionToolSequences(t, te, "tool-sequences-duckdb")

	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := duckdb.Push(t.Context(), path, te.db, "test-installation", storage.MirrorPushOptions{}, true, nil)
	require.NoError(t, err)
	store, err := duckdb.NewStore(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	mirror := fetchSessionToolSequences(t, te, "tool-sequences-duckdb")
	assert.Equal(t, source, mirror)
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
			assert.Equal(t, 1, store.called["session"])
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

func TestHandleToolSequences_NilTimingIsNotFound(t *testing.T) {
	te := setup(t)
	dbtest.SeedToolSequencesExample(t, te.db, "tool-sequences-nil-timing")
	store := &toolSequenceFailureStore{Store: te.db, noTiming: true}
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	w := te.get(t, "/api/v1/sessions/tool-sequences-nil-timing/tool-sequences")
	assertStatus(t, w, http.StatusNotFound)
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
		for _, nested := range value {
			if containsCostKey(nested) {
				return true
			}
		}
	}
	return false
}

type toolSequenceFailureStore struct {
	db.Store
	fail      string
	noTiming  bool
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
	if ctx.Err() != nil {
		s.cancelled = true
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
	if s.noTiming {
		return nil, nil
	}
	return s.Store.GetSessionTiming(ctx, id)
}

func (s *toolSequenceFailureStore) count(key string) {
	if s.called == nil {
		s.called = make(map[string]int)
	}
	s.called[key]++
}
