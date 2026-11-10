package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"runtime"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func newCLISearchCommand(root *cobra.Command) *cobra.Command {
	// Distinguish source discovery from parsing a single file without changing
	// either command's help text.
	clisearch.SetKeywords(directCLICommand(directCLICommand(root, "source"), "sync"), "scan ingest local agent sources")
	clisearch.SetKeywords(directCLICommand(directCLICommand(root, "session"), "sync"), "parse reparse session file")
	clisearch.SetKeywords(directCLICommand(directCLICommand(root, "session"), "export"), "raw source JSONL")
	// Prioritize explicit MCP queries over generic daemon startup.
	clisearch.SetKeywords(directCLICommand(root, "mcp"), "MCP")
	cmd := clisearch.NewCommand(root, clisearch.Options{
		Synonyms: map[string][]string{
			"conversation": {"session"}, "chat": {"session"}, "transcript": {"session"},
			"provider": {"agent"}, "harness": {"agent"},
			"workspace": {"project"}, "repository": {"project"}, "repo": {"project"},
			"spend": {"cost"}, "spending": {"cost"}, "price": {"cost"},
			"ingest": {"sync"}, "vector": {"embedding"},
		},
		Skip: skipCLISearchCommand,
		UsageError: func(err error) error {
			return cliUsageError(err, "run `agentsview cli search --help` for flags and examples")
		},
		Render: func(cmd *cobra.Command, response clisearch.Response) error {
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), response)
			}
			return clisearch.RenderText(cmd, response)
		},
	})
	cmd.GroupID = groupMeta
	cmd.Long = "Discover commands locally from a task description, without configuration, an archive, or a daemon."
	search := directCLICommand(cmd, "search")
	registerFormatFlags(search.Flags())
	// Diagnostics are installed on root after construction. Resolve its flag
	// handler at execution time so malformed values retain validation exit 65.
	search.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return root.FlagErrorFunc()(cmd, err)
	})
	return cmd
}

// Skip individual help-only entry points, rather than every parent: serve
// and mcp perform actions themselves, and future runnable parents should be
// discovered automatically. Kit still visits a skipped node's children.
func skipCLISearchCommand(cmd *cobra.Command) bool {
	switch cmd.CommandPath() {
	case "agentsview source", "agentsview project", "agentsview report",
		"agentsview replica", "agentsview index", "agentsview source cursor",
		"agentsview report usage", "agentsview session", "agentsview capture",
		"agentsview export", "agentsview doctor", "agentsview friction",
		"agentsview insight", "agentsview kata", "agentsview memory",
		"agentsview recall", "agentsview secrets", "agentsview skills",
		"agentsview source raw", "agentsview index embeddings",
		"agentsview replica pg", "agentsview replica clickhouse",
		"agentsview replica duckdb", "agentsview replica pg vectors",
		"agentsview replica pg service", "agentsview replica clickhouse service",
		"agentsview replica duckdb quack":
		return true
	case "agentsview recall extract":
		// The legacy parent action has an explicit preview child.
		return true
	case "agentsview source chrome setup",
		"agentsview replica pg service install",
		"agentsview replica clickhouse service install":
		// Browser setup requires human follow-through; service installation
		// can prompt to enable lingering and has no noninteractive selector.
		return true
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && cmd.Parent() != nil {
		switch cmd.Parent().CommandPath() {
		case "agentsview replica pg service", "agentsview replica clickhouse service":
			switch cmd.Name() {
			case "uninstall", "status", "start", "stop":
				return true
			}
		}
	}
	return false
}
