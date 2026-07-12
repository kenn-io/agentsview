package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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
		"duckdb quack           Quack remote protocol commands",
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

func TestRootHelpShowsQuackEnvironment(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	assert.NotContains(t, help, "AGENTSVIEW_QUACK_URL")
	assert.NotContains(t, help, "AGENTSVIEW_QUACK_TOKEN")
}

func TestRootHelpDocumentsCopilotExportDir(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err, "Execute")
	assert.Contains(t, help,
		"COPILOT_DIR             Copilot sessions or exported JetBrains Copilot directory")
}

func TestDuckDBPushHelpShowsProjectFlags(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "duckdb", "push", "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
		"--full",
		"--projects",
		"--exclude-projects",
		"--all-projects",
		"--watch",
		"--debounce",
		"--interval",
	} {
		assert.Contains(t, help, want)
	}
}

func TestPGStatusHelpShowsProjectFlags(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "pg", "status", "--help")
	require.NoError(t, err, "Execute")
	for _, want := range []string{
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
		"required",
		"--allow-insecure",
	} {
		assert.Contains(t, help, want)
	}
	assert.NotContains(t, help, "generated if omitted")
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

func TestServeCheckDataVersionRejectsNewerDatabase(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")

	database, err := db.Open(dbPath)
	require.NoError(t, err, "open db")
	require.NoError(t, database.Close(), "close db")

	futureVersion := db.CurrentDataVersion() + 10
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "raw sqlite open")
	_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", futureVersion))
	require.NoError(t, err, "set future user_version")
	require.NoError(t, conn.Close(), "close raw sqlite")

	out, err := executeCommand(newRootCommand(), "serve", "--check-data-version")
	require.Error(t, err, "preflight should reject newer archive")
	assert.Equal(t, dataVersionTooNewExitCode, exitCodeFromError(err))
	assert.Empty(t, out)
	assert.Contains(t, err.Error(), "database data version")
	assert.Contains(t, err.Error(), "is newer than this agentsview binary")
	assert.Contains(t, err.Error(), `Run "agentsview update"`)
}

func TestServeCheckDataVersionDoesNotCreateConfig(t *testing.T) {
	dataDir := testDataDir(t)

	out, err := executeCommand(newRootCommand(), "serve", "--check-data-version")

	require.NoError(t, err, "preflight with no database")
	assert.Empty(t, out)
	_, statErr := os.Stat(filepath.Join(dataDir, "config.toml"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"preflight must not create config.toml")
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

func TestVersionJSONContractDoesNotRequireRuntimeState(t *testing.T) {
	oldVersion, oldCommit, oldBuildDate := version, commit, buildDate
	t.Cleanup(func() {
		version, commit, buildDate = oldVersion, oldCommit, oldBuildDate
	})
	version = "v1.2.3"
	commit = "abc1234"
	buildDate = "2026-07-12T14:30:00Z"

	dataDirFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(dataDirFile, []byte("occupied"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDirFile)

	got, err := executeCommand(newRootCommand(), "version", "--json")
	require.NoError(t, err, "Execute")

	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		Name          string `json:"name"`
		Version       string `json:"version"`
		Commit        string `json:"commit"`
		BuildDate     string `json:"build_date"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &doc))
	assert.Equal(t, 1, doc.SchemaVersion)
	assert.Equal(t, "agentsview", doc.Name)
	assert.Equal(t, "v1.2.3", doc.Version)
	assert.Equal(t, "abc1234", doc.Commit)
	assert.Equal(t, "2026-07-12T14:30:00Z", doc.BuildDate)
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
	for _, want := range []string{
		"remote_hosts",
		"passwordless",
		"transport = \"http\"",
		"daemon_idle_timeout",
		"Top-level daemon_idle_timeout",
		"Tailscale",
	} {
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
