package main

import (
	"context"
	"encoding/json/jsontext"
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
	"go.kenn.io/agentsview/internal/money"
)

func TestSessionUsageBatchPGOrderedResults(t *testing.T) {
	for _, ownOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "descendants", true: "own-only"}[ownOnly], func(t *testing.T) {
			dataDir := newAgentDataDir(t)
			t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
			store := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
			seedSubagentOnlyUsage(t, store, "claude:parent", "agent-child", 42)
			seedUsageSession(t, store, "codex:priced", "project-a", "codex", 12)
			require.NoError(t, store.UpsertModelPricing([]db.ModelPricing{{
				ModelPattern: "model-a", OutputPerMTok: money.MustParseDollars("10"),
			}}))
			require.NoError(t, store.InsertMessages(t.Context(), []db.Message{{
				SessionID: "codex:priced", Role: "assistant", Model: "model-a",
				TokenUsage: jsontext.Value(`{"output_tokens":12}`), OutputTokens: 12, HasOutputTokens: true,
			}}))
			opens, closes := 0, 0
			original := openPGReadStore
			openPGReadStore = func(config.Config, config.PGConfig) (db.Store, func(), error) {
				opens++
				return store, func() { closes++ }, nil
			}
			t.Cleanup(func() { openPGReadStore = original })
			args := []string{"session", "usage-batch", "claude:parent", "--pg", "--no-sync"}
			if ownOnly {
				args = append(args, "--own-only")
			}
			cmd := sessionUsageCommand(t, args...)
			out, err := sessionUsageBatchDataForCommand(cmd, []string{"claude:parent", "codex:missing", "codex:priced", "claude:parent"})
			require.NoError(t, err)
			require.Len(t, out.Results, 4)
			assert.Equal(t, 1, opens)
			assert.Equal(t, 1, closes)
			assert.Equal(t, "claude:parent", out.Results[0].RequestedID)
			assert.Equal(t, 2, out.Results[1].ExitCode)
			assert.Equal(t, "session not found: codex:missing", out.Results[1].Error)
			assert.Nil(t, out.Results[1].Usage)
			assert.Equal(t, 0, out.Results[2].ExitCode)
			assert.Equal(t, 12, out.Results[2].Usage.TotalOutputTokens)
			assert.True(t, out.Results[2].Usage.HasCost)
			assert.Equal(t, money.MustParseDollars("0.00012"), out.Results[2].Usage.Cost)
			assert.Equal(t, out.Results[0], out.Results[3])
			if ownOnly {
				assert.Equal(t, 3, out.Results[0].ExitCode)
				assert.Zero(t, out.Results[0].Usage.TotalOutputTokens)
			} else {
				assert.Equal(t, 0, out.Results[0].ExitCode)
				assert.Equal(t, 42, out.Results[0].Usage.TotalOutputTokens)
				assert.Equal(t, 1, out.Results[0].Usage.SubagentCount)
			}
		})
	}
}

func TestSessionUsageBatchLocalArchive(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	store := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	seedSubagentOnlyUsage(t, store, "claude:parent", "agent-child", 42)
	require.NoError(t, store.Close())
	cmd := sessionUsageCommand(t, "session", "usage-batch", "claude:parent")
	out, err := sessionUsageBatchDataForCommand(cmd, []string{"claude:parent", "claude:parent"})
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	assert.Equal(t, 0, out.Results[0].ExitCode)
	assert.Equal(t, 42, out.Results[0].Usage.TotalOutputTokens)
	assert.Equal(t, 1, out.Results[0].Usage.SubagentCount)
	assert.Equal(t, out.Results[0], out.Results[1])
	var human strings.Builder
	require.NoError(t, renderSessionUsageBatch(&human, out, "human"))
	assert.Contains(t, human.String(), "claude:parent (exit 0)")
	assert.Contains(t, human.String(), "Subagents:")
}

func TestSessionUsageBatchRemoteReusesSetup(t *testing.T) {
	for _, discovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "discovered"}[discovered], func(t *testing.T) {
			newAgentDataDir(t)
			ts, reqs := newRemoteUsageServer(t, remoteUsageSpec{canonicalID: "codex:session-a"})
			args := []string{"session", "usage-batch", "codex:session-a", "--no-sync", "--json"}
			starts := 0
			if discovered {
				stubStartBackgroundServeForTransport(t, func(_ context.Context, cfg *config.Config, _ time.Duration, _ bool) (*DaemonRuntime, error) {
					starts++
					assert.True(t, cfg.NoSync)
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
			assert.Equal(t, float64(0), raw["results"][0]["exit_code"])
			assert.Contains(t, raw["results"][0], "usage")
			assert.NotContains(t, raw["results"][0], "error")
			got := decodeCLIJSON[sessionUsageBatchOutput](t, output.String())
			require.Len(t, got.Results, 2)
			assert.Equal(t, "codex:session-a", got.Results[0].RequestedID)
			assert.Equal(t, 42, got.Results[0].Usage.TotalOutputTokens)
			assert.Equal(t, got.Results[0], got.Results[1])
			assert.Empty(t, reqs.SyncInput.ID)
			assert.Equal(t, "breakdown=true&subagents=true", reqs.UsageQuery)
			versions := 0
			for _, path := range reqs.RequestPath {
				if path == "/api/v1/version" {
					versions++
				}
			}
			if discovered {
				assert.Equal(t, 1, starts)
			} else {
				assert.Equal(t, 1, versions)
			}
		})
	}
}

func TestSessionUsageBatchErrors(t *testing.T) {
	newAgentDataDir(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			http.Error(w, "usage unavailable", http.StatusInternalServerError)
		} else {
			writeJSONResponse(w, `{"id":"codex:a","agent":"codex"}`)
		}
	})
	cmd := sessionUsageCommand(t, "session", "usage-batch", "codex:a", "--server", ts.URL, "--own-only")
	out, err := sessionUsageBatchDataForCommand(cmd, []string{"codex:a", "codex:a"})
	require.NoError(t, err)
	require.Len(t, out.Results, 2)
	for _, result := range out.Results {
		assert.Equal(t, 1, result.ExitCode)
		assert.Contains(t, result.Error, "usage unavailable")
	}
	require.NoError(t, cmd.Flags().Set("pg", "true"))
	_, err = sessionUsageBatchDataForCommand(cmd, []string{"codex:a"})
	require.ErrorContains(t, err, "mutually exclusive")
	for _, format := range []string{"json", "human"} {
		err := renderSessionUsageBatch(&failingRawSyncWriter{err: errors.New("output unavailable")}, out, format)
		require.ErrorContains(t, err, "output unavailable")
	}
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
	cmd := sessionUsageCommand(t, "session", "usage", "codex:missing", "--server", ts.URL, "--own-only")
	stderr := captureStderr(t, func() {
		_, code, err := sessionUsageDataForCommand(cmd, "codex:missing")
		require.NoError(t, err)
		assert.Equal(t, tokenUseExitNotFound, code)
	})
	assert.Equal(t, "session not found: codex:missing\n", stderr)
}
