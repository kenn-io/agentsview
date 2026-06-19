// ABOUTME: `session list` subcommand — lists sessions with the
// ABOUTME: full set of HTTP query-param equivalents as CLI flags.
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

func newSessionListCommand() *cobra.Command {
	var (
		project, excludeProject, machine, agent string
		date, dateFrom, dateTo, activeSince     string
		minMessages, maxMessages                int
		minUserMessages                         int
		includeOneShot                          bool
		includeAutomated, includeChildren       bool
		outcome, healthGrade                    string
		minToolFailures                         int
		hasSecret                               bool
		cursor                                  string
		limit                                   int
		sort                                    string
		reverse                                 bool
	)
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List sessions with filters",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			f := service.ListFilter{
				Project:          project,
				ExcludeProject:   excludeProject,
				Machine:          machine,
				Agent:            agent,
				Date:             date,
				DateFrom:         dateFrom,
				DateTo:           dateTo,
				ActiveSince:      activeSince,
				MinMessages:      minMessages,
				MaxMessages:      maxMessages,
				MinUserMessages:  minUserMessages,
				IncludeOneShot:   includeOneShot,
				IncludeAutomated: includeAutomated,
				IncludeChildren:  includeChildren,
				Outcome:          outcome,
				HealthGrade:      healthGrade,
				HasSecret:        hasSecret,
				Cursor:           cursor,
				Limit:            limit,
			}
			if cmd.Flags().Changed("min-tool-failures") {
				f.MinToolFailures = &minToolFailures
			}
			// Parse the multi-key sort spec; --reverse flips the natural
			// direction of any term left without an explicit :asc/:desc, which
			// is folded into the canonical spec string so the wire form fully
			// captures the ordering.
			keys, err := db.ParseSortSpec(sort)
			if err != nil {
				return fmt.Errorf("invalid sort %q: %w", sort, err)
			}
			// An empty spec means the implicit default; materialize it so
			// --reverse has a term to flip instead of silently no-opping.
			if len(keys) == 0 {
				keys = []db.SortKey{{Key: db.DefaultSortKey()}}
			}
			if reverse {
				for i := range keys {
					if keys[i].Descending == nil {
						d := !db.SortDefaultDescending(keys[i].Key)
						keys[i].Descending = &d
					}
				}
			}
			f.OrderBy = db.FormatSortSpec(keys)

			list, err := svc.List(cmd.Context(), f)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
			}
			return printSessionListHuman(cmd.OutOrStdout(), list)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&project, "project", "",
		"Filter by project name")
	flags.StringVar(&excludeProject, "exclude-project", "",
		"Exclude sessions from the given project")
	flags.StringVar(&machine, "machine", "",
		"Filter by machine name")
	flags.StringVar(&agent, "agent", "",
		"Filter by agent (claude, codex, cursor, ...)")
	flags.StringVar(&date, "date", "",
		"Filter sessions started on YYYY-MM-DD")
	flags.StringVar(&dateFrom, "date-from", "",
		"Filter sessions started on or after YYYY-MM-DD")
	flags.StringVar(&dateTo, "date-to", "",
		"Filter sessions started on or before YYYY-MM-DD")
	flags.StringVar(&activeSince, "active-since", "",
		"Filter sessions active since RFC3339 timestamp")
	flags.IntVar(&minMessages, "min-messages", 0,
		"Minimum total message count")
	flags.IntVar(&maxMessages, "max-messages", 0,
		"Maximum total message count")
	flags.IntVar(&minUserMessages, "min-user-messages", 0,
		"Minimum user message count")
	flags.BoolVar(&includeOneShot, "include-one-shot", false,
		"Include one-shot sessions (excluded by default)")
	flags.BoolVar(&includeAutomated, "include-automated", false,
		"Include automated sessions (excluded by default)")
	flags.BoolVar(&includeChildren, "include-children", false,
		"Include subagent/child sessions")
	flags.StringVar(&outcome, "outcome", "",
		"Filter by outcome (comma-separated: success,failure,...)")
	flags.StringVar(&healthGrade, "health-grade", "",
		"Filter by health grade (comma-separated: A,B,C,D,F)")
	flags.IntVar(&minToolFailures, "min-tool-failures", 0,
		"Minimum tool-failure signal count (0 is a valid filter)")
	flags.BoolVar(&hasSecret, "has-secret", false,
		"Only sessions with detected secret leaks")
	flags.StringVar(&cursor, "cursor", "",
		"Pagination cursor from a previous response")
	flags.IntVar(&limit, "limit", 0,
		fmt.Sprintf(
			"Maximum sessions to return (default %d, max %d)",
			db.DefaultSessionLimit, db.MaxSessionLimit,
		))
	flags.StringVar(&sort, "sort", "recent",
		"Sort by a comma-separated list of keys, each optionally key:asc or "+
			"key:desc (e.g. messages:desc,started:asc). Keys: "+
			strings.Join(db.SortKeys(), ", "))
	flags.BoolVarP(&reverse, "reverse", "r", false,
		"Reverse the natural direction of sort keys that have no explicit "+
			":asc/:desc suffix")

	return cmd
}

// printSessionListHuman writes a compact columnar summary of the
// session list, with a trailing hint when another page is
// available. Prints "(no sessions)" for empty lists.
func printSessionListHuman(
	w io.Writer, list *service.SessionList,
) error {
	if len(list.Sessions) == 0 {
		fmt.Fprintln(w, "(no sessions)")
		return nil
	}
	fmt.Fprintf(w, "%-40s  %-20s  %-15s  %s\n",
		"ID", "PROJECT", "AGENT", "STARTED")
	for _, s := range list.Sessions {
		started := "-"
		if s.StartedAt != nil && len(*s.StartedAt) >= 16 {
			started = (*s.StartedAt)[:16]
		}
		fmt.Fprintf(w, "%-40s  %-20s  %-15s  %s\n",
			sanitizeTerminal(s.ID),
			sanitizeTerminal(s.Project),
			sanitizeTerminal(s.Agent),
			sanitizeTerminal(started))
	}
	if list.NextCursor != "" {
		// Cursor is an opaque server-minted string. Sanitize too
		// so a malicious DB row can't feed escapes through a hint.
		fmt.Fprintf(w, "\nMore results: --cursor %s\n",
			sanitizeTerminal(list.NextCursor))
	}
	return nil
}
