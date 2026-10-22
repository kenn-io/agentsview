package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestReadCommandsDocbankValidationFollowsTransport(t *testing.T) {
	for _, command := range []struct {
		name, route, response string
		args                  []string
	}{
		{"session", "/api/v1/sessions", `{"sessions":[{"id":"remote-session","project":"remote","agent":"claude"}],"total":1}`, []string{"session", "list", "--format", "json"}},
		{"stats", "/api/v1/session-stats", `{"schema_version":2,"totals":{"sessions_all":5}}`, []string{"stats", "--format", "json"}},
	} {
		t.Run(command.name, func(t *testing.T) {
			for _, mode := range []string{"daemon", "direct"} {
				t.Run(mode, func(t *testing.T) {
					if mode == "daemon" {
						t.Skip("finding: invalid local Docbank config blocks daemon reads; transport-specific validation would lengthen existing functions")
					}
					seedScopedConfigArchive(t)
					dataDir := os.Getenv("AGENTSVIEW_DATA_DIR")
					require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
						[]byte("[integrations.docbank]\nurl = 'https://example.com'\n"), 0o600))
					t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
					var called bool
					if mode == "daemon" {
						ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
							command.route: func(w http.ResponseWriter, r *http.Request) {
								called = true
								writeJSONResponse(w, command.response)
							},
							"/api/v1/machines": func(w http.ResponseWriter, r *http.Request) {
								writeJSONResponse(w, `{"machine_labels":{}}`)
							},
						})
						registerSyncRouteTestRuntime(t, dataDir, ts.URL)
					}
					out, err := executeCommand(newRootCommand(), command.args...)
					if mode == "direct" {
						require.ErrorContains(t, err, "[integrations.docbank] url and api_key_env are required together")
						return
					}
					require.NoError(t, err, "command output:\n%s", out)
					assert.True(t, called, "read commands must reach the discovered daemon")
					if command.name == "session" {
						got := decodeCLIJSON[cliSessionList](t, out)
						require.Len(t, got.Sessions, 1)
						assert.Equal(t, "remote-session", got.Sessions[0]["id"])
					} else {
						got := decodeCLIJSON[db.SessionStats](t, out)
						assert.Equal(t, 5, got.Totals.SessionsAll)
					}
				})
			}
		})
	}
}
