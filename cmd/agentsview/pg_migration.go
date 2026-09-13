package main

import "github.com/spf13/cobra"

func newPGMigrationCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "migration",
		Short:        "Run bounded PostgreSQL migration checks",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newPGMigrationParityCommand())
	return cmd
}
