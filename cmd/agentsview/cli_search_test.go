package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type cliSearchResult struct {
	Command string `json:"command"`
	Summary string `json:"summary"`
	Usage   string `json:"usage"`
}

type cliSearchResponse struct {
	Query   string            `json:"query"`
	Results []cliSearchResult `json:"results"`
}

// These tasks exercise the registration, domain vocabulary, and ambiguous
// exports/syncs. The expectations name user actions, not scoring internals.
var cliSearchTasks = []struct {
	query string
	want  string
	rank  int
}{
	{"list recent conversations", "agentsview session list", 1},
	{"search chat messages for an error", "agentsview session search", 1},
	{"show messages from a transcript", "agentsview session messages", 1},
	{"show token cost for a conversation", "agentsview session usage", 1},
	{"export raw session JSONL", "agentsview session export", 1},
	{"export session summaries from the archive", "agentsview export sessions", 1},
	{"list projects and session counts", "agentsview project list", 1},
	{"list supported providers", "agentsview source list", 1},
	{"sync local agent sources", "agentsview source sync", 1},
	{"parse one session file", "agentsview session sync", 1},
	{"show daily spending", "agentsview report usage daily", 1},
	{"build vector search index", "agentsview index embeddings build", 1},
	{"show background server status", "agentsview daemon status", 3},
	{"list tool calls made during a conversation", "agentsview session tool-calls", 1},
	{"start the web UI server", "agentsview serve", 1},
	{"start MCP server", "agentsview mcp", 1},
}

func isolateCLISearch(tb testing.TB) (string, string) {
	tb.Helper()
	home, data := tb.TempDir(), tb.TempDir()
	tb.Setenv("HOME", home)
	tb.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	tb.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	tb.Setenv("AGENTSVIEW_DATA_DIR", data)
	tb.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	tb.Setenv("AGENTSVIEW_MODE", "invalid-discovery-fixture")
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR"} {
		tb.Setenv(name, filepath.Join(home, name))
	}
	return home, data
}

func runCLISearch(args ...string) (cliSearchResponse, string, error) {
	var stdout, stderr bytes.Buffer
	err := executeCLIWithLegacyFlagCompat(append([]string{"cli", "search"}, args...), &stdout, &stderr)
	var response cliSearchResponse
	if err == nil {
		err = json.Unmarshal(stdout.Bytes(), &response)
	}
	return response, stderr.String(), err
}

func TestCLISearchTasks(t *testing.T) {
	isolateCLISearch(t)
	hit1, hit3 := 0, 0
	for _, tt := range cliSearchTasks {
		t.Run(tt.query, func(t *testing.T) {
			got, stderr, err := runCLISearch(tt.query, "--json")
			require.NoError(t, err)
			assert.Empty(t, stderr)
			assert.Equal(t, tt.query, got.Query)
			commands := make([]string, len(got.Results))
			for i, result := range got.Results {
				commands[i] = result.Command
				for _, skipped := range cliSearchUnavailablePaths {
					assert.NotEqual(t, "agentsview "+skipped, result.Command)
				}
				assert.NotEmpty(t, result.Summary)
				assert.True(t, strings.HasPrefix(result.Usage, result.Command), result.Usage)
			}
			rank := slices.Index(commands, tt.want)
			if rank == 0 {
				hit1++
			}
			if rank >= 0 && rank < 3 {
				hit3++
			}
			assert.Contains(t, commands[:min(len(commands), tt.rank)], tt.want, "ranked: %v", commands)
		})
	}
	t.Logf("task eval: hit@1 %d/%d; hit@3 %d/%d", hit1, len(cliSearchTasks), hit3, len(cliSearchTasks))
}

func TestCLISearchMCPWithoutServiceActions(t *testing.T) {
	isolateCLISearch(t)
	root := newRootCommand()
	// Windows omits these service actions from discovery.
	for _, backend := range []string{"pg", "clickhouse"} {
		service, _, err := root.Find([]string{"replica", backend, "service"})
		require.NoError(t, err)
		for _, action := range []string{"uninstall", "status", "start", "stop"} {
			cmd := directCLICommand(service, action)
			require.NotNil(t, cmd)
			service.RemoveCommand(cmd)
		}
	}
	out, err := executeCommand(root, "cli", "search", "start MCP server", "--json", "--limit", "1")
	require.NoError(t, err)
	var got cliSearchResponse
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Results, 1)
	assert.Equal(t, "agentsview mcp", got.Results[0].Command)
}

func TestCLISearchJSONContract(t *testing.T) {
	isolateCLISearch(t)
	var out, stderr bytes.Buffer
	require.NoError(t, executeCLIWithLegacyFlagCompat([]string{"cli", "search", "session usage", "--json", "--limit", "1"}, &out, &stderr))
	assert.Empty(t, stderr.String())
	var got map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Len(t, got, 2)
	assert.Equal(t, "session usage", got["query"])
	results, ok := got["results"].([]any)
	require.True(t, ok)
	require.Len(t, results, 1)
	assert.Equal(t, map[string]any{
		"command": "agentsview session usage",
		"summary": "Show token usage and cost estimate for a session",
		"usage":   "agentsview session usage <id>",
	}, results[0])
}

func TestCLISearchTextAndHelp(t *testing.T) {
	isolateCLISearch(t)
	out, err := executeCommand(newRootCommand(), "cli", "search", "session usage", "--limit", "1")
	require.NoError(t, err)
	assert.Equal(t, "agentsview session usage  Show token usage and cost estimate for a session\nRun 'agentsview session usage --help' for flags and examples.\n", out)
	help, err := executeCommand(newRootCommand(), "cli")
	require.NoError(t, err)
	assert.Contains(t, help, "search")
}

func TestCLISearchRootDiscovery(t *testing.T) {
	isolateCLISearch(t)
	rootHelp, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(rootHelp, "Agents: find the right command with: agentsview cli search \"<what you want to do>\"\n"))
	// Exercise the exact advertised command safely; discovery never executes
	// recommended commands, including the placeholder's empty match.
	_, err = executeCommand(newRootCommand(), "cli", "search", "<what you want to do>")
	require.NoError(t, err)
}

func TestCLISearchNoMatches(t *testing.T) {
	isolateCLISearch(t)
	got, stderr, err := runCLISearch("qzxjkvwzz", "--json")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "qzxjkvwzz", got.Query)
	assert.Equal(t, []cliSearchResult{}, got.Results)
	out, err := executeCommand(newRootCommand(), "cli", "search", "qzxjkvwzz")
	require.NoError(t, err)
	assert.Contains(t, out, "Run 'agentsview --help' for flags and examples.")
}

func TestCLISearchDiagnostics(t *testing.T) {
	isolateCLISearch(t)
	for _, tt := range []struct {
		args []string
		code int
		kind string
	}{
		{nil, 64, "usage"},
		{[]string{" \t\n"}, 64, "usage"},
		{[]string{"session", "--limit", "0"}, 64, "usage"},
		{[]string{"session", "--limit", "21"}, 64, "usage"},
		{[]string{"session", "--bogus"}, 64, "usage"},
		{[]string{"session", "--limit"}, 64, "usage"},
		{[]string{"session", "--limit", "abc"}, 65, "validation"},
		{[]string{"session", "--format", "yaml"}, 65, "validation"},
		{[]string{"session", "--format"}, 64, "usage"},
		{[]string{"session", "--format="}, 65, "validation"},
		{[]string{"session", "--json=garbage"}, 65, "validation"},
		{[]string{"session", "--limit=999999999999999999999999999999"}, 65, "validation"},
	} {
		for _, selector := range [][]string{{"--json"}, {"--format", "json"}, {"--json=false"}} {
			t.Run(fmt.Sprint(tt.args, selector), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				args := append([]string{"cli", "search"}, selector...)
				args = append(args, tt.args...)
				err := executeCLIWithLegacyFlagCompat(args, &stdout, &stderr)
				require.Error(t, err)
				assert.Equal(t, tt.code, reportCLIError(err, &stderr))
				assert.Empty(t, stdout.String())
				if selector[0] == "--json=false" {
					assert.Contains(t, stderr.String(), "fatal:")
					assert.Contains(t, stderr.String(), "hint:")
				} else {
					var diagnostic map[string]string
					require.NoError(t, json.Unmarshal(stderr.Bytes(), &diagnostic))
					assert.Equal(t, tt.kind, diagnostic["error"])
					assert.Contains(t, diagnostic["hint"], "agentsview cli search --help")
					assert.Equal(t, 1, strings.Count(stderr.String(), "\n"))
				}
			})
		}
	}
}

type cliSearchNoNetwork struct{ calls int }

func (g *cliSearchNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	g.calls++
	return nil, errors.New("discovery must not contact a server")
}

func TestCLISearchDoesNotLoadConfigOrContactDaemon(t *testing.T) {
	home, data := isolateCLISearch(t)
	require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte("[invalid TOML"), 0o600))
	guard := new(cliSearchNoNetwork)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = guard
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	for _, args := range [][]string{{"session", "--json"}, {"daemon", "--format", "json"}, {"sync", "--json=false"}} {
		var stdout, stderr bytes.Buffer
		require.NoError(t, executeCLIWithLegacyFlagCompat(append([]string{"cli", "search"}, args...), &stdout, &stderr))
		assert.NotEmpty(t, stdout.String())
		assert.Empty(t, stderr.String())
	}
	assert.Zero(t, guard.calls)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
	entries, err = os.ReadDir(data)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.toml", entries[0].Name())
}

func TestCLISearchFormatSelectorsAgree(t *testing.T) {
	isolateCLISearch(t)
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		query = string([]rune(query)[:min(len([]rune(query)), 4096)])
		limit := hegel.Draw(ht, hegel.Integers(1, 20))
		if strings.TrimSpace(query) == "" {
			return // blank inputs are covered by TestCLISearchDiagnostics
		}
		var responses []cliSearchResponse
		for _, selector := range [][]string{
			{"--json"},
			{"--json=true"},
			{"--format", "json"},
			{"--format", "human", "--json"},
			{"--json=true", "--format=human"},
			{"--format=json", "--json=false"},
			{"--json=false", "--format=json"},
		} {
			args := append([]string{"--limit", strconv.Itoa(limit)}, selector...)
			args = append(args, "--", query)
			got, stderr, err := runCLISearch(args...)
			require.NoError(ht, err)
			assert.Empty(ht, stderr)
			assert.Equal(ht, query, got.Query)
			responses = append(responses, got)
		}
		for _, got := range responses[1:] {
			assert.Equal(ht, responses[0], got)
		}
	}, hegel.WithTestCases(1000))
}

type cliSearchFailWriter struct{ err error }

func (w cliSearchFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCLISearchPropagatesWriterFailure(t *testing.T) {
	isolateCLISearch(t)
	cause := errors.New("output fixture failure")
	for _, selector := range [][]string{nil, {"--json"}} {
		var stderr bytes.Buffer
		args := append([]string{"cli", "search", "session"}, selector...)
		err := executeCLIWithLegacyFlagCompat(args, cliSearchFailWriter{cause}, &stderr)
		require.ErrorIs(t, err, cause)
		assert.Equal(t, 1, reportCLIError(err, &stderr))
	}
}

var cliSearchUnavailablePaths = []string{
	"source", "project", "report", "replica", "index", "source cursor", "report usage",
	"session", "capture", "export", "doctor", "friction", "insight", "kata", "memory",
	"recall", "recall extract", "secrets", "skills", "source raw", "index embeddings",
	"replica pg", "replica clickhouse", "replica duckdb", "replica pg vectors",
	"replica pg service", "replica clickhouse service", "replica duckdb quack",
	"source chrome setup", "replica pg service install", "replica clickhouse service install",
	"sync", "usage daily", "chrome setup", "remote-sync", "remote-sync watch",
	"sync-worker", "chrome-host", "index classifier rebuild", "token-use",
	"help", "completion bash", "cli", "cli search",
}

func TestCLISearchOmitsUnavailableCommands(t *testing.T) {
	isolateCLISearch(t)
	for _, path := range cliSearchUnavailablePaths {
		t.Run(path, func(t *testing.T) {
			got, _, err := runCLISearch(path, "--json", "--limit", "20")
			require.NoError(t, err)
			for _, result := range got.Results {
				assert.NotEqual(t, "agentsview "+path, result.Command)
			}
		})
	}
}

func TestCLISearchHumanSelectorsAgree(t *testing.T) {
	isolateCLISearch(t)
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		runes := []rune(query)
		query = "session " + string(runes[:min(len(runes), 4096)])
		limit := strconv.Itoa(hegel.Draw(ht, hegel.Integers(1, 20)))
		want, err := executeCommand(newRootCommand(), "cli", "search", "--limit", limit, "--", query)
		require.NoError(ht, err)
		for _, selector := range [][]string{{"--json=false"}, {"--format=human"}, {"--format=human", "--json=false"}, {"--json=false", "--format=human"}} {
			args := append([]string{"cli", "search", "--limit", limit}, selector...)
			args = append(args, "--", query)
			got, err := executeCommand(newRootCommand(), args...)
			require.NoError(ht, err)
			assert.Equal(ht, want, got)
		}
	}, hegel.WithTestCases(1000))
}

func TestCLISearchRetainsPlatformActions(t *testing.T) {
	isolateCLISearch(t)
	paths := []string{"serve", "mcp", "signing keygen", "signing init-replay", "daemon start", "daemon stop", "daemon status", "daemon restart", "recall extract preview"}
	for _, backend := range []string{"pg", "clickhouse"} {
		for _, action := range []string{"install", "uninstall", "status", "start", "stop", "logs"} {
			paths = append(paths, "replica "+backend+" service "+action)
		}
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			root := newRootCommand()
			cmd, _, err := root.Find(strings.Fields(path))
			require.NoError(t, err)
			require.Equal(t, "agentsview "+path, cmd.CommandPath())
			// A unique description isolates eligibility from ranking and top-20
			// truncation while exercising the actual canonical tree and adapter.
			cmd.Short = "eligibilityfixture"
			out, err := executeCommand(root, "cli", "search", "eligibilityfixture", "--json", "--limit", "20")
			require.NoError(t, err)
			var got cliSearchResponse
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			want := true
			if strings.Contains(path, " service ") {
				action := cmd.Name()
				want = action == "logs" || (action != "install" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin"))
			}
			if want {
				require.Len(t, got.Results, 1)
				assert.Equal(t, "agentsview "+path, got.Results[0].Command)
			} else {
				assert.Empty(t, got.Results)
			}
		})
	}
}

func TestCLISearchPayloadBudget(t *testing.T) {
	isolateCLISearch(t)
	out, err := executeCommand(newRootCommand(), "cli", "search", "show token cost for a conversation")
	require.NoError(t, err)
	assert.Less(t, len(out), 1024)
	rootHelp, err := executeCommand(newRootCommand(), "--help")
	require.NoError(t, err)
	sessionHelp, err := executeCommand(newRootCommand(), "session", "--help")
	require.NoError(t, err)
	t.Logf("five-result text: %d bytes; root + session help: %d bytes", len(out), len(rootHelp)+len(sessionHelp))
}

func TestCLISearchUsesLateFlagErrorHandler(t *testing.T) {
	isolateCLISearch(t)
	cause := errors.New("late flag handler fixture")
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		runes := []rune(value)
		value = "invalid" + string(runes[:min(len(runes), 4096)])
		root := newRootCommand()
		root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
			assert.Equal(ht, "agentsview cli search", cmd.CommandPath())
			require.Error(ht, err)
			return cause
		})
		_, err := executeCommand(root, "cli", "search", "session", "--limit", value)
		assert.ErrorIs(ht, err, cause)
	}, hegel.WithTestCases(1000))
}

func TestCLISearchRetainsActionsAndSkippedParentsChildren(t *testing.T) {
	isolateCLISearch(t)
	hegel.Test(t, func(ht *hegel.T) {
		number := hegel.Draw(ht, hegel.Integers[int](math.MinInt, math.MaxInt))
		name := "fixture" + strconv.Itoa(number)
		root := newRootCommand()
		run := func(*cobra.Command, []string) error {
			require.FailNow(ht, "discovery executed a result")
			return nil
		}
		parent := &cobra.Command{Use: name, Short: "Fixture action", RunE: run}
		parent.AddCommand(&cobra.Command{Use: "child", Short: "Fixture child action", RunE: run})
		root.AddCommand(parent)
		doctor := directCLICommand(root, "doctor")
		require.NotNil(ht, doctor)
		doctor.AddCommand(&cobra.Command{Use: name, Short: "Fixture diagnostic", RunE: run})
		out, err := executeCommand(root, "cli", "search", name, "--json", "--limit", "20")
		require.NoError(ht, err)
		var got cliSearchResponse
		require.NoError(ht, json.Unmarshal([]byte(out), &got))
		var commands []string
		for _, result := range got.Results {
			commands = append(commands, result.Command)
		}
		// A negative suffix also searches a numeric token that may legitimately
		// match other commands. Eligibility requires all three fixture actions.
		assert.Subset(ht, commands, []string{"agentsview " + name, "agentsview " + name + " child", "agentsview doctor " + name})
	}, hegel.WithTestCases(1000))
}

func BenchmarkCLISearch(b *testing.B) {
	isolateCLISearch(b)
	root := newRootCommand()
	for b.Loop() {
		_, err := executeCommand(root, "cli", "search", "show token cost for a conversation")
		require.NoError(b, err)
	}
}
