package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/xaiusage"
)

func newUsageGrokCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "grok <csv>",
		Short:        "Import an xAI account usage CSV",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := os.Open(args[0])
			if err != nil {
				return err
			}
			events, parseErr := xaiusage.ReadCSV(file)
			closeErr := file.Close()
			if parseErr != nil {
				return parseErr
			}
			if closeErr != nil {
				return closeErr
			}
			cfg, err := config.LoadMinimalFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			database, lock, err := openWriteDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer closeWriteDB(database, lock)
			if err := database.InsertCursorUsageEvents(cmd.Context(), events); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Processed %d xAI billing items; existing billing IDs were skipped\n", len(events))
			return nil
		},
	}
}
