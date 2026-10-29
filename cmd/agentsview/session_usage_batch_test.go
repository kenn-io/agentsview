package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSessionUsageBatchPGOrderedResults(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	store := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	seedSubagentOnlyUsage(t, store, "claude:parent", "agent-child", 42)
	seedUsageSession(t, store, "codex:later", "usage-project", "codex", 17)
	opens, closes := 0, 0
	original := openPGReadStore
	openPGReadStore = func(config.Config, config.PGConfig) (db.Store, func(), error) {
		opens++
		return store, func() { closes++ }, nil
	}
	t.Cleanup(func() { openPGReadStore = original })
	cmd := sessionUsageCommand(t, "session", "usage-batch", "claude:parent", "--pg", "--no-sync")
	var out *sessionUsageBatchOutput
	stderr := captureStderr(t, func() {
		var err error
		out, err = sessionUsageBatchDataForCommand(cmd, []string{
			"claude:parent", "codex:missing", "codex:later", "claude:parent",
		})
		require.NoError(t, err)
	})
	assert.Empty(t, stderr)
	require.Len(t, out.Results, 4)
	assert.Equal(t, 1, opens)
	assert.Equal(t, 1, closes)
	assert.Equal(t, "claude:parent", out.Results[0].RequestedID)
	assert.Equal(t, 0, out.Results[0].ExitCode)
	assert.Equal(t, 2, out.Results[1].ExitCode)
	assert.Nil(t, out.Results[1].Usage)
	assert.Equal(t, "codex:later", out.Results[2].RequestedID)
	assert.Equal(t, 0, out.Results[2].ExitCode)
	require.NotNil(t, out.Results[2].Usage)
	assert.Equal(t, 17, out.Results[2].Usage.TotalOutputTokens)
	assert.Equal(t, out.Results[0], out.Results[3])
	var human strings.Builder
	require.NoError(t, renderSessionUsageBatch(&human, out, "human"))
	assert.Contains(t, human.String(), "claude:parent (exit 0)")
}

func TestSessionUsageBatchRemoteReusesSetup(t *testing.T) {
	for _, autostart := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "autostart"}[autostart], func(t *testing.T) {
			newAgentDataDir(t)
			ts, reqs := newRemoteUsageServer(t, remoteUsageSpec{canonicalID: "codex:session-a"})
			args := []string{"session", "usage-batch", "codex:session-a", "--no-sync", "--json"}
			starts := 0
			if autostart {
				stubStartBackgroundServeForTransport(t, func(_ context.Context, _ *config.Config, _ time.Duration, _ bool) (*DaemonRuntime, error) {
					starts++
					return daemonRuntimeFromTestURL(t, ts.URL), nil
				})
			} else {
				args = append(args, "--server", ts.URL)
			}
			root := newRootCommand()
			root.SetArgs(append(args, "codex:session-a"))
			var output strings.Builder
			root.SetOut(&output)
			require.NoError(t, root.Execute())
			raw := decodeCLIJSON[map[string][]map[string]any](t, output.String())
			require.Len(t, raw["results"], 2)
			assert.Equal(t, "codex:session-a", raw["results"][0]["requested_id"])
			assert.InDelta(t, 0, raw["results"][0]["exit_code"], 0)
			assert.Contains(t, raw["results"][0], "usage")
			assert.NotContains(t, raw["results"][0], "error")
			versions := 0
			for _, path := range reqs.RequestPath {
				if path == "/api/v1/version" {
					versions++
				}
			}
			if autostart {
				assert.Equal(t, 1, starts)
			} else {
				assert.Equal(t, 1, versions)
			}
		})
	}
}

func TestSessionUsageBatchErrors(t *testing.T) {
	t.Run("per-ID failure", func(t *testing.T) {
		newAgentDataDir(t)
		ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/usage") {
				http.Error(w, "usage unavailable", http.StatusInternalServerError)
			} else {
				writeJSONResponse(w, `{"id":"codex:a","agent":"codex"}`)
			}
		})
		cmd := sessionUsageCommand(t, "session", "usage-batch", "codex:a", "--server", ts.URL, "--own-only")
		out, err := sessionUsageBatchDataForCommand(cmd, []string{"codex:a"})
		require.NoError(t, err)
		require.Len(t, out.Results, 1)
		assert.Equal(t, 1, out.Results[0].ExitCode)
		assert.Contains(t, out.Results[0].Error, "usage unavailable")
	})
	t.Run("setup failure", func(t *testing.T) {
		newAgentDataDir(t)
		cmd := sessionUsageCommand(t, "session", "usage-batch", "codex:a", "--server", "http://example.test", "--pg")
		_, err := sessionUsageBatchDataForCommand(cmd, []string{"codex:a"})
		require.ErrorContains(t, err, "mutually exclusive")
	})
	t.Run("writer failure", func(t *testing.T) {
		out := &sessionUsageBatchOutput{Results: []sessionUsageBatchResult{{
			RequestedID: "codex:a", ExitCode: 1, Error: "usage unavailable",
		}}}
		for _, format := range []string{"json", "human"} {
			t.Run(format, func(t *testing.T) {
				err := renderSessionUsageBatch(&failingRawSyncWriter{err: errors.New("output unavailable")}, out, format)
				require.ErrorContains(t, err, "output unavailable")
			})
		}
	})
}

func TestSessionUsageBatchMissingDiagnostic(t *testing.T) {
	newAgentDataDir(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			http.NotFound(w, r)
		} else {
			writeJSONResponse(w, `{"id":"codex:missing","agent":"codex"}`)
		}
	})
	for _, format := range []string{"human", "json"} {
		t.Run(format, func(t *testing.T) {
			root := newRootCommand()
			root.SetArgs([]string{"session", "usage-batch", "codex:missing", "--server", ts.URL, "--own-only", "--format", format})
			var output strings.Builder
			root.SetOut(&output)
			stderr := captureStderr(t, func() { require.NoError(t, root.Execute()) })
			assert.Empty(t, stderr)
			assert.Equal(t, 1, strings.Count(output.String(), "session not found: codex:missing"))
		})
	}
}
