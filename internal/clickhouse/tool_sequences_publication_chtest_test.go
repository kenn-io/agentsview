//go:build chtest

package clickhouse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestToolSequencesRefuseUnpublishedEvidence(t *testing.T) {
	failBeforeSessionRows := &pushHooks{beforeSessionRows: func([]db.Session) error {
		return errors.New("simulated crash before session rows")
	}}
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, local *db.DB, target Target, s *Sync)
	}{
		{
			name: "newer messages without their session row",
			change: func(t *testing.T, local *db.DB, _ Target, s *Sync) {
				appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:30:00.000Z")
				s.hooks = failBeforeSessionRows
				_, err := s.Push(context.Background(), false, nil)
				require.NoError(t, err)
			},
		},
		{
			name: "an emptied transcript without its session row",
			change: func(t *testing.T, local *db.DB, _ Target, s *Sync) {
				require.NoError(t, local.ReplaceSessionMessages(t.Context(), fixtureAlphaID, nil))
				s.hooks = failBeforeSessionRows
				_, err := s.Push(context.Background(), false, nil)
				require.NoError(t, err)
			},
		},
		{
			name: "a message deleted ahead of its session row",
			change: func(t *testing.T, _ *db.DB, target Target, _ *Sync) {
				conn := chtest.Open(t, target.URL, target.Database)
				_, err := conn.ExecContext(t.Context(), "DELETE FROM messages WHERE session_id = ? AND ordinal = 0", fixtureAlphaID)
				require.NoError(t, err)
			},
		},
		{
			name: "a leftover result event from an older push",
			change: func(t *testing.T, _ *db.DB, target Target, _ *Sync) {
				conn := chtest.Open(t, target.URL, target.Database)
				_, err := conn.ExecContext(t.Context(), `INSERT INTO tool_result_events
					(session_id, tool_call_message_ordinal, call_index, event_index, source, status, push_version)
					VALUES (?, 99, 0, 0, 'tool_execution', 'completed', 1)`, fixtureAlphaID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			local, target := seedFixture(t)
			s := newTestSync(t, local, target, storage.PusherOptions{})
			_, err := s.Push(ctx, false, nil)
			require.NoError(t, err)

			store, err := NewStore(ctx, target)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, store, nil).Handler()
			get := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+fixtureAlphaID+"/tool-sequences", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				return res
			}
			require.Equal(t, http.StatusOK, get().Code)

			tc.change(t, local, target, s)

			res := get()
			assert.Equal(t, http.StatusConflict, res.Code, res.Body.String())
			assert.Contains(t, res.Body.String(), `"code":"source_changed"`)
			assert.NotContains(t, res.Body.String(), `"sequences"`)
		})
	}
}

func TestToolSequencesServeAfterPublicationRecovers(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:30:00.000Z")
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return errors.New("simulated crash") }}
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	s.hooks = nil
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, store, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+fixtureAlphaID+"/tool-sequences", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	assert.Equal(t, http.StatusOK, res.Code, res.Body.String())
}
