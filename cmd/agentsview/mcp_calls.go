package main

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
)

func newMCPCallsCommand() *cobra.Command {
	var since, until string
	cmd := &cobra.Command{
		Use: "calls", Short: "Summarize recorded MCP calls", Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, to, err := resolveUsageWindow(since, until, time.Now(), time.UTC)
			if err != nil {
				return err
			}
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			rows, err := mcpserver.SummarizeCalls(cmd.Context(), filepath.Join(cfg.DataDir, "mcp", "calls"), from, to)
			if err != nil {
				return err
			}
			if outputFormat(cmd) == "json" {
				return json.MarshalWrite(cmd.OutOrStdout(), rows)
			}
			if len(rows) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No MCP calls recorded in this range.")
				return err
			}
			for _, row := range rows {
				top, empty := "-", "-"
				if row.TopError != nil {
					top = fmt.Sprintf("%s (%d)", row.TopError.Name, row.TopError.Count)
				}
				if row.EmptyPercent != nil {
					empty = fmt.Sprintf("%.1f%%", *row.EmptyPercent)
				}
				var rejected []string
				for _, item := range row.Rejected {
					rejected = append(rejected, fmt.Sprintf("%s (%d)", item.Name, item.Count))
				}
				if len(rejected) == 0 {
					rejected = []string{"-"}
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %d calls, %.1f%% errors, %.1f%% truncated, top error: %s, empty: %s, rejected: %s, median: %d bytes, %d ms\n", row.Tool, row.Calls, row.ErrorPercent, row.TruncatedPercent, top, empty, strings.Join(rejected, ", "), row.MedianBytes, row.MedianMs); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "30d", "Start date or duration (UTC)")
	cmd.Flags().StringVar(&until, "until", "", "End date or duration (UTC)")
	registerFormatFlags(cmd.Flags())
	return cmd
}
