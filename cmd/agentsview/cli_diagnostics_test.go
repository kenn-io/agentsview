package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRecoveryUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		hint string
	}{
		{"unknown flag", []string{"session", "list", "--bogus"}, 64, "agentsview session list --help"},
		{"missing id", []string{"session", "get"}, 64, "agentsview session get --help"},
		{"bad value", []string{"session", "list", "--limit", "abc"}, 65, "agentsview session list --help"},
		{"unknown nested command", []string{"session", "lisst"}, 64, "did you mean list?"},
		{"unknown root command", []string{"sessionn"}, 64, "did you mean session?"},
		{"missing required flag", []string{"import", "fixture.json"}, 64, "agentsview import --help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
			require.Error(t, err)
			assert.Equal(t, tc.code, reportCLIError(err, &stderr))
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), "fatal:")
			assert.Contains(t, stderr.String(), "\nhint:")
			assert.Contains(t, stderr.String(), tc.hint)
		})
	}
}

func TestCLIRecoveryJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := executeCLIWithLegacyFlagCompat([]string{"session", "list", "--bogus", "--json"}, &stdout, &stderr)
	require.Error(t, err)
	reportCLIError(err, &stderr)
	assert.Empty(t, stdout.String())
	var diagnostic struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	}
	require.NoError(t, json.Unmarshal(stderr.Bytes(), &diagnostic))
	assert.Equal(t, "usage", diagnostic.Error)
	assert.Contains(t, diagnostic.Message, "unknown flag")
	assert.Contains(t, diagnostic.Hint, "agentsview session list --help")
}

func TestCLIRecoveryValidationBeforeTransport(t *testing.T) {
	isolateDirectCLISources(t)
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	registerTestRuntime(t, dataDir, ts.URL, false)
	for _, args := range [][]string{{"session", "list"}, {"stats"}} {
		var stdout, stderr bytes.Buffer
		_ = executeCLIWithLegacyFlagCompat(args, &stdout, &stderr)
		assert.NotZero(t, requests.Load(), "fixture runtime must receive a valid %s request", strings.Join(args, " "))
		requests.Store(0)
	}
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"session", "list", "--sort", "bogus"}, "sort"},
		{[]string{"session", "list", "--since", "garbage"}, "since"},
		{[]string{"stats", "--since", "garbage"}, "since"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
			require.Error(t, err)
			assert.Equal(t, 65, reportCLIError(err, &stderr))
			assert.Contains(t, err.Error(), tc.message)
			assert.Contains(t, stderr.String(), "\nhint:")
			assert.Empty(t, stdout.String())
			assert.Zero(t, requests.Load(), "invalid input must be rejected before any transport request")
		})
	}
}

func TestCLIRecoveryFlagConflicts(t *testing.T) {
	isolateDirectCLISources(t)
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"session", "list", "--server", "http://localhost:1", "--pg"}, "--server and --pg are mutually exclusive"},
		{[]string{"session", "list", "--since", "7d", "--active-since", "2026-01-01T00:00:00Z"}, "--since and --active-since are mutually exclusive"},
		{[]string{"session", "messages", "codex:fixture", "--before", "3"}, "--before/--after require --around"},
		{[]string{"session", "search", "pattern", "--regex", "--fts"}, "--regex, --fts, --semantic and --hybrid are mutually exclusive"},
		{[]string{"session", "search", "pattern", "--scope", "top"}, "--scope requires --semantic or --hybrid"},
		{[]string{"session", "watch", "codex:fixture", "--json"}, "streams NDJSON; --format/--json not supported"},
		{[]string{"session", "export", "codex:fixture", "--pg"}, "session export: local-only command; --pg not supported"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
			require.Error(t, err)
			assert.Equal(t, 64, reportCLIError(err, &stderr))
			assert.Contains(t, err.Error(), tc.message)
			assert.Empty(t, stdout.String())
		})
	}
}

func TestCLIRecoveryConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		process bool
		json    bool
	}{
		{name: "session list", args: []string{"session", "list"}},
		{name: "projects JSON", args: []string{"projects", "--json"}, process: true, json: true},
		{name: "token-use", args: []string{"token-use", "codex:fixture"}, process: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := legacyRecoveryEnv(t)
			writeTestConfig(t, dataDir, "invalid = [")
			var stdout, stderr string
			var code int
			if tc.process {
				code, stdout, stderr = legacyRecoveryProcess(t, tc.args...)
			} else {
				var stdoutBuf, stderrBuf bytes.Buffer
				err := executeCLIWithLegacyFlagCompat(tc.args, &stdoutBuf, &stderrBuf)
				require.Error(t, err)
				code, stdout, stderr = reportCLIError(err, &stderrBuf), stdoutBuf.String(), stderrBuf.String()
			}
			assert.Equal(t, 1, code)
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "config.toml")
			assert.Contains(t, stderr, "TOML")
			if tc.json {
				var diagnostic map[string]string
				require.NoError(t, json.Unmarshal([]byte(stderr), &diagnostic), stderr)
				assert.Contains(t, diagnostic["hint"], "TOML")
			} else {
				assert.Contains(t, stderr, "\nhint:")
			}
		})
	}
}

func TestCLIRecoveryInvalidFormatRetainsJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		hint string
	}{
		{"session list", []string{"session", "list", "--format=json", "--format=bogus"}, "agentsview session list --help"},
		{"export sessions", []string{"export", "sessions", "--format=json", "--format=bogus"}, "agentsview export sessions --help"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
			require.Error(t, err)
			assert.Equal(t, 65, reportCLIError(err, &stderr))
			assert.Empty(t, stdout.String())
			var diagnostic struct {
				Error   string `json:"error"`
				Message string `json:"message"`
				Hint    string `json:"hint"`
			}
			require.NoError(t, json.Unmarshal(stderr.Bytes(), &diagnostic))
			assert.Equal(t, "validation", diagnostic.Error)
			assert.Contains(t, diagnostic.Message, "--format")
			assert.Contains(t, diagnostic.Hint, tc.hint)
		})
	}
}
