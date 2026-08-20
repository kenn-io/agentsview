// ABOUTME: `session tool-calls <id>` subcommand — flattens every
// ABOUTME: tool call in a session into a JSON list or tab-aligned
// ABOUTME: human table.
package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionToolCallsCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "tool-calls <id>",
		Short:        "List tool calls made during a session",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			id, err := resolveServiceSessionID(cmd.Context(), svc, args[0])
			if err != nil {
				return err
			}
			list, err := svc.ToolCalls(cmd.Context(), id)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), list)
			}
			return printToolCallsHuman(cmd.OutOrStdout(), list)
		},
	}
}

// printToolCallsHuman writes a tabwriter-aligned table. Timestamp
// is trimmed to 19 chars (YYYY-MM-DDTHH:MM:SS). Session-derived
// fields are sanitized for terminal safety.
func printToolCallsHuman(w io.Writer, list *service.ToolCallList) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ORDINAL\tTIMESTAMP\tTOOL\tCATEGORY")
	for _, tc := range list.ToolCalls {
		ts := tc.Timestamp
		if len(ts) >= 19 {
			ts = ts[:19]
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			tc.Ordinal,
			sanitizeTerminal(ts),
			sanitizeTerminal(tc.ToolName),
			sanitizeTerminal(tc.Category))
	}
	return tw.Flush()
}
