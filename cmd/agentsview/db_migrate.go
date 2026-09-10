package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBMigrateCommand() *cobra.Command {
	var images bool
	var project, before string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:          "migrate",
		Short:        "Move retained inline tool-result images into the asset store",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !images {
				return fmt.Errorf("db migrate requires --images")
			}
			jsonOutput := outputFormat(cmd) == "json"
			if jsonOutput && !yes && !dryRun {
				return fmt.Errorf("--format json requires --yes for db migrate --images")
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			filter := db.StripImagesFilter{Project: project, Before: before}
			if dryRun {
				report, err := previewDBMigrate(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				return writeDBMigrateReport(cmd.OutOrStdout(), report, jsonOutput, true)
			}
			if !yes {
				report, err := previewDBMigrate(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				if err := writeDBMigrateReport(cmd.ErrOrStderr(), report, false, true); err != nil {
					return err
				}
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Migrated images require a backup of the assets directory beside the archive.")
				if !confirm(cmd.InOrStdin(), cmd.ErrOrStderr(),
					fmt.Sprintf("Migrate images from %d sessions?", report.Sessions)) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			report, err := runDBMigrate(cmd.Context(), cfg, filter)
			if err != nil {
				if report.Sessions > 0 {
					writeDBMigratePartialReport(cmd.ErrOrStderr(), report)
				}
				return err
			}
			return writeDBMigrateReport(cmd.OutOrStdout(), report, jsonOutput, false)
		},
	}
	cmd.Flags().BoolVar(&images, "images", false, "Migrate inline image payloads into the asset store")
	cmd.Flags().StringVar(&project, "project", "", "Sessions whose project contains this substring")
	cmd.Flags().StringVar(&before, "before", "", "Sessions that ended before this date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report matching rows without changing the archive")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func previewDBMigrate(
	ctx context.Context, cfg config.Config, filter db.StripImagesFilter,
) (db.StripImagesReport, error) {
	database, err := openReadOnlyDB(cfg)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("opening archive for image migration preview: %w", err)
	}
	defer database.Close()
	report, err := database.PreviewMigrateToolImages(ctx, filter)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("previewing image migration: %w", err)
	}
	return report, nil
}

func runDBMigrate(
	ctx context.Context, cfg config.Config, filter db.StripImagesFilter,
) (db.StripImagesReport, error) {
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("opening archive for image migration: %w", err)
	}
	defer closeWriteDB(database, lock)
	assetsDir := filepath.Join(cfg.DataDir, "assets")
	put := func(mediaType string, body []byte) (string, bool, error) {
		return assets.Put(assetsDir, mediaType, body)
	}
	report, err := database.MigrateToolImages(ctx, filter, put)
	if err != nil {
		// Return the partial report so the caller can show committed work.
		return report, fmt.Errorf("migrating images: %w", err)
	}
	return report, nil
}

func writeDBMigrateReport(
	out io.Writer, report db.StripImagesReport, jsonOutput, preview bool,
) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(report)
	}
	if preview {
		fmt.Fprintln(out, "Image migration preview.")
	} else {
		fmt.Fprintln(out, "Image migration completed.")
	}
	writeDBMigrateCounts(out, report)
	if !preview {
		fmt.Fprintln(out, "Migrated images live in the assets directory beside the archive; back them up together.")
		fmt.Fprintln(out, "Run db compact separately to measure SQLite file-space reclamation.")
	}
	return nil
}

// writeDBMigratePartialReport describes the work a failed run committed before
// it stopped. The run neither completed nor left the archive as it found it, so
// the counts arrive under their own header and without the tips that only make
// sense once every selected session is done.
func writeDBMigratePartialReport(out io.Writer, report db.StripImagesReport) {
	fmt.Fprintln(out, "Image migration stopped early. Counts below cover the sessions that committed:")
	writeDBMigrateCounts(out, report)
}

func writeDBMigrateCounts(out io.Writer, report db.StripImagesReport) {
	fmt.Fprintf(out, "  Sessions: %d\n", report.Sessions)
	fmt.Fprintf(out, "  Changed: %d\n", report.Changed)
	fmt.Fprintf(out, "  Image payloads: %d\n", report.Payloads)
	fmt.Fprintf(out, "  Stored content bytes: %s\n", formatBytes(report.StoredBytes))
	fmt.Fprintf(out, "  Decoded image bytes: %s\n", formatBytes(report.DecodedBytes))
	if len(report.Projects) == 0 {
		return
	}
	fmt.Fprintln(out, "By project:")
	for _, project := range report.Projects {
		fmt.Fprintf(out,
			"  %s: %d sessions, %d changed, %d payloads, %s stored, %s decoded\n",
			project.Project, project.Sessions, project.Changed, project.Payloads,
			formatBytes(project.StoredBytes), formatBytes(project.DecodedBytes))
	}
}
