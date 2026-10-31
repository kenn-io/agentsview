package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Literal public paths protect the registration boundary, including subtrees
// with their own inherited flags. Never execute source ingestion here.
func TestCommandGroupsCanonicalHelp(t *testing.T) {
	for _, path := range []string{
		"source list", "source sync", "source import", "source chrome setup",
		"source raw watch", "source raw backfill", "source raw status",
		"source cursor sync-usage", "project list", "session health", "session prune",
		"report stats", "report activity", "report usage daily", "report usage statusline",
		"replica pg push", "replica pg vectors list", "replica pg service install",
		"replica duckdb status", "replica duckdb quack serve", "replica clickhouse push",
		"index embeddings build", "index classifier rebuild", "archive diff",
	} {
		t.Run(path, func(t *testing.T) {
			root := newRootCommand()
			args := strings.Fields(path)
			cmd, rest, err := root.Find(args)
			require.NoError(t, err)
			require.Empty(t, rest)
			require.Equal(t, "agentsview "+path, cmd.CommandPath())
			out, err := executeCommand(root, append(args, "--help")...)
			require.NoError(t, err)
			assert.Contains(t, out, "agentsview "+path)
			assert.NotContains(t, out, "deprecated")
		})
	}
}

func TestCommandGroupsRootDiscovery(t *testing.T) {
	dir := testDataDir(t)
	help, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err)
	assert.Less(t, len(help), 11500, "root help must remain compact")
	for _, name := range []string{"source", "project", "report", "replica", "index", "session", "archive", "mcp", "openapi"} {
		assert.Contains(t, help, "\n  "+name+" ")
	}
	for _, name := range []string{"agents", "sync", "raw-sync", "projects", "health", "prune", "stats", "usage", "activity", "pg", "duckdb", "clickhouse", "embeddings", "parse-diff", "token-use"} {
		assert.NotContains(t, help, "\n  "+name+" ")
	}
	assert.NotContains(t, help, "\n  session list ", "subcommands belong in domain help")
	assert.Contains(t, help, "agentsview <command> --help")

	for _, tt := range []struct{ prefix, want string }{
		{"source", "sync\t"}, {"report", "activity\t"},
		{"replica", "pg\t"}, {"index", "embeddings\t"},
		{"project", "list\t"}, {"session", "prune\t"},
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
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		out, err := executeCommand(newRootCommand(), "completion", shell)
		require.NoError(t, err)
		assert.NotEmpty(t, out)
	}
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
	assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
}

func TestCommandGroupsAdvertisedExamplesParse(t *testing.T) {
	for _, path := range []string{
		"source", "project", "report", "replica", "index", "source cursor",
		"source list", "source sync", "source import", "source chrome", "source raw",
		"project list", "session health", "session prune", "report stats", "report activity",
		"report usage", "replica pg", "replica duckdb", "replica clickhouse",
		"index embeddings", "index classifier", "archive diff",
	} {
		cmd, _, err := newRootCommand().Find(strings.Fields(path))
		require.NoError(t, err)
		require.NotEmpty(t, cmd.Example, path)
		for _, line := range strings.Split(cmd.Example, "\n") {
			args, err := shlex.Split(line)
			require.NoError(t, err, line)
			require.Greater(t, len(args), 1, line)
			require.Equal(t, "agentsview", args[0])
			root := newRootCommand()
			leaf, rest, err := root.Find(args[1:])
			require.NoError(t, err, line)
			leaf.InitDefaultHelpFlag()
			require.NoError(t, leaf.ParseFlags(rest), line)
			require.NoError(t, leaf.ValidateArgs(leaf.Flags().Args()), line)
			require.NoError(t, leaf.ValidateRequiredFlags(), line)
		}
	}
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

func FuzzCommandGroupsFlagCompatibility(f *testing.F) {
	for _, value := range []string{"", "source", "--pg", "-1", "a,b", `"invalid`, "a\nb", "会話"} {
		f.Add(uint8(0), value, true)
		f.Add(uint8(1), value, false)
		f.Add(uint8(2), value, true)
		f.Add(uint8(3), value, false)
	}
	f.Fuzz(func(t *testing.T, variant uint8, value string, joined bool) {
		cases := []struct{ canonical, legacy, flag string }{
			{"source sync", "sync", "host"},
			{"source import", "import", "replace"},
			{"replica pg push", "pg push", "projects"},
			{"archive diff", "parse-diff", "agent"},
		}
		tt := cases[int(variant)%len(cases)]
		root := newRootCommand()
		canonical, _, err := root.Find(strings.Fields(tt.canonical))
		require.NoError(t, err)
		legacy, _, err := root.Find(strings.Fields(tt.legacy))
		require.NoError(t, err)
		args := []string{"--" + tt.flag, value, "--" + tt.flag, "source"}
		if joined {
			args = []string{"--" + tt.flag + "=" + value, "--" + tt.flag + "=source"}
		}
		args = append(args, "--", value, "report", "--json")
		got, old := canonical.ParseFlags(args), legacy.ParseFlags(args)
		if old != nil {
			require.EqualError(t, got, old.Error())
			return
		}
		require.NoError(t, got)
		assert.Equal(t, legacy.Flag(tt.flag).Value.String(), canonical.Flag(tt.flag).Value.String())
		assert.True(t, canonical.Flag(tt.flag).Changed)
		assert.Equal(t, []string{value, "report", "--json"}, canonical.Flags().Args())
	})
}

func TestCommandGroupsHealthUsesSessionTransport(t *testing.T) {
	dir := testDataDir(t)
	requests := 0
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sessions", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("limit"))
		requests++
		writeJSONResponse(w, `{"sessions":[],"total":0}`)
	})
	out := captureStdout(t, func() {
		_, err := executeCommand(newRootCommand(), "session", "--server", ts.URL, "--json", "health", "--limit", "7")
		require.NoError(t, err)
	})
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
	assert.Equal(t, "", legacy.Flag("host").Value.String())
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
