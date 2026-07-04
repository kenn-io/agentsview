// ABOUTME: `session search` subcommand — substring/regex/fts content
// ABOUTME: search across messages and tool I/O with redacted snippets.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionSearchCommand() *cobra.Command {
	var (
		useRegex, useFTS                  bool
		in                                string
		excludeSystem, reveal             bool
		project, excludeProject, agent    string
		machine, branch                   string
		date, dateFrom, dateTo            string
		activeSince                       string
		includeChildren, includeAutomated bool
		includeOneShot                    bool
		limit, cursor                     int
	)
	cmd := &cobra.Command{
		Use:          "search <pattern>",
		Short:        "Search message and tool content across sessions",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if useRegex && useFTS {
				return fmt.Errorf("--regex and --fts are mutually exclusive")
			}
			var sources []string
			for s := range strings.SplitSeq(in, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sources = append(sources, s)
				}
			}
			if useFTS {
				for _, s := range sources {
					if s != "messages" {
						return fmt.Errorf(
							"--fts searches messages only; drop --in or --fts")
					}
				}
			}
			mode := "substring"
			switch {
			case useRegex:
				mode = "regex"
			case useFTS:
				mode = "fts"
			}
			gitBranch, err := branchFilterToken(project, branch)
			if err != nil {
				return err
			}
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			res, err := svc.SearchContent(cmd.Context(), service.ContentSearchRequest{
				Pattern:          args[0],
				Mode:             mode,
				Sources:          sources,
				ExcludeSystem:    excludeSystem,
				Reveal:           reveal,
				Project:          project,
				ExcludeProject:   excludeProject,
				Machine:          machine,
				GitBranch:        gitBranch,
				Agent:            agent,
				Date:             date,
				DateFrom:         dateFrom,
				DateTo:           dateTo,
				ActiveSince:      activeSince,
				IncludeChildren:  includeChildren,
				IncludeAutomated: includeAutomated,
				IncludeOneShot:   includeOneShot,
				Limit:            limit,
				Cursor:           cursor,
			})
			if err != nil {
				return err
			}
			if reveal {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"WARNING: --reveal prints full secret values; "+
						"this terminal/session may itself be recorded.")
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
			}
			return printContentMatchesHuman(cmd.OutOrStdout(), res)
		},
	}
	flags := cmd.Flags()
	flags.BoolVar(&useRegex, "regex", false, "Treat pattern as an RE2 regex")
	flags.BoolVar(&useFTS, "fts", false, "Fast tokenized FTS over messages only")
	flags.StringVar(&in, "in", "",
		"Comma-separated sources: messages,tool_input,tool_result (default all)")
	flags.BoolVar(&excludeSystem, "exclude-system", false,
		"Exclude system messages (included by default)")
	flags.BoolVar(&reveal, "reveal", false, "Show full secret values (unredacted)")
	flags.StringVar(&project, "project", "", "Filter by project name")
	flags.StringVar(&excludeProject, "exclude-project", "", "Exclude project")
	flags.StringVar(&machine, "machine", "", "Filter by machine")
	flags.StringVar(&branch, "branch", "", "Filter by git branch name (requires --project)")
	flags.StringVar(&agent, "agent", "", "Filter by agent")
	flags.StringVar(&date, "date", "", "Sessions started on YYYY-MM-DD")
	flags.StringVar(&dateFrom, "date-from", "", "Sessions on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "", "Sessions on or before YYYY-MM-DD")
	flags.StringVar(&activeSince, "active-since", "", "Active since RFC3339 timestamp")
	flags.BoolVar(&includeChildren, "include-children", false, "Include subagent sessions")
	flags.BoolVar(&includeAutomated, "include-automated", false, "Include automated sessions")
	flags.BoolVar(&includeOneShot, "include-one-shot", false, "Include one-shot sessions")
	flags.IntVar(&limit, "limit", 0, "Max results (default 50, max 500)")
	flags.IntVar(&cursor, "cursor", 0, "Pagination cursor from a previous response")
	return cmd
}

// printContentMatchesHuman writes one line per match, terminal-sanitized.
func printContentMatchesHuman(w io.Writer, res *service.ContentSearchResult) error {
	if len(res.Matches) == 0 {
		fmt.Fprintln(w, "(no matches)")
		return nil
	}
	for _, m := range res.Matches {
		loc := m.Location
		if m.ToolName != "" {
			loc = m.Location + ":" + m.ToolName
		}
		fmt.Fprintf(w, "%s  #%d  %s  %s\n",
			sanitizeTerminal(m.SessionID), m.Ordinal,
			sanitizeTerminal(m.Project), sanitizeTerminal(loc))
		fmt.Fprintf(w, "    %s\n",
			sanitizeTerminal(strings.ReplaceAll(m.Snippet, "\n", " ")))
	}
	if res.NextCursor != 0 {
		fmt.Fprintf(w, "\nMore results: --cursor %d\n", res.NextCursor)
	}
	return nil
}
