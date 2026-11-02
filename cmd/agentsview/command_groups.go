package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type commandMove struct {
	legacy    string
	canonical string
	new       func() *cobra.Command
	example   string
}

// Construct both trees independently: Cobra parsing state, flag values and
// handler closures must never be shared between the two spellings.
var commandMoves = []commandMove{
	{"agents", "source list", newAgentsCommand, "agentsview source list codex --json"},
	{"sync", "source sync", newSyncCommand, "agentsview source sync --full"},
	{"import", "source import", newImportCommand, "agentsview source import --type chatgpt export.json\nagentsview source import --type claude-ai --sync"},
	{"chrome", "source chrome", newChromeCommand, "agentsview source chrome setup"},
	{"raw-sync", "source raw", newRawSyncCommand, "agentsview source raw status"},
	{"projects", "project list", newProjectsCommand, "agentsview project list --json"},
	{"health", "session health", newHealthCommand, "agentsview session health --limit 10 --json"},
	{"prune", "session prune", newPruneCommand, "agentsview session prune --project scratch --dry-run"},
	{"stats", "report stats", newStatsCommand, "agentsview report stats --json"},
	{"activity", "report activity", newActivityReportCommand, "agentsview report activity --preset week"},
	{"usage", "report usage", newReportUsageCommand, "agentsview report usage daily --no-sync"},
	{"pg", "replica pg", newPGCommand, "agentsview replica pg status"},
	{"duckdb", "replica duckdb", newDuckDBCommand, "agentsview replica duckdb status"},
	{"clickhouse", "replica clickhouse", newClickHouseCommand, "agentsview replica clickhouse status"},
	{"embeddings", "index embeddings", newEmbeddingsCommand, "agentsview index embeddings list"},
	{"classifier", "index classifier", newClassifierCommand, "agentsview index classifier rebuild"},
	{"parse-diff", "archive diff", newParseDiffCommand, "agentsview archive diff --agent claude --limit 10"},
}

func newCommandDomain(name, short, long, group, example string) *cobra.Command {
	return &cobra.Command{
		Use: name, Short: short, Long: long, GroupID: group, Example: example,
		SilenceUsage: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

func newReportUsageCommand() *cobra.Command {
	cmd := newCommandDomain("usage", "Report token consumption and costs",
		"Read daily costs or a compact status line. To ingest Cursor admin usage,\n"+
			"use 'agentsview source cursor sync-usage'.", "", "")
	cmd.AddCommand(newUsageDailyCommand(), newUsageStatuslineCommand())
	return cmd
}

func groupCommands(root *cobra.Command) {
	root.AddCommand(
		newCommandDomain("source", "Discover and ingest session sources",
			"List supported agents, sync their sessions, import conversations, or upload\n"+
				"raw sources. These actions may read source files and write the archive.", groupCore,
			"agentsview source list --json\nagentsview source sync\nagentsview source import --type chatgpt export.json"),
		newCommandDomain("project", "Browse projects in the archive",
			"List projects and their recorded session counts.", groupCore,
			"agentsview project list --json"),
		newCommandDomain("report", "Report activity, usage, and workspace statistics",
			"Read activity, costs, and statistics from recorded sessions. Use each\n"+
				"action's --help for its time window, filters, and output formats.", groupUsage,
			"agentsview report stats --json\nagentsview report activity --preset week\nagentsview report usage daily --no-sync"),
		newCommandDomain("replica", "Sync and serve database replicas",
			"Choose pg, duckdb, or clickhouse, then an action such as push, status,\n"+
				"or serve. Configure a backend before pushing or serving its replica.", groupData,
			"agentsview replica pg --help\nagentsview replica duckdb status"),
		newCommandDomain("index", "Build and manage derived search indexes",
			"Manage semantic embedding generations. Indexes are derived from the\n"+
				"archive; building or activating one may update local derived data.", groupData,
			"agentsview index embeddings list\nagentsview index embeddings build --help"),
	)
	source := directCLICommand(root, "source")
	cursor := newCommandDomain("cursor", "Ingest Cursor admin usage",
		"Import usage events from the configured Cursor admin API.", "",
		"agentsview source cursor sync-usage --since 2026-01-01")
	source.AddCommand(cursor)
	addCanonicalCommand(root, "source cursor sync-usage", newUsageCursorCommand())

	for _, move := range commandMoves {
		legacy := directCLICommand(root, move.legacy)
		if legacy == nil {
			panic("missing legacy CLI command: " + move.legacy)
		}
		canonical := move.new()
		canonical.Example = move.example
		addCanonicalCommand(root, move.canonical, canonical)
		if move.canonical == "source import" {
			canonical.Use = "import [path]"
		}
		if move.canonical == "session prune" {
			guardLocalPruneFlags(canonical)
		}
		markLegacyCommand(legacy, move.legacy, move.canonical)
		legacy.Hidden = true
	}
	// This command already has its own JSON-only handler and deprecation notice.
	// Keep it executable, but direct new clients to session usage --json.
	tokenUse := directCLICommand(root, "token-use")
	if tokenUse == nil {
		panic("missing legacy CLI command: token-use")
	}
	tokenUse.Hidden = true
	tokenUse.Long = "Deprecated: use 'agentsview session usage ID --json'.\n" +
		"This spelling retains its JSON-only output and existing exit codes."
}

func directCLICommand(parent *cobra.Command, name string) *cobra.Command {
	for _, child := range parent.Commands() {
		if child.Name() == name {
			return child
		}
	}
	return nil
}

func addCanonicalCommand(root *cobra.Command, path string, cmd *cobra.Command) {
	parts := strings.Fields(path)
	parent := root
	for _, name := range parts[:len(parts)-1] {
		parent = directCLICommand(parent, name)
		if parent == nil {
			panic("missing CLI domain for " + path)
		}
	}
	name := parts[len(parts)-1]
	if directCLICommand(parent, name) != nil {
		panic("duplicate CLI command: " + path)
	}
	_, synopsis, _ := strings.Cut(cmd.Use, " ")
	cmd.Use = name
	if synopsis != "" {
		cmd.Use += " " + synopsis
	}
	// GroupIDs refer to the immediate parent's groups, not root's groups.
	cmd.GroupID = ""
	parent.AddCommand(cmd)
}

func markLegacyCommand(cmd *cobra.Command, legacy, replacement string) {
	// These old subtrees change shape in the canonical grammar.
	switch legacy {
	case "activity report":
		replacement = "report activity"
	case "usage cursor":
		replacement = "source cursor sync-usage"
	}
	notice := fmt.Sprintf("warning: '%s' is deprecated; use 'agentsview %s' instead\n", legacy, replacement)
	long := cmd.Long
	if long == "" {
		long = cmd.Short
	}
	cmd.Long = long + "\n\nDeprecated: use 'agentsview " + replacement + "'."
	// Cobra's Deprecated field prints through OutOrStderr, which inherits this
	// CLI's stdout writer. Wrap handlers instead so JSON stdout stays intact.
	if run := cmd.Run; run != nil {
		cmd.Run = func(cmd *cobra.Command, args []string) {
			fmt.Fprint(cmd.ErrOrStderr(), notice)
			run(cmd, args)
		}
	}
	if run := cmd.RunE; run != nil {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.ErrOrStderr(), notice)
			return run(cmd, args)
		}
	}
	for _, child := range cmd.Commands() {
		if !child.Hidden {
			markLegacyCommand(child, legacy+" "+child.Name(), replacement+" "+child.Name())
		}
	}
}

func guardLocalPruneFlags(cmd *cobra.Command) {
	cmd.Long = "Prune sessions from the local archive using the selected filters.\n" +
		"Use --dry-run to inspect the selection. Remote and output-format flags\n" +
		"inherited from session are not supported by this local maintenance action."
	previous := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		for _, name := range []string{"server", "server-token-file", "pg", "json", "format"} {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("session prune only supports the local archive; --%s is not supported", name)
			}
		}
		if previous != nil {
			return previous(cmd, args)
		}
		return nil
	}
}
