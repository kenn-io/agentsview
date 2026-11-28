package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDetachedArchiveDirectoryAndResumeRequests(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, detached := range []bool{false, true} {
		name := "ordinary archive"
		if detached {
			name = "detached copy"
		}
		t.Run(name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			ctx := t.Context()
			path := t.TempDir()
			f.srv.cfg.Terminal.Mode = "clipboard"
			for _, id := range []string{"claude:local", "remote~claude:remote"} {
				require.NoError(t, f.db.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "project", Cwd: path}))
				require.NoError(t, f.db.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "hello"}}))
			}
			if detached {
				require.NoError(t, f.db.ClearHostSources(ctx))
			}
			require.NoError(t, f.db.EnableArchiveOnly(ctx))
			if detached {
				assigned := serveJSON(t, f.handler, http.MethodPut, "/api/v1/settings/session-project-assignments/claude:local", map[string]string{"project": path})
				require.Equal(t, http.StatusOK, assigned.Code, assigned.Body.String())
			}
			directory := serveJSON(t, f.handler, http.MethodGet, "/api/v1/sessions/claude:local/directory", nil)
			require.Equal(t, http.StatusOK, directory.Code, directory.Body.String())
			if detached {
				assert.Empty(t, decodeRecorder[sessionDirectoryResponse](t, directory).Path)
			} else {
				assert.Equal(t, path, decodeRecorder[sessionDirectoryResponse](t, directory).Path)
			}
			for _, tc := range []struct {
				name, id string
				body     map[string]any
			}{
				{"resume", "claude:local", map[string]any{}},
				{"command only", "claude:local", map[string]any{"command_only": true}},
				{"remote command", "remote~claude:remote", map[string]any{"command_only": true}},
				{"message fork", "claude:local", map[string]any{"command_only": true, "fork_session": true, "from_ordinal": 0}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					response := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/"+tc.id+"/resume", tc.body)
					if detached {
						assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
						assert.NotContains(t, response.Body.String(), "claude --")
					} else {
						assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
					}
				})
			}
		})
	}
}
