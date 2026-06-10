package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func executeCommand(root *cobra.Command, args ...string) (string, error) {
	_, output, err := executeCommandC(root, args...)
	return output, err
}

func executeCommandC(root *cobra.Command, args ...string) (*cobra.Command, string, error) {
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	cmd, err := root.ExecuteC()
	return cmd, buf.String(), err
}

func TestRootHelpShowsKeySectionsAndCommands(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"Usage:\n  agentsview [flags]\n  agentsview <command> [flags]",
		"Core Commands:",
		"Data Commands:",
		"Usage Commands:",
		"Other Commands:",
		"serve                  Start server",
		"duckdb status          Show DuckDB sync status",
		"pg push                Push local data to PostgreSQL",
		"usage daily            Daily cost summary",
		"completion             Generate the autocompletion script for the specified shell",
		"Flags:",
		"--version",
	} {
		assert.Contains(t, help, want, "help missing %q", want)
	}
	for _, unwanted := range []string{
		"--host string",
		"--port int",
	} {
		assert.NotContains(t, help, unwanted,
			"root help should not include serve flag %q", unwanted)
	}
}

func TestRootHelpShowsDuckDBEnvironment(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"AGENTSVIEW_DUCKDB_PATH",
		"AGENTSVIEW_DUCKDB_URL",
		"AGENTSVIEW_DUCKDB_TOKEN",
		"AGENTSVIEW_DUCKDB_MACHINE",
	} {
		assert.Contains(t, help, want, "help missing %q", want)
	}
	assert.NotContains(t, help, "env-token")
}

func TestDuckDBPushHelpShowsProjectFlags(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "duckdb", "push", "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"--full",
		"--projects",
		"--exclude-projects",
		"--all-projects",
	} {
		assert.Contains(t, help, want)
	}
}

func TestDuckDBQuackServeHelpShowsSafetyFlags(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "duckdb", "quack", "serve", "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"--bind",
		"--path",
		"--token",
		"--allow-insecure",
	} {
		assert.Contains(t, help, want)
	}
}

func TestOpenAPICommandEmitsSpec(t *testing.T) {
	out, err := executeCommand(newRootCommand(), "openapi")
	require.NoError(t, err, "Execute")

	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &spec))
	assert.Equal(t, "3.1.0", spec.OpenAPI)
	require.Contains(t, spec.Paths, "/api/v1/sessions")
	assert.Contains(t, spec.Paths["/api/v1/sessions"], "get")
	require.Contains(t, spec.Paths, "/api/v1/sessions/{id}/rename")
	assert.Contains(t, spec.Paths["/api/v1/sessions/{id}/rename"], "patch")
}

func TestRootNoArgsShowsHelp(t *testing.T) {
	out, err := executeCommand(newRootCommand())
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"Usage:\n  agentsview [flags]\n  agentsview <command> [flags]",
		"Core Commands:",
		"serve                  Start server",
	} {
		assert.Contains(t, out, want, "output missing %q", want)
	}
}

func TestRootHelpKeepsSummaryClean(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	for _, unwanted := range []string{
		"agentsview serve [flags]",
		"\nCommands:\n",
		"completion bash",
		"completion fish",
		"completion powershell",
		"completion zsh",
	} {
		assert.NotContains(t, help, unwanted,
			"root help should not include %q", unwanted)
	}
}

func TestNormalizeFlagHelpWidth(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{in: 0, want: 80},
		{in: -1, want: 80},
		{in: 79, want: 79},
		{in: 120, want: 120},
		{in: 160, want: 160},
		{in: 220, want: 160},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, normalizeFlagHelpWidth(tt.in),
			"normalizeFlagHelpWidth(%d)", tt.in)
	}
}

func TestFlagHelpWidthFallback(t *testing.T) {
	assert.Equal(t, 80, flagHelpWidth(&bytes.Buffer{}),
		"flagHelpWidth(buffer)")

	f, err := os.CreateTemp(t.TempDir(), "help-width")
	require.NoError(t, err, "CreateTemp")
	defer f.Close()

	assert.Equal(t, 80, flagHelpWidth(f), "flagHelpWidth(file)")
}

func TestRootVersionFlag(t *testing.T) {
	got, err := executeCommand(newRootCommand(), "--version")
	require.NoError(t, err, "Execute")
	assert.Contains(t, got, "agentsview ", "version output = %q", got)
}

func TestNormalizeLegacyLongFlags(t *testing.T) {
	flags := collectLongFlags(newRootCommand())
	got, rewrites := normalizeLegacyLongFlags([]string{
		"-host", "0.0.0.0",
		"-port=9090",
		"sync",
		"-full",
		"--",
		"-port", "1000",
	}, flags)
	want := []string{
		"--host", "0.0.0.0",
		"--port=9090",
		"sync",
		"--full",
		"--",
		"-port", "1000",
	}
	assert.Equal(t, want, got)
	wantRewrites := []string{
		"-host -> --host",
		"-port -> --port",
		"-full -> --full",
	}
	assert.Equal(t, wantRewrites, rewrites)
}

func TestNormalizeLegacyLongFlagsSkipsShortFlagsAndNumbers(t *testing.T) {
	flags := collectLongFlags(newRootCommand())
	got, rewrites := normalizeLegacyLongFlags([]string{
		"-h",
		"-v",
		"-1",
		"-abc",
		"--port", "9090",
	}, flags)
	want := []string{"-h", "-v", "-1", "-abc", "--port", "9090"}
	assert.Equal(t, want, got)
	assert.Empty(t, rewrites)
}

func TestLegacyLongFlagWarning(t *testing.T) {
	got := legacyLongFlagWarning([]string{
		"-host -> --host",
		"-port -> --port",
	})
	want := "warning: deprecated single-dash long flags detected; use GNU-style long flags instead: -host -> --host, -port -> --port\n"
	assert.Equal(t, want, got)
}

func TestExecuteCLIWithLegacyFlagCompatWarnsOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.NoError(t,
		executeCLIWithLegacyFlagCompat([]string{"-version"}, &stdout, &stderr),
		"Execute")
	assert.Contains(t, stdout.String(), "agentsview ",
		"version output = %q", stdout.String())
	want := "warning: deprecated single-dash long flags detected; use GNU-style long flags instead: -version -> --version\n"
	assert.Equal(t, want, stderr.String())
}

func TestRootHelpDocumentsRemoteHosts(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{"remote_hosts", "passwordless"} {
		assert.Contains(t, help, want,
			"root help should document %q", want)
	}
}

func TestSyncHelpMentionsConfiguredHosts(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "sync", "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{"remote_hosts", "--host", "passwordless"} {
		assert.Contains(t, help, want, "sync help missing %q", want)
	}
}
