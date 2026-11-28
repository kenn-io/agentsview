package server

import (
	"net/http"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDetachedArchiveDirectoryAndResumeRequests(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	bin, err := exec.LookPath("go")
	require.NoError(t, err)
	openerCacheMu.Lock()
	previous, previousTime := openerCache, openerCacheAt
	openerCache = []Opener{{ID: "test-opener", Kind: "editor", Bin: bin}}
	openerCacheAt = time.Now()
	openerCacheMu.Unlock()
	t.Cleanup(func() {
		openerCacheMu.Lock()
		openerCache, openerCacheAt = previous, previousTime
		openerCacheMu.Unlock()
	})
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
			require.NoError(t, f.db.UpsertSession(ctx, db.Session{ID: "claude:local", Agent: "claude", Project: "project", Cwd: path}))
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
			for _, action := range []string{"open", "resume"} {
				t.Run(action, func(t *testing.T) {
					body := map[string]any{}
					if action == "open" {
						body["opener_id"] = "test-opener"
					}
					response := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sessions/claude:local/"+action, body)
					if detached {
						assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
					} else {
						assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
					}
				})
			}
		})
	}
}
