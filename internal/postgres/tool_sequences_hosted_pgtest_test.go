//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"
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

type toolSequenceExhaustedStore struct {
	*HostedStore
	fail       string
	err        error
	pageRead   bool
	timingRead bool
}

func (s *toolSequenceExhaustedStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	if s.fail == "initial session" || (s.fail == "session after hydration" && s.pageRead) || (s.fail == "final session" && s.timingRead) {
		return nil, s.err
	}
	return s.HostedStore.GetSession(ctx, id)
}

func (s *toolSequenceExhaustedStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	s.pageRead = true
	if s.fail == "messages" {
		return nil, s.err
	}
	return s.HostedStore.GetAllMessages(ctx, id)
}

func (s *toolSequenceExhaustedStore) GetSessionTiming(ctx context.Context, id string) (*db.SessionTiming, error) {
	s.timingRead = true
	if s.fail == "timing" {
		return nil, s.err
	}
	return s.HostedStore.GetSessionTiming(ctx, id)
}

func TestToolSequencesHosted_ExhaustedReadBinding(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("transcript")))
	h, err := NewHostedStore(f.dsn, f.schema, f.tenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	attempts := 0
	_, exhausted := hostedRead(t.Context(), h, func(hostedRevision) (bool, error) {
		attempts++
		_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1`)
		return true, err
	})
	require.Equal(t, 3, attempts)
	require.ErrorIs(t, exhausted, ErrHostedIdentityChanged)
	wrapped := fmt.Errorf("enclosed hosted read: %w", exhausted)
	assert.True(t, h.SessionSourceChanged(wrapped))
	assert.False(t, h.SessionSourceChanged(errors.New("ordinary read error")))
	assert.False(t, h.SessionSourceChanged(nil))
	for _, boundary := range []string{"initial session", "session after hydration", "final session", "messages", "timing"} {
		t.Run(boundary, func(t *testing.T) {
			store := &toolSequenceExhaustedStore{HostedStore: h, fail: boundary, err: wrapped}
			handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "hosted"}, store, nil).Handler()
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/codex:portable/tool-sequences", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"source_changed"`)
			assert.NotContains(t, response.Body.String(), `"sequences"`)
			assert.NotContains(t, response.Body.String(), `"total_tool_calls"`)
		})
	}
}

type toolSequenceAliasSwapStore struct {
	*HostedStore
	afterPage func()
	reads     int
}

func (s *toolSequenceAliasSwapStore) GetAllMessages(ctx context.Context, id string) ([]db.Message, error) {
	s.reads++
	messages, err := s.HostedStore.GetAllMessages(ctx, id)
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
	// The alias moves during the first read, so the route rereads the replacement once instead of answering with a conflict.
	response := request()
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 2, store.reads)
	assert.NotContains(t, response.Body.String(), "raw-row-")
}

func TestToolSequencesHosted_TimingAndMapping(t *testing.T) {
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
	child, err := h.resolve(t.Context(), "codex:child-0")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE sessions SET deleted_at=clock_timestamp() WHERE id=$1`, child.SessionID)
	require.NoError(t, err)

	full, err := h.GetSessionTiming(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.Len(t, full.Turns[0].Calls, 40)
	assert.Equal(t, new(int64(7000)), full.Turns[0].Calls[0].DurationMs)

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
