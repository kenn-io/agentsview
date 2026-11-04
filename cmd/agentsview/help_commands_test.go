package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Core commands must carry an example, and leaf commands an explanation.
var agentHelpCoreCommands = []string{
	"serve", "serve status", "serve stop", "daemon", "daemon start", "daemon stop", "daemon status", "daemon restart", "openapi", "update", "mcp",
	"source", "source list", "source sync", "source import", "source chrome", "source raw", "source cursor", "source cursor sync-usage",
	"project", "project list", "report", "report stats", "report activity", "report usage", "report usage daily", "report usage statusline",
	"replica", "replica pg", "replica duckdb", "replica clickhouse", "index", "index embeddings", "index classifier", "archive diff",
	"session", "session list", "session get", "session messages", "session tool-calls", "session search", "session usage", "session usage-batch", "session watch", "session export", "session sync", "session label", "session parent", "session health", "session prune",
	"recall", "insight", "friction", "memory", "skills", "secrets", "export", "doctor",
}

func TestAgentAdvertisedCommandsParse(t *testing.T) {
	root := newRootCommand()
	help, err := executeCommand(root, "--help")
	require.NoError(t, err)
	sources := map[string]string{"root help": help}
	walkHelpCommands(root, func(cmd *cobra.Command) {
		sources[cmd.CommandPath()] = cmd.Long
		if cmd.Parent() == nil {
			return
		}
		path := strings.TrimPrefix(cmd.CommandPath(), "agentsview ")
		if slices.Contains(agentHelpCoreCommands, path) {
			require.NotEmpty(t, cmd.Example, "%s needs a usable invocation", cmd.CommandPath())
			if !cmd.HasSubCommands() {
				require.NotEmpty(t, cmd.Long, "%s needs an explanation and its relevant limits", cmd.CommandPath())
			}
		}
		args := append(strings.Fields(path), "--help")
		rendered, err := executeCommand(newRootCommand(), args...)
		require.NoError(t, err, "%s help", cmd.CommandPath())
		assert.Contains(t, rendered, cmd.CommandPath())
		if slices.Contains(agentHelpCoreCommands, path) {
			assert.NotContains(t, rendered, "Deprecated:", "%s is a canonical command", cmd.CommandPath())
		}
		if cmd.HasSubCommands() {
			require.NotEmpty(t, cmd.Long, "%s needs a concept explanation", cmd.CommandPath())
		}
		if cmd.Example != "" {
			assert.Contains(t, rendered, "Examples:")
			assert.Contains(t, rendered, strings.TrimSpace(cmd.Example), "%s help must render its declared example", cmd.CommandPath())
			require.NoError(t, validateCommandExamples(cmd.CommandPath(), cmd.Example))
		}
	})
	assert.Contains(t, help, "Agent quick start:")
	assert.Contains(t, help, "Search and reports:")
	tokenUseHelp, err := executeCommand(newRootCommand(), "token-use", "--help")
	require.NoError(t, err, "token-use help")
	require.NotEmpty(t, tokenUseHelp, "token-use help must be rendered")
	assert.Contains(t, tokenUseHelp, "Deprecated: use 'agentsview session usage ID --json'.")
	for name, source := range sources {
		commands := advertisedHelpCommands(source)
		if name == "root help" {
			require.NotEmpty(t, commands, name)
		}
		for _, line := range commands {
			require.NoError(t, validateAdvertisedCommand(line), "%s: %s", name, line)
		}
	}
}

func TestAgentHelpCommandChecker(t *testing.T) {
	for _, line := range []string{
		"agentsview session get <id> --json",
		"agentsview session search 'connection reset' --json",
		"agentsview openapi > openapi.json",
		"agentsview session search 'a > b' --json",
	} {
		require.NoError(t, validateAdvertisedCommand(line), line)
	}
	for _, line := range []string{
		"agentsview session made-up --help",
		"agentsview session list --nonexistent",
		"agentsview session get",
		"agentsview session search 'unterminated",
		"agentsview openapi >>>out",
	} {
		require.Error(t, validateAdvertisedCommand(line), line)
	}
	require.Error(t, validateCommandExamples("agentsview source list", "agentsview report stats --json"))
}

func TestSelectedHelpExplainsReplicaBackendsAndExportExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "ClickHouse service",
			args: []string{"replica", "clickhouse", "--help"},
			want: "Configure a ClickHouse target before push, status, or serving.",
		},
		{
			name: "session backend selection",
			args: []string{"session", "--help"},
			want: "Local reads use the daemon. --server and --pg are mutually exclusive.",
		},
		{
			name: "conversation export failures",
			args: []string{"export", "conversations", "--help"},
			want: "Exit 4: archive reconciliation required. Exit 5: message revision changed.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := executeCommand(newRootCommand(), tc.args...)
			require.NoError(t, err)
			assert.Contains(t, rendered, tc.want)
		})
	}
}

func walkHelpCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Hidden || cmd.Name() == "completion" {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkHelpCommands(child, visit)
	}
}

var helpInlineCommands = regexp.MustCompile("`([^`]+)`|'([^']+)'|\"([^\"]+)\"")

// advertisedHelpCommands returns lines that start with agentsview and quoted
// agentsview commands inside prose.
func advertisedHelpCommands(source string) []string {
	var commands []string
	for line := range strings.Lines(source) {
		line = strings.TrimSpace(line)
		if line == "agentsview <command> [flags]" || line == "agentsview [flags]" ||
			strings.Contains(line, " - local web viewer for AI agent sessions") {
			continue
		}
		if strings.HasPrefix(line, "agentsview") {
			commands = append(commands, line)
			continue
		}
		for _, match := range helpInlineCommands.FindAllStringSubmatch(line, -1) {
			for _, span := range match[1:] {
				if strings.HasPrefix(span, "agentsview") {
					commands = append(commands, span)
				}
			}
		}
	}
	return commands
}

func validateAdvertisedCommand(line string) error {
	_, err := resolveAdvertisedCommand(line)
	return err
}

// Help examples use single quotes, <placeholder> words, and at most one
// trailing "> file" redirect. Split with shlex so quoted text stays one word,
// then drop an unquoted trailing redirect.
func resolveAdvertisedCommand(line string) (*cobra.Command, error) {
	words, err := shlex.Split(line)
	if err != nil {
		return nil, err
	}
	if n := len(words); n >= 2 && words[n-2] == ">" {
		words = words[:n-2]
	}
	if len(words) == 0 || words[0] != "agentsview" {
		return nil, errors.New("expected an agentsview command")
	}
	return validateAdvertisedArgs(words)
}

func validateCommandExamples(path, source string) error {
	count := 0
	for line := range strings.Lines(source) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		cmd, err := resolveAdvertisedCommand(line)
		if err != nil {
			return fmt.Errorf("%s example %q: %w", path, line, err)
		}
		if cmd.CommandPath() != path && !strings.HasPrefix(cmd.CommandPath(), path+" ") {
			return fmt.Errorf("%s example %q resolves to %s outside its command path", path, line, cmd.CommandPath())
		}
	}
	if count == 0 {
		return fmt.Errorf("%s has no command examples", path)
	}
	return nil
}

func validateAdvertisedArgs(words []string) (*cobra.Command, error) {
	for i, word := range words {
		if strings.HasPrefix(word, "<") && strings.HasSuffix(word, ">") {
			words[i] = "example"
		}
	}
	root := newRootCommand()
	cmd, rest, err := root.Find(words[1:])
	if err != nil {
		return nil, err
	}
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return nil, err
	}
	args := cmd.Flags().Args()
	if cmd.HasSubCommands() && len(args) > 0 {
		return nil, fmt.Errorf("unknown command path: %v", args)
	}
	help, _ := cmd.Flags().GetBool("help")
	if help {
		return cmd, nil
	}
	if err := cmd.ValidateArgs(args); err != nil {
		return nil, err
	}
	if err := cmd.ValidateRequiredFlags(); err != nil {
		return nil, err
	}
	if err := cmd.ValidateFlagGroups(); err != nil {
		return nil, err
	}
	return cmd, nil
}
