// ABOUTME: `session search` subcommand — substring/regex/fts content
// ABOUTME: search across messages and tool I/O with redacted snippets.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionSearchCommand() *cobra.Command {
	var (
		useRegex, useFTS, useSemantic, useHybrid bool
		in                                       string
		excludeSystem, reveal                    bool
		project, excludeProject, agent           string
		machine, date, dateFrom, dateTo          string
		activeSince                              string
		includeChildren, includeAutomated        bool
		includeOneShot                           bool
		limit, cursor, contextN                  int
	)
	cmd := &cobra.Command{
		Use:          "search <pattern>",
		Short:        "Search message and tool content across sessions",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sources []string
			for s := range strings.SplitSeq(in, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sources = append(sources, s)
				}
			}
			mode, err := resolveContentSearchMode(
				useRegex, useFTS, useSemantic, useHybrid, sources)
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
				Context:          contextN,
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
	flags.BoolVar(&useSemantic, "semantic", false,
		"Semantic (vector) search over user/assistant messages")
	flags.BoolVar(&useHybrid, "hybrid", false,
		"Hybrid semantic + full-text search (reciprocal rank fusion)")
	flags.StringVar(&in, "in", "",
		"Comma-separated sources: messages,tool_input,tool_result (default all)")
	flags.BoolVar(&excludeSystem, "exclude-system", false,
		"Exclude system messages (included by default)")
	flags.BoolVar(&reveal, "reveal", false, "Show full secret values (unredacted)")
	flags.StringVar(&project, "project", "", "Filter by project name")
	flags.StringVar(&excludeProject, "exclude-project", "", "Exclude project")
	flags.StringVar(&machine, "machine", "", "Filter by machine")
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
	flags.IntVar(&contextN, "context", 0,
		"Include N messages of context before and after each match (max 10)")
	return cmd
}

// resolveContentSearchMode picks the search mode from the mutually exclusive
// --regex/--fts/--semantic/--hybrid flags and, for the modes that only search
// message content ("fts", "semantic", "hybrid"), rejects an explicit --in
// naming other sources.
func resolveContentSearchMode(
	useRegex, useFTS, useSemantic, useHybrid bool, sources []string,
) (string, error) {
	modes := 0
	for _, b := range []bool{useRegex, useFTS, useSemantic, useHybrid} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		return "", fmt.Errorf(
			"--regex, --fts, --semantic and --hybrid are mutually exclusive")
	}
	mode := "substring"
	switch {
	case useRegex:
		mode = "regex"
	case useFTS:
		mode = "fts"
	case useSemantic:
		mode = "semantic"
	case useHybrid:
		mode = "hybrid"
	}
	if useFTS {
		for _, s := range sources {
			if s != "messages" {
				return "", fmt.Errorf(
					"--fts searches messages only; drop --in or --fts")
			}
		}
	}
	if useSemantic || useHybrid {
		for _, s := range sources {
			if s != "messages" {
				return "", fmt.Errorf(
					"--%s searches messages only; drop --in", mode)
			}
		}
	}
	return mode, nil
}

// printContentMatchesHuman writes one line per match, terminal-sanitized.
// Scored matches (semantic/hybrid modes) show "score=0.83" after the
// ordinal; unscored matches (substring/regex/fts) omit it. When --context
// requested inline context, ContextBefore/ContextAfter print as indented
// "role: content" lines around the match line.
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
		for _, cm := range m.ContextBefore {
			printContentContextLine(w, cm)
		}
		fmt.Fprintf(w, "%s  #%d", sanitizeTerminal(m.SessionID), m.Ordinal)
		if m.Score != nil {
			fmt.Fprintf(w, " score=%.2f", *m.Score)
		}
		fmt.Fprintf(w, "  %s  %s\n",
			sanitizeTerminal(m.Project), sanitizeTerminal(loc))
		fmt.Fprintf(w, "    %s\n",
			sanitizeTerminal(strings.ReplaceAll(m.Snippet, "\n", " ")))
		for _, cm := range m.ContextAfter {
			printContentContextLine(w, cm)
		}
	}
	if res.NextCursor != 0 {
		fmt.Fprintf(w, "\nMore results: --cursor %d\n", res.NextCursor)
	}
	return nil
}

// contentContextLineMaxChars caps a printed context line's length so a long
// stored message cannot blow out the human-format search output.
const contentContextLineMaxChars = 200

// printContentContextLine writes one --context line: two-space indent,
// "role: " prefix, terminal-sanitized and truncated to
// contentContextLineMaxChars runes (with an ellipsis marker when cut).
func printContentContextLine(w io.Writer, m db.Message) {
	content := strings.ReplaceAll(m.Content, "\n", " ")
	if truncated, cut := truncateRunes(content, contentContextLineMaxChars); cut {
		content = truncated + "…"
	} else {
		content = truncated
	}
	fmt.Fprintf(w, "  %s: %s\n",
		sanitizeTerminal(m.Role), sanitizeTerminal(content))
}
