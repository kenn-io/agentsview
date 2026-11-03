package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var agentHelpCoreCommands = []string{
	"serve", "serve status", "serve stop", "daemon", "daemon start", "daemon stop", "daemon status", "daemon restart", "openapi", "update", "mcp",
	"source", "source list", "source sync", "source import", "source chrome", "source raw", "source cursor", "source cursor sync-usage",
	"project", "project list", "report", "report stats", "report activity", "report usage", "report usage daily", "report usage statusline",
	"replica", "replica pg", "replica duckdb", "replica clickhouse", "index", "index embeddings", "index classifier", "archive diff",
	"session", "session list", "session get", "session messages", "session tool-calls", "session search", "session usage", "session usage-batch", "session watch", "session export", "session sync", "session label", "session parent", "session health", "session prune",
	"recall", "insight", "friction", "memory", "skills", "secrets", "export", "doctor",
}

// Literal public paths protect the registration boundary, including subtrees
// with their own inherited flags. Never execute source ingestion here.
func TestCommandGroupsCanonicalHelp(t *testing.T) {
	check := func(t *testing.T, path string) {
		t.Helper()
		root := newRootCommand()
		args := strings.Fields(path)
		cmd, rest, err := root.Find(args)
		require.NoError(t, err)
		require.Empty(t, rest)
		require.Equal(t, "agentsview "+path, cmd.CommandPath())
	}
	for _, path := range agentHelpCoreCommands {
		t.Run(path, func(t *testing.T) { check(t, path) })
	}
	for _, path := range []string{
		"source chrome setup", "source raw watch", "source raw backfill", "source raw status",
		"replica pg push", "replica pg vectors list", "replica pg service install",
		"replica duckdb status", "replica duckdb quack serve", "replica clickhouse push",
		"index embeddings build", "index classifier rebuild",
	} {
		t.Run(path, func(t *testing.T) { check(t, path) })
	}
}

func TestCommandGroupsRootDiscovery(t *testing.T) {
	dir := testDataDir(t)
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(help, "agentsview "), "root heading should retain the CLI name")
	for _, want := range []string{
		"Usage:\n  agentsview [flags]\n  agentsview <command> [flags]",
		"Agent quick start:",
		"Session IDs:",
		"Where data comes from:",
		"Output:",
		"Exit codes:",
		"1. Discover supported agents and their default source locations:",
		"Flags:",
		"--version",
		"[agents.claude]",
		"dirs = [\"/path/one\", \"/path/two\"]",
		"[agents.codex]",
		"dirs = [\"/codex/a\", \"/codex/b\"]",
		"agentsview source list --json",
	} {
		assert.Contains(t, help, want, "help missing %q", want)
	}
	for _, tc := range []struct {
		heading  string
		commands []string
	}{
		{"Sessions, projects, and sources:", []string{"session", "project", "source"}},
		{"Servers, mirrors, and indexes:", []string{"serve", "daemon", "replica", "index"}},
		{"Reports, usage, and archive:", []string{"report", "archive", "db", "export"}},
		{"Tools and integrations:", []string{"openapi", "mcp", "version"}},
	} {
		_, section, found := strings.Cut(help, tc.heading+"\n")
		require.True(t, found, "missing group %q", tc.heading)
		section, _, _ = strings.Cut(section, "\n\n")
		for _, command := range tc.commands {
			assert.Contains(t, section, "  "+command+" ", "%s should list %s", tc.heading, command)
		}
	}
	assert.Less(t, len(help), 16*1024, "root help must stay under the 16 KiB design budget")
	for _, name := range []string{"agents", "sync", "raw-sync", "projects", "health", "prune", "stats", "usage", "activity", "pg", "duckdb", "clickhouse", "embeddings", "parse-diff", "token-use"} {
		assert.NotContains(t, help, "\n  "+name+" ")
	}
	assert.NotContains(t, help, "\n  session list ", "subcommands belong in domain help")
	assert.Contains(t, help, "agentsview <command> --help")
	for _, unwanted := range []string{
		"--host string",
		"--port int",
		"agentsview serve [flags]",
		"\nCommands:\n",
	} {
		assert.NotContains(t, help, unwanted, "root help should not include %q", unwanted)
	}

	for _, tt := range []struct{ prefix, want string }{
		{"source", "sync\t"},
		{"report", "activity\t"},
		{"replica", "pg\t"},
		{"index", "embeddings\t"},
		{"project", "list\t"},
		{"session", "prune\t"},
	} {
		out, err := executeCommand(newRootCommand(), "__complete", tt.prefix, "")
		require.NoError(t, err)
		assert.Contains(t, out, tt.want)
	}
	out, err := executeCommand(newRootCommand(), "__complete", "")
	require.NoError(t, err)
	assert.Contains(t, out, "source\t")
	assert.NotContains(t, out, "raw-sync\t")
	assert.NotContains(t, out, "token-use\t")
	t.Run("no arguments", func(t *testing.T) {
		out, err := executeCommand(newRootCommand())
		require.NoError(t, err, "Execute")
		assert.Equal(t, help, out)
	})
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
	assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
}

func TestCommandGroupsLegacyCatalogKeepsJSONOnStdout(t *testing.T) {
	t.Setenv("AGENTSVIEW_MODE", "")
	var canonical, canonicalErr, legacy, legacyErr bytes.Buffer
	require.NoError(t, executeCLIWithLegacyFlagCompat([]string{"source", "list", "codex", "--json"}, &canonical, &canonicalErr))
	require.NoError(t, executeCLIWithLegacyFlagCompat([]string{"agents", "codex", "--json"}, &legacy, &legacyErr))
	assert.JSONEq(t, canonical.String(), legacy.String())
	assert.Empty(t, canonicalErr.String())
	assert.Equal(t, "warning: 'agents' is deprecated; use 'agentsview source list' instead\n", legacyErr.String())
}

func TestCommandGroupsLegacyNestedWarning(t *testing.T) {
	t.Setenv("AGENTSVIEW_MODE", "")
	testDataDir(t)
	var stdout, stderr bytes.Buffer
	require.NoError(t, executeCLIWithLegacyFlagCompat([]string{"raw-sync", "status"}, &stdout, &stderr))
	assert.Equal(t, "warning: 'raw-sync status' is deprecated; use 'agentsview source raw status' instead\n", stderr.String())
	assert.NotContains(t, stdout.String(), "deprecated")
}

func TestCommandGroupsPruneRejectsUnsupportedInheritedFlags(t *testing.T) {
	for _, flag := range []string{"--server=http://example.invalid", "--server=", "--server-token-file=missing", "--server-token-file=", "--pg", "--pg=false", "--json", "--json=false", "--format=human"} {
		t.Run(flag, func(t *testing.T) {
			dir := testDataDir(t)
			for _, args := range [][]string{
				{"session", "prune", "--yes", flag},
				{"session", flag, "prune", "--yes"},
			} {
				out, err := executeCommand(newRootCommand(), args...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "session prune only supports the local archive")
				assert.Empty(t, out)
			}
			assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
			assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
		})
	}
}

func TestCommandGroupsHealthUsesSessionTransport(t *testing.T) {
	dir := testDataDir(t)
	requests := 0
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/sessions", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("limit"))
		requests++
		writeJSONResponse(w, `{"sessions":[],"total":0}`)
	})
	out, err := executeCommand(newRootCommand(), "session", "--server", ts.URL, "--json", "health", "--limit", "7")
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, out)
	assert.Equal(t, 1, requests)
	assertNoLocalSessionsDB(t, dir)
}

func TestCommandGroupsFlagStateIsIndependent(t *testing.T) {
	root := newRootCommand()
	canonical, rest, err := root.Find([]string{"source", "sync"})
	require.NoError(t, err)
	require.Empty(t, rest)
	require.NoError(t, canonical.ParseFlags([]string{"--host", "source sync", "--full"}))
	assert.Equal(t, "source sync", canonical.Flag("host").Value.String())
	assert.Equal(t, "true", canonical.Flag("full").Value.String())
	legacy, _, err := root.Find([]string{"sync"})
	require.NoError(t, err)
	assert.Empty(t, legacy.Flag("host").Value.String())
	assert.Equal(t, "false", legacy.Flag("full").Value.String())
	assert.False(t, legacy.Flag("host").Changed)
}

func TestCommandGroupsKeepValidationBeforeStorage(t *testing.T) {
	dir := testDataDir(t)
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"source", "import", "missing.json"}, `required flag(s) "type" not set`},
		{[]string{"replica", "pg", "push", "--embed", "--all"}, "--embed cannot be combined with --all"},
		{[]string{"archive", "diff", "--limit=-1"}, "--limit must be >= 0"},
	} {
		_, err := executeCommand(newRootCommand(), tt.args...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), tt.want)
	}
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
	assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
}

func TestCommandGroupsKeepHiddenProtocolPaths(t *testing.T) {
	root := newRootCommand()
	for _, path := range []string{"remote-sync probe", "remote-sync serve", "chrome-host", "sync-worker"} {
		cmd, rest, err := root.Find(strings.Fields(path))
		require.NoError(t, err)
		require.Empty(t, rest)
		assert.Equal(t, "agentsview "+path, cmd.CommandPath())
	}
	out, err := executeCommand(newRootCommand(), "remote-sync", "probe")
	require.NoError(t, err)
	assert.NotContains(t, out, "deprecated")
	assert.NotContains(t, out, "warning")
}

// Catalog reads need no configuration or source files. Arbitrary positional
// values must be forwarded literally, including command names and flag-looking
// values after --; canonical and legacy invocations return identical documents
// or identical validation errors.
func FuzzCommandGroupsCatalogCompatibility(f *testing.F) {
	for _, value := range []string{"codex", "claude", "sync", "--json", "source", "", "x y", "--", "a\nb"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		var canonical, canonicalErr, legacy, legacyErr bytes.Buffer
		canonicalRoot, legacyRoot := newRootCommand(), newRootCommand()
		canonicalRoot.SetOut(&canonical)
		canonicalRoot.SetErr(&canonicalErr)
		canonicalRoot.SetArgs([]string{"source", "list", "--json", "--", value})
		legacyRoot.SetOut(&legacy)
		legacyRoot.SetErr(&legacyErr)
		legacyRoot.SetArgs([]string{"agents", "--json", "--", value})
		got, old := canonicalRoot.Execute(), legacyRoot.Execute()
		if old != nil {
			require.EqualError(t, got, old.Error())
		} else {
			require.NoError(t, got)
		}
		assert.Equal(t, legacy.String(), canonical.String())
		assert.NotContains(t, canonical.String(), "deprecated")
	})
}

// Docker deployments and installed services keep their argv across upgrades,
// so the binary must launch canonical commands that will outlive the legacy
// spellings and do not print deprecation warnings into service logs.
func TestCommandGroupsSelfLaunchedCommandsAreCanonical(t *testing.T) {
	isolateDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_MODE", "pg-serve")
	pgServe, err := deploymentArgs(nil)
	require.NoError(t, err)
	for _, args := range [][]string{pgServe, pgServiceKind.Args, clickHouseServiceKind.Args} {
		cmd, rest, err := newRootCommand().Find(args)
		require.NoError(t, err)
		require.NoError(t, cmd.ParseFlags(rest))
		for c := cmd; c != nil; c = c.Parent() {
			assert.False(t, c.Hidden, "%v resolves through hidden legacy command %q", args, c.Name())
		}
	}
}
