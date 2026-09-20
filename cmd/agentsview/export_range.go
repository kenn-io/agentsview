package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/config"
)

func newExportRangeCommand(deps exportReportingDeps) *cobra.Command {
	return &cobra.Command{
		Use:          "range",
		Short:        "Discover the available UTC reporting date range",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			appConfig, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			database, err := openReadOnlyDB(cmd.Context(), appConfig)
			if err != nil {
				return fmt.Errorf("open local archive: %w", err)
			}
			defer database.Close()
			document, err := database.ExportReportingRange(cmd.Context(), deps.now())
			if err != nil {
				return err
			}
			return writeCanonicalReportingDocument(cmd, document)
		},
	}
}
