package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/service"
)

func newSessionCompactionsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "compactions [<id>]", Short: "Show compaction summaries", Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			since, _ := cmd.Flags().GetString("active-since")
			if (len(args) == 0) == (since == "") {
				return cliUsageError(errors.New("provide exactly one session ID or --active-since"), "")
			}
			limit, _ := cmd.Flags().GetInt("limit")
			if limit < 0 {
				return cliUsageError(errors.New("--limit must be 0 or more"), "")
			}
			cursor, _ := cmd.Flags().GetString("cursor")
			if len(args) == 1 && cursor != "" {
				return cliUsageError(errors.New("--cursor requires --active-since"), "omit --cursor or use --active-since")
			}
			svc, cleanup, err := resolveService(cmd)
			if err != nil {
				return err
			}
			defer cleanup()
			req := service.CompactionSummariesRequest{ActiveSince: since, Limit: limit, Cursor: cursor}
			if len(args) == 1 {
				req.SessionID, err = resolveServiceSessionID(cmd.Context(), svc, args[0])
				if err != nil {
					return err
				}
			}
			result, err := service.CompactionSummaries(cmd.Context(), svc, req)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result)
			}
			for _, summary := range result.Summaries {
				fmt.Fprintf(cmd.OutOrStdout(), "--- %s  #%d  %s ---\n%s\n\n", sanitizeTerminal(summary.SessionID), summary.Ordinal, sanitizeTerminal(summary.Timestamp), sanitizeTerminal(summary.Content))
			}
			if result.NextCursor != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "More results: --cursor %s\n", sanitizeTerminal(result.NextCursor))
			}
			return nil
		},
	}
	cmd.Flags().String("active-since", "", "Sessions active since this RFC3339 timestamp")
	cmd.Flags().Int("limit", service.DefaultCompactionSessionLimit, fmt.Sprintf("Sessions per page, 0 or more (max %d)", service.MaxCompactionSessionLimit))
	cmd.Flags().String("cursor", "", "Resume value; use the same --active-since")
	return cmd
}
