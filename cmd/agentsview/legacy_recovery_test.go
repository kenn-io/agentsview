package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Isolate direct-exit handlers so the parent can observe whether they pass
// through the shared renderer without terminating the test process.
func TestLegacyRecoveryProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_TEST_LEGACY_RECOVERY") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			args := os.Args[i+1:]
			os.Args = append([]string{os.Args[0]}, args...)
			err := executeCLIWithLegacyFlagCompat(args, os.Stdout, os.Stderr)
			if err != nil {
				os.Exit(reportCLIError(err, os.Stderr))
			}
			os.Exit(0)
		}
	}
	require.FailNow(t, "missing CLI arguments")
}

func legacyRecoveryProcess(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], append([]string{
		"-test.run=^TestLegacyRecoveryProcess$", "--",
	}, args...)...)
	cmd.Env = append(os.Environ(), "AGENTSVIEW_TEST_LEGACY_RECOVERY=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	return exitErr.ExitCode(), stdout.String(), stderr.String()
}

func legacyRecoveryEnv(t *testing.T) string {
	t.Helper()
	clearConfiguredAgentEnvVars(t)
	isolateDirectCLISources(t)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	return newAgentDataDir(t)
}

func TestLegacyUsageMissingSessionRecovery(t *testing.T) {
	legacyRecoveryEnv(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	for _, format := range []string{"human", "json"} {
		t.Run(format, func(t *testing.T) {
			code, stdout, stderr := legacyRecoveryProcess(t,
				"session", "usage", "codex:missing", "--own-only", "--no-sync",
				"--server", ts.URL, "--format", format,
			)
			assert.Equal(t, 2, code)
			assert.Empty(t, stdout)
			if format == "json" {
				var diagnostic map[string]string
				require.NoError(t, json.Unmarshal([]byte(stderr), &diagnostic), stderr)
				assert.Contains(t, diagnostic["message"], "session not found: codex:missing")
				assert.Contains(t, diagnostic["hint"], "session list")
			} else {
				assert.Contains(t, stderr, "fatal: session not found: codex:missing")
				assert.Contains(t, stderr, "hint:")
				assert.Contains(t, stderr, "session list")
			}
		})
	}
}

func TestLegacyUsageNoDataKeepsSilentExit(t *testing.T) {
	legacyRecoveryEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/codex:empty":
			writeJSONResponse(w, `{"id":"codex:empty","agent":"codex"}`)
		case "/api/v1/sessions/codex:empty/usage":
			writeJSONResponse(w, `{"session_id":"codex:empty","agent":"codex","has_token_data":false,"has_cost":false,"models":[],"unpriced_models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	code, stdout, stderr := legacyRecoveryProcess(t,
		"session", "usage", "codex:empty", "--own-only", "--no-sync",
		"--server", ts.URL, "--json",
	)
	assert.Equal(t, 3, code)
	assert.Empty(t, stderr)
	var document map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &document), stdout)
	assert.Equal(t, "codex:empty", document["session_id"])
	assert.Equal(t, false, document["has_token_data"])
	assert.NotContains(t, document, "error")
}

func TestLegacyUsageBatchKeepsPerIDExitCodes(t *testing.T) {
	legacyRecoveryEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/codex:empty", "/api/v1/sessions/codex:failed":
			writeJSONResponse(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/")+`","agent":"codex"}`)
		case "/api/v1/sessions/codex:empty/usage":
			writeJSONResponse(w, `{"session_id":"codex:empty","agent":"codex","has_token_data":false,"has_cost":false,"models":[],"unpriced_models":[]}`)
		case "/api/v1/sessions/codex:failed/usage":
			http.Error(w, "usage unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	code, stdout, stderr := legacyRecoveryProcess(t,
		"session", "usage-batch", "codex:missing", "codex:empty", "codex:failed",
		"--own-only", "--no-sync", "--server", ts.URL, "--json",
	)
	assert.Zero(t, code)
	assert.Empty(t, stderr)
	var document sessionUsageBatchOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &document), stdout)
	require.Len(t, document.Results, 3)
	assert.Equal(t, "codex:missing", document.Results[0].RequestedID)
	assert.Equal(t, 2, document.Results[0].ExitCode)
	assert.Equal(t, "session not found: codex:missing", document.Results[0].Error)
	assert.Equal(t, "codex:empty", document.Results[1].RequestedID)
	assert.Equal(t, 3, document.Results[1].ExitCode)
	require.NotNil(t, document.Results[1].Usage)
	assert.Equal(t, "codex:failed", document.Results[2].RequestedID)
	assert.Equal(t, 1, document.Results[2].ExitCode)
	assert.Contains(t, document.Results[2].Error, "usage unavailable")
}

func TestLegacyHealthMissingSessionRecovery(t *testing.T) {
	dataDir := legacyRecoveryEnv(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/session-ids/resolve" {
			writeJSONResponse(w, `{"ids":[]}`)
			return
		}
		http.NotFound(w, r)
	})
	registerTestRuntime(t, dataDir, ts.URL, false)
	code, stdout, stderr := legacyRecoveryProcess(t,
		"health", "codex:missing", "--json",
	)
	assert.Equal(t, 1, code)
	assert.Empty(t, stdout)
	var diagnostic map[string]string
	require.NoError(t, json.Unmarshal([]byte(stderr), &diagnostic), stderr)
	assert.Contains(t, diagnostic["message"], "session not found: codex:missing")
	assert.Contains(t, diagnostic["hint"], "session list")
}

func TestLegacyCommandsUseCommandOutput(t *testing.T) {
	dataDir := legacyRecoveryEnv(t)
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/usage/summary/stream":
			writeUsageStreamResponse(t, w, r, `{"daily":[],"totals":{}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/usage/"):
			writeJSONResponse(w, `{"daily":[],"totals":{}}`)
		case r.URL.Path == "/api/v1/sync":
			writeJSONResponse(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/activity/"):
			writeJSONResponse(w, `{}`)
		case r.URL.Path == "/api/v1/sessions":
			writeJSONResponse(w, `{"sessions":[],"total":0}`)
		default:
			http.NotFound(w, r)
		}
	})
	registerTestRuntime(t, dataDir, ts.URL, false)
	for _, args := range [][]string{
		{"usage", "daily", "--timezone", "UTC"},
		{"usage", "statusline"},
		{"activity", "report", "--timezone", "UTC"},
		{"health"},
	} {
		t.Run(strings.Join(args[:1], " ")+" "+strings.Join(args[1:], " "), func(t *testing.T) {
			root := newRootCommand()
			root.SetArgs(append(args, "--json"))
			var output bytes.Buffer
			root.SetOut(&output)
			stdout := captureStdout(t, func() { require.NoError(t, root.Execute()) })
			assert.Empty(t, stdout, "commands must honor their configured output writer")
			var document any
			require.NoError(t, json.Unmarshal(output.Bytes(), &document), output.String())
		})
	}
}

func TestLegacySelectionValidationBeforeTransport(t *testing.T) {
	dataDir := legacyRecoveryEnv(t)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	registerTestRuntime(t, dataDir, ts.URL, false)
	for _, tc := range []struct {
		name    string
		args    []string
		message string
		hint    string
	}{
		{"usage timezone", []string{"usage", "daily", "--timezone", "Invalid/Timezone"}, "invalid --timezone", "--timezone"},
		{"usage window", []string{"usage", "daily", "--since", "not-a-window"}, "invalid --since", "--since"},
		{"activity date", []string{"activity", "report", "--date", "2026-99-99"}, "date", "--date"},
		{"activity bucket", []string{"activity", "report", "--bucket", "2m"}, "bucket", "--bucket"},
		{"activity session range", []string{"activity", "report", "--sessions-bucket-start", "0"}, "requires both start and end", "--sessions-bucket"},
		{"activity cursor session range", []string{"activity", "report", "--sessions-cursor", "fixture-cursor", "--sessions-bucket-start", "0"}, "requires both start and end", "--sessions-bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := legacyRecoveryProcess(t,
				append(tc.args, "--json")...)
			assert.Equal(t, 65, code)
			assert.Empty(t, stdout)
			var diagnostic map[string]string
			require.NoError(t, json.Unmarshal([]byte(stderr), &diagnostic), stderr)
			assert.Equal(t, "validation", diagnostic["error"])
			assert.Contains(t, strings.ToLower(diagnostic["message"]), tc.message)
			assert.Contains(t, diagnostic["hint"], tc.hint)
		})
	}
	assert.Zero(t, requests.Load(), "invalid selections must not open a transport")
}
