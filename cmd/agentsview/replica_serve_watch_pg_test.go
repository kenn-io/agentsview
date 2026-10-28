//go:build pgtest

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestReplicaServeWatchPublication(t *testing.T) {
	dsn := os.Getenv("TEST_PG_URL")
	if dsn == "" {
		t.Skip("TEST_PG_URL not set; requires a dedicated test database")
	}
	for _, embed := range []bool{false, true} {
		t.Run(fmt.Sprintf("embed=%t", embed), func(t *testing.T) {
			dir := testDataDir(t)
			clearConfiguredAgentEnvVars(t)
			isolateDefaultAgentDirs(t, t.TempDir())
			schema := fmt.Sprintf("serve_watch_%d", time.Now().UnixNano())
			t.Setenv("AGENTSVIEW_PG_URL", dsn)
			admin, err := postgres.Open(dsn, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
				assert.NoError(t, err)
				assert.NoError(t, admin.Close())
			})
			root := t.TempDir()
			project := filepath.Join(root, "-workspace-project")
			require.NoError(t, os.MkdirAll(project, 0o700))
			transcript := filepath.Join(project, "combined-session.jsonl")
			initial := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID(
				"2026-01-01T00:00:00Z", "initial mounted content", "combined-session", "/workspace/project",
			).String()
			require.NoError(t, os.WriteFile(transcript, []byte(initial), 0o600))
			writeTestConfig(t, dir, fmt.Sprintf(`[[session_sources]]
agent = "claude"
dir = %q
[pg]
schema = %q
allow_insecure = true
`, root, schema))
			cfg, _, err := loadPGServeConfigForTest(t, "--port=0", "--no-browser")
			require.NoError(t, err)
			if embed {
				cfg.Vector = vectorTestConfig(cfg.DataDir).Vector
				cfg.Vector.Embed.BackstopInterval = "1s"
				endpoint := newEmbeddingsStubServer(t, 3)
				t.Cleanup(endpoint.Close)
				server := cfg.Vector.Embeddings.Servers["local"]
				server.Endpoint = endpoint.URL + "/v1"
				cfg.Vector.Embeddings.Servers["local"] = server
			}
			startup, err := prepareReplicaServeImpl(pgReplica{}, cfg, "", &ReplicaPushConfig{Embed: embed, Debounce: time.Second, Interval: time.Second})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(startup.ctx)
			startup.ctx = ctx
			finished := make(chan error, 1)
			go func() { finished <- runPreparedReplicaServe("pg", startup) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-finished:
					assert.NoError(t, err)
				case <-time.After(40 * time.Second):
					assert.Fail(t, "serve did not join watch")
				}
			})
			client := &http.Client{Timeout: 10 * time.Second}
			sessionURL := fmt.Sprintf("http://%s:%d/api/v1/sessions/combined-session/messages", startup.cfg.Host, startup.cfg.Port)
			published := func(content string) bool {
				response, err := client.Get(sessionURL)
				if err != nil {
					return false
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				return response.StatusCode == http.StatusOK && strings.Contains(string(body), content)
			}
			require.Eventually(t, func() bool { return published("initial mounted content") }, 30*time.Second, time.Second)
			file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = file.WriteString(testjsonl.NewSessionBuilder().AddClaudeAssistant("2026-01-01T00:01:00Z", "appended mounted content").String())
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.Eventually(t, func() bool { return published("appended mounted content") }, 30*time.Second, time.Second)
			if embed {
				searchURL := fmt.Sprintf("http://%s:%d/api/v1/search/content?pattern=%s&mode=semantic&limit=5&include_one_shot=true", startup.cfg.Host, startup.cfg.Port, url.QueryEscape("initial mounted content"))
				require.Eventually(t, func() bool {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, searchURL, nil)
					require.NoError(t, err)
					req.Header.Set("X-AgentsView-Search-Intent", "semantic")
					resp, err := client.Do(req)
					if err != nil {
						return false
					}
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					return resp.StatusCode == http.StatusOK && strings.Contains(string(body), "combined-session")
				}, 30*time.Second, time.Second)
			}
		})
	}
}
