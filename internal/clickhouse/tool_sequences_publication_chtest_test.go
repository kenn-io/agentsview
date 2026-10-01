//go:build chtest

package clickhouse

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestToolSequencesPublicationBinding(t *testing.T) {
	for _, change := range []string{"messages", "timing", "shorter", "zero"} {
		t.Run(change, func(t *testing.T) {
			local := dbtest.OpenTestDB(t)
			const id = "tool-sequences-publication"
			dbtest.SeedToolSequencesExample(t, local, id)
			dsn, database := chtest.FreshDatabase(t)
			target := Target{URL: dsn, Database: database}
			syncer := newTestSync(t, local, target, storage.PusherOptions{})
			result, err := syncer.Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			store, err := NewStore(t.Context(), target)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "test"}, store, nil).Handler()
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+id+"/tool-sequences", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				return response
			}
			response := request()
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			binding, pending, err := store.ToolSequenceReadSource(t.Context(), id, true)
			require.NoError(t, err)
			require.NotEmpty(t, binding)
			require.False(t, pending)
			metadata, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			messages, err := local.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			switch change {
			case "messages":
				messages[1].ToolCalls[0].ResultContent = "replacement content"
			case "timing":
				messages[1].ToolCalls[0].ResultEvents[1].Timestamp = "2026-04-26T10:00:05Z"
			case "shorter":
				messages = messages[:1]
			case "zero":
				messages = nil
			}
			session, err := local.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			session.MessageCount = len(messages)
			_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: *session, Messages: messages, ReplaceMessages: true, DataVersion: session.DataVersion}})
			require.NoError(t, err)
			observed := false
			syncer.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error {
				observed = true
				current, err := store.GetSession(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, metadata.TranscriptRevision, current.TranscriptRevision)
				response := request()
				assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), `"code":"source_changed"`)
				assert.NotContains(t, response.Body.String(), `"sequences"`)
				return nil
			}}
			result, err = syncer.Push(t.Context(), true, nil)
			require.NoError(t, err)
			require.Zero(t, result.Errors)
			require.True(t, observed)
			syncer.hooks = nil
			response = request()
			assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if change == "zero" {
				assert.Contains(t, response.Body.String(), `"total_tool_calls":0`)
			}
			if change == "messages" {
				for _, table := range []string{"messages", "tool_calls", "tool_result_events", "sessions"} {
					where := "session_id"
					if table == "sessions" {
						where = "id"
					}
					_, err := store.conn.ExecContext(t.Context(), `INSERT INTO `+table+` SELECT * REPLACE (push_version + 1 AS push_version) FROM `+table+` WHERE `+where+` = ?`, id)
					require.NoError(t, err)
					response := request()
					assert.Equal(t, http.StatusConflict, response.Code, table+": "+response.Body.String())
					assert.NotContains(t, response.Body.String(), `"sequences"`)
					result, err := syncer.Push(t.Context(), true, nil)
					require.NoError(t, err)
					require.Zero(t, result.Errors)
					response = request()
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				}
				_, err := store.conn.ExecContext(t.Context(), `INSERT INTO sessions SELECT * REPLACE (push_version + 1 AS push_version, message_count + 1 AS message_count) FROM sessions WHERE id = ?`, id)
				require.NoError(t, err)
				current, pending, err := store.ToolSequenceReadSource(t.Context(), id, false)
				require.NoError(t, err)
				assert.NotEqual(t, binding, current)
				assert.False(t, pending)
				_, pending, err = store.ToolSequenceReadSource(t.Context(), id, true)
				require.NoError(t, err)
				assert.True(t, pending)
				assert.Equal(t, http.StatusConflict, request().Code)
			}
		})
	}
}
