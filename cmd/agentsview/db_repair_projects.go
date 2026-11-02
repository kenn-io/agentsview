package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBRepairProjectsCommand() *cobra.Command {
	var project string
	var apply bool
	cmd := &cobra.Command{
		Use: "repair-projects", Short: "Preview project corrections from recorded repositories",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadReadOnlyFor(config.ScopeLocal)
			if err != nil {
				return err
			}
			var report db.RepositoryProjectRepair
			if apply {
				database, lock, err := openWriteDBWith(cmd.Context(), cfg, func(ctx context.Context, cfg config.Config) (*db.DB, error) {
					applyClassifierConfig(cfg)
					return db.OpenForRepositoryProjectRepair(ctx, cfg.DBPath, cfg.ArchiveContent)
				})
				if err != nil {
					return err
				}
				defer closeWriteDB(database, lock)
				report, err = database.RepairRepositoryProjects(cmd.Context(), project)
				if err != nil {
					return err
				}
			} else {
				database, err := openReadOnlyDB(cmd.Context(), cfg)
				if err != nil {
					return err
				}
				defer database.Close()
				report, err = database.PreviewRepositoryProjects(cmd.Context(), project)
				if err != nil {
					return err
				}
			}
			return writeRepositoryProjectRepair(cmd.OutOrStdout(), report, outputFormat(cmd) == "json", apply)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Only this exact current project label")
	cmd.Flags().BoolVar(&apply, "apply", false, "Apply corrections (requires exclusive access to the archive)")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func writeRepositoryProjectRepair(w io.Writer, report db.RepositoryProjectRepair, jsonOutput, applied bool) error {
	if jsonOutput {
		if err := json.MarshalWrite(w, report, jsontext.WithIndent("  ")); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w)
		return err
	}
	verb := "Proposed"
	if applied {
		verb = "Updated"
	}
	if _, err := fmt.Fprintf(w, "%s: %d sessions. Ambiguous: %d sessions.\n", verb, report.Sessions, report.AmbiguousSessions); err != nil {
		return err
	}
	for _, change := range report.Changes {
		if _, err := fmt.Fprintf(w, "%s -> %s: %d sessions (%s; %s)\n", change.From, change.To, change.Sessions, change.Repository, change.Reason); err != nil {
			return err
		}
	}
	if !applied {
		_, err := fmt.Fprintln(w, "Preview only. Use --apply to save these corrections.")
		return err
	}
	return nil
}
