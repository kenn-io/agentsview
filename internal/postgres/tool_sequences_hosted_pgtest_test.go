//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
)

type toolSequenceAliasSwapStore struct {
	*HostedStore
	afterPage func()
}

func (s *toolSequenceAliasSwapStore) GetMessagesWindow(ctx context.Context, id string, w db.MessageWindow) ([]db.Message, error) {
	messages, err := s.HostedStore.GetMessagesWindow(ctx, id, w)
	if err == nil && s.afterPage != nil {
		s.afterPage()
		s.afterPage = nil
	}
	return messages, err
}

func TestToolSequencesHosted_SourceBinding(t *testing.T) {
	f := newProjectionFixture(t)
	m, accepted := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("first transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	before, err := h.resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	metadata, err := h.GetSession(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, metadata.TranscriptRevision)
	store := &toolSequenceAliasSwapStore{HostedStore: h, afterPage: func() {
		replacement, _ := f.accept(t, "device-a", "capture-b", accepted.Receipt)
		require.NoError(t, f.sink.Project(t.Context(), f.lease(t, replacement), replacement, projectionOutcome("replacement transcript")))
		after, err := h.resolve(t.Context(), "codex:portable")
		require.NoError(t, err)
		require.NotEqual(t, before.SessionID, after.SessionID)
		_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET transcript_revision=$1, termination_status=$2 WHERE id=$3`, *metadata.TranscriptRevision, metadata.TerminationStatus, after.SessionID)
		require.NoError(t, err)
		current, err := h.GetSession(t.Context(), "codex:portable")
		require.NoError(t, err)
		assert.Equal(t, metadata.TranscriptRevision, current.TranscriptRevision)
		assert.Equal(t, metadata.TerminationStatus, current.TerminationStatus)
	}}
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "hosted"}, store, nil).Handler()
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/codex:portable/tool-sequences", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := request()
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"source_changed"`)
	assert.NotContains(t, response.Body.String(), `"sequences"`)
	assert.NotContains(t, response.Body.String(), "raw-row-")
	response = request()
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), "raw-row-")
}

func TestToolSequencesHosted_SelectedTimingAndMapping(t *testing.T) {
	f := newProjectionFixture(t)
	manifest, _ := f.accept(t, "device-a", "capture-a", "")
	outcome := projectionOutcome("delegate tasks")
	parent := &outcome.Outcome.Results[0].Result
	parent.Messages[1].HasToolUse = true
	parent.Messages[1].ToolCalls = nil
	for i := range 40 {
		id := fmt.Sprint("codex:child-", i)
		child := projectionOutcome("child").Outcome.Results[0]
		child.Result.Session.ID = id
		child.Result.Session.SourceSessionID = fmt.Sprint("child-", i)
		child.Result.Session.ParentSessionID = "codex:portable"
		child.Result.Session.RelationshipType = "subagent"
		child.Result.Session.EndedAt = child.Result.Session.StartedAt.Add(time.Duration(i+7) * time.Second)
		parent.Messages[1].ToolCalls = append(parent.Messages[1].ToolCalls, parser.ParsedToolCall{
			ToolUseID: fmt.Sprint("call-", i), ToolName: "Task", Category: "Tool", SubagentSessionID: id,
			ResultEvents: []parser.ParsedToolResultEvent{{Source: "tool_execution", Status: "errored"}},
		})
		outcome.Outcome.Results = append(outcome.Outcome.Results, child)
		parent = &outcome.Outcome.Results[0].Result
	}
	parent.Messages = append(parent.Messages, parser.ParsedMessage{Ordinal: 2, Role: parser.RoleAssistant, HasToolUse: true, ToolCalls: []parser.ParsedToolCall{{ToolUseID: "recovered", ToolName: "Read", Category: "Read", ResultEvents: []parser.ParsedToolResultEvent{{Source: "tool_execution", Status: "completed", Content: "text"}}}}})
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, manifest), manifest, outcome))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	root, err := h.resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	child, err := h.resolve(t.Context(), "codex:child-0")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=clock_timestamp() WHERE id=$1`, child.SessionID)
	require.NoError(t, err)

	positions := []db.ToolCallPosition{{MessageOrdinal: 1}, {MessageOrdinal: 1, CallIndex: 39}, {MessageOrdinal: 2}}
	durations, err := h.GetToolCallDurations(t.Context(), "codex:portable", positions)
	require.NoError(t, err)
	require.Len(t, durations, 3)
	assert.Equal(t, new(int64(7000)), durations[positions[0]])
	assert.Equal(t, new(int64(46000)), durations[positions[1]])
	assert.Nil(t, durations[positions[2]])
	full, err := h.GetSessionTiming(t.Context(), "codex:portable")
	require.NoError(t, err)
	assert.Equal(t, full.Turns[0].Calls[0].DurationMs, durations[positions[0]])
	assert.Equal(t, full.Turns[0].Calls[39].DurationMs, durations[positions[1]])

	refs := hostedRefs{links: []hostedLink{{owner: root.SessionID, alias: "codex:child-0"}, {owner: root.SessionID, alias: "codex:child-0"}}}
	edges, err := refs.linkTargets(t.Context(), h)
	require.NoError(t, err)
	require.Len(t, edges, 1)
	assert.Equal(t, []string{child.SessionID}, edges[root.SessionID+"\x00codex:child-0"])
	var allLinks int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(DISTINCT e.target_alias) `+hostedLinkFromSQL+` AND owner.session_id=$1`, root.SessionID).Scan(&allLinks))
	assert.Equal(t, 40, allLinks)
	messages, err := h.GetMessages(t.Context(), "codex:portable", 1, 1, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "codex:child-0", messages[0].ToolCalls[0].SubagentSessionID)
	assert.Equal(t, "codex:child-39", messages[0].ToolCalls[39].SubagentSessionID)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/codex:portable/tool-sequences", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	server.New(config.Config{Host: "127.0.0.1", InstallationID: "hosted"}, h, nil).Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var document struct {
		SessionID      string `json:"session_id"`
		TotalToolCalls int    `json:"total_tool_calls"`
		OmittedCalls   int    `json:"omitted_calls"`
		Sequences      []struct {
			Calls []struct {
				DurationMs *int64 `json:"duration_ms"`
			} `json:"calls"`
		} `json:"sequences"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &document))
	assert.Equal(t, "codex:portable", document.SessionID)
	assert.Equal(t, 41, document.TotalToolCalls)
	assert.Equal(t, 31, document.OmittedCalls)
	require.Len(t, document.Sequences, 1)
	require.Len(t, document.Sequences[0].Calls, 10)
	assert.Equal(t, new(int64(7000)), document.Sequences[0].Calls[0].DurationMs)
	assert.Nil(t, document.Sequences[0].Calls[9].DurationMs)
}
