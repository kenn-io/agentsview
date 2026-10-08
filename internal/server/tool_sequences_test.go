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

	var raw any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.False(t, containsCostKey(raw))
	call := raw.(map[string]any)["sequences"].([]any)[0].(map[string]any)["calls"].([]any)[1].(map[string]any)
	assert.NotContains(t, call, "duration_ms")
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
		require.NoError(t, json.Unmarshal([]byte(`{"sequences":[{"calls":[{"result_bytes":null,"result_omitted_bytes":null}]}]}`), &response))
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
		for _, key := range []string{"result_bytes", "result_omitted_bytes"} {
			value, present := roundTrip.Sequences[0].Calls[0][key]
			assert.True(t, present, "required nullable key %s", key)
			assert.Nil(t, value, "nullable key %s", key)
		}
	})
	call.Outcome = "invalid"
	require.Error(t, response.Validate())
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
	for _, id := range []string{"sequence-root", childID} {
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
}

func TestHandleToolSequences_RetainedEvidence(t *testing.T) {
	te := setup(t)
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

	t.Run("a late completed event replaces an earlier error", func(t *testing.T) {
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
	streamed := source["tool-sequences-parity-streamed"]
	assert.Equal(t, 154, streamed.TotalToolCalls)
	assert.Equal(t, 144, streamed.OmittedCalls)
	require.Len(t, streamed.Sequences, 1)
	require.Len(t, streamed.Sequences[0].Calls, 10)
	assert.Equal(t, 260, streamed.Sequences[0].Calls[9].Ordinal)

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
	for _, failure := range []string{"session", "messages"} {
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
