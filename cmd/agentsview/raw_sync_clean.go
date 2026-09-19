package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

const rawSyncCleanUploadsCompletion = "Raw upload cleanup pass completed."

func newRawSyncCleanUploadsCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "clean-uploads",
		Short:        "Run one bounded server upload cleanup pass",
		Long:         "Run one bounded server upload cleanup pass against the configured PostgreSQL target and AgentsView data directory.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runRawSyncCleanUploads(); err != nil {
				return fmt.Errorf("raw-sync clean-uploads: %w", err)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), rawSyncCleanUploadsCompletion)
			return err
		},
	}
}

func runRawSyncCleanUploads() (err error) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		return fmt.Errorf("resolving PostgreSQL config: %w", err)
	}
	applyClassifierConfig(appCfg)
	database, err := postgres.Open(pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure)
	if err != nil {
		return fmt.Errorf("opening PostgreSQL: %w", err)
	}
	defer func() { err = errors.Join(err, database.Close()) }()
	err = postgres.CheckRawSyncWritePrivileges(
		context.Background(), database, pgCfg.Schema,
	)
	if err != nil {
		return fmt.Errorf("checking raw-sync schema: %w", err)
	}

	store, err := postgres.NewRawUploadStore(database, appCfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening raw upload store: %w", err)
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	return nil
}
