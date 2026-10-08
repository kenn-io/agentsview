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
			name: "messages deleted ahead of their session row",
			change: func(t *testing.T, _ *db.DB, target Target, _ *Sync) {
				conn := chtest.Open(t, target.URL, target.Database)
				_, err := conn.ExecContext(t.Context(), "DELETE FROM messages WHERE session_id = ?", fixtureAlphaID)
				require.NoError(t, err)
			},
		},
		{
			name: "one message deleted ahead of its session row",
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

func TestToolSequencesServeSessionsWithoutStoredMessages(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	const id = "ch-metadata-only"
	// Metadata-only and usage-only sessions keep a positive message_count with no stored message rows.
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session:     fixtureSession(id, "alpha", "metadata only", "2026-01-11T00:00:00.000Z", 3),
		DataVersion: 1,
	}})
	require.NoError(t, err)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, store, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+id+"/tool-sequences", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	assert.Equal(t, http.StatusOK, res.Code, res.Body.String())
}

func TestToolSequencesRefuseEmptiedUntimedTranscript(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	const id = "ch-untimed"
	// Untimed messages leave last_message_at null, so only the stored count shows they went missing.
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: fixtureSession(id, "alpha", "untimed", "2026-01-11T00:00:00.000Z", 2),
		Messages: []db.Message{
			fixtureMessage(id, 0, "user", "untimed first", ""),
			fixtureMessage(id, 1, "assistant", "untimed second", "", db.ToolCall{ToolName: "search", Category: "search", ToolUseID: "tool-untimed"}),
		},
		DataVersion: 1,
	}})
	require.NoError(t, err)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, store, nil).Handler()
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+id+"/tool-sequences", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	require.Equal(t, http.StatusOK, get().Code)

	require.NoError(t, local.ReplaceSessionMessages(ctx, id, nil))
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return errors.New("simulated crash") }}
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	res := get()
	assert.Equal(t, http.StatusConflict, res.Code, res.Body.String())
	assert.Contains(t, res.Body.String(), `"code":"source_changed"`)
}

func TestToolSequencesAfterStoredCountUpgrade(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	_, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)

	// Turn the mirror back into one written before stored_message_count existed.
	conn := chtest.Open(t, target.URL, target.Database)
	for _, stmt := range []string{
		"ALTER TABLE sessions DROP COLUMN stored_message_count",
		"ALTER TABLE usage_session_snapshots DROP COLUMN stored_message_count",
	} {
		_, err := conn.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.Error(t, CheckSchemaCompat(ctx, conn))

	require.NoError(t, EnsureSchema(ctx, target))
	require.NoError(t, CheckSchemaCompat(ctx, conn))
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
	res := get()
	assert.Equal(t, http.StatusNotImplemented, res.Code, "an unknown stored count is unavailable: %s", res.Body.String())
	assert.Contains(t, res.Body.String(), `"code":"revision_unavailable"`)

	result, err := newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.True(t, result.Full)
	assert.Equal(t, "publishing stored message counts", result.FullReason)
	assert.Zero(t, result.Errors)
	res = get()
	assert.Equal(t, http.StatusOK, res.Code, res.Body.String())

	result, err = newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Full, "the next push is incremental again")

	// An older writer republishes a session without the count.
	_, err = conn.ExecContext(ctx, "ALTER TABLE sessions UPDATE stored_message_count = NULL WHERE id = ? SETTINGS mutations_sync = 2", fixtureAlphaID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotImplemented, get().Code)
	result, err = newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "publishing stored message counts", result.FullReason)
	res = get()
	assert.Equal(t, http.StatusOK, res.Code, res.Body.String())

	// A repair that republishes the unknown row but fails another session's row keeps repairing on the next push.
	_, err = conn.ExecContext(ctx, "ALTER TABLE sessions UPDATE stored_message_count = NULL WHERE id = ? SETTINGS mutations_sync = 2", fixtureChildID)
	require.NoError(t, err)
	failing := newTestSync(t, local, target, storage.PusherOptions{})
	failing.hooks = &pushHooks{beforeSessionRows: func(batch []db.Session) error {
		for _, sess := range batch {
			if sess.ID == fixtureAlphaID {
				return errors.New("simulated crash before session rows")
			}
		}
		return nil
	}}
	// An explicitly requested full push still records the repair.
	result, err = failing.Push(ctx, true, nil)
	require.NoError(t, err)
	require.True(t, result.Full)
	require.NotZero(t, result.Errors)
	assert.Equal(t, http.StatusConflict, get().Code)
	result, err = newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, "publishing stored message counts", result.FullReason)
	res = get()
	assert.Equal(t, http.StatusOK, res.Code, res.Body.String())
	result, err = newTestSync(t, local, target, storage.PusherOptions{}).Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, result.Full, "a clean repair ends it")
}
