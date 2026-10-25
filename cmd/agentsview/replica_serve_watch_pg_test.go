//go:build pgtest

package main

import (
	"context"
	"database/sql"
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
			startup, err := prepareReplicaServeWithWatch(pgReplica{}, cfg, "", &replicaServeWatch{push: ReplicaPushConfig{Embed: embed, Debounce: time.Second, Interval: time.Second}})
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
					t.Error("serve did not join watch")
				}
			})
			client := &http.Client{Timeout: 10 * time.Second}
			sessionURL := fmt.Sprintf("http://%s:%d/api/v1/sessions/combined-session/messages", startup.cfg.Host, startup.cfg.Port)
			require.Eventually(t, func() bool {
				resp, err := client.Get(sessionURL)
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				return resp.StatusCode == 200 && strings.Contains(string(body), "initial mounted content")
			}, 30*time.Second, time.Second)
			file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			appended := testjsonl.NewSessionBuilder().AddClaudeAssistant(
				"2026-01-01T00:01:00Z", "appended mounted content",
			).String()
			_, err = file.WriteString(appended)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.Eventually(t, func() bool {
				var count int
				err := admin.QueryRow("SELECT count(*) FROM " + schema + ".messages WHERE content = 'appended mounted content'").Scan(&count)
				return err == nil && count == 1
			}, 30*time.Second, time.Second)
			response, err := client.Get(sessionURL)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Contains(t, string(body), "appended mounted content")
			local, err := sql.Open("sqlite3", "file:"+cfg.DBPath+"?mode=ro")
			require.NoError(t, err)
			defer local.Close()
			var content string
			require.NoError(t, local.QueryRow("SELECT content FROM messages WHERE role = 'assistant'").Scan(&content))
			assert.Equal(t, "appended mounted content", content)
			if embed {
				require.Eventually(t, func() bool {
					var count int
					err := admin.QueryRow("SELECT count(*) FROM " + schema + ".vector_push_state").Scan(&count)
					return err == nil && count > 0
				}, 60*time.Second, time.Second)
				searchURL := "http://" + startup.cfg.Host + ":" + fmt.Sprint(startup.cfg.Port) +
					"/api/v1/search/content?pattern=" + url.QueryEscape("initial mounted content") +
					"&mode=semantic&limit=5&include_one_shot=true"
				var searchStatus int
				var searchBody string
				var searchErr string
				searchReady := assert.Eventually(t, func() bool {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, searchURL, nil)
					if err != nil {
						searchErr = err.Error()
						return false
					}
					req.Header.Set("X-AgentsView-Search-Intent", "semantic")
					resp, err := client.Do(req)
					if err != nil {
						searchErr = err.Error()
						return false
					}
					defer resp.Body.Close()
					body, _ := io.ReadAll(resp.Body)
					searchStatus, searchBody = resp.StatusCode, string(body)
					return searchStatus == http.StatusOK && strings.Contains(searchBody, "combined-session")
				}, 30*time.Second, time.Second)
				require.True(t, searchReady, "status=%d body=%s error=%s", searchStatus, searchBody, searchErr)
			}
		})
	}
}
