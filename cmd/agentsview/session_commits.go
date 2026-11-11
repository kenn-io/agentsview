package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newSessionCommitsCommand() *cobra.Command {
	return &cobra.Command{
		Use: "commits <id>", Short: "List commits made during a session", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			list, err := svc.SessionCommits(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			// Retained links also resolve an exact ID after its session is deleted.
			if len(list.Commits) == 0 {
				id, err := resolveServiceSessionID(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
				list, err = svc.SessionCommits(cmd.Context(), id)
				if err != nil {
					return err
				}
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), list)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SESSION\tORDINAL\tTIME\tSOURCE\tSHA\tBRANCH\tSUBJECT\tUSER TURN")
			for _, l := range list.Commits {
				source := l.Source
				if source == "trailer" {
					source = "commit message"
				}
				subject := l.Subject
				if l.Missing {
					subject += " (missing)"
				}
				turn := ""
				if l.Turn != nil {
					turn = fmt.Sprintf("%d: %s", l.Turn.Ordinal, truncCell(sanitizeTerminal(l.Turn.Text), 100))
					if l.Turn.Heuristic {
						turn += " (estimated)"
					}
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", sanitizeTerminal(l.SessionID), l.MessageOrdinal, sanitizeTerminal(l.CommittedAt), sanitizeTerminal(source), sanitizeTerminal(l.SHA), sanitizeTerminal(l.Branch), sanitizeTerminal(subject), turn)
			}
			return tw.Flush()
		},
	}
}
