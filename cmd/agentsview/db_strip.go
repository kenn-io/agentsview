package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBStripCommand() *cobra.Command {
	return newDBImageCommand(
		"Strip", "strip", "Remove retained inline tool-result images",
		"Strip inline image payloads", previewDBStrip, runDBStrip,
	)
}

func newDBImageCommand(
	verb, operation, short, imagesHelp string,
	preview, apply func(context.Context, config.Config, db.StripImagesFilter) (db.StripImagesReport, error),
) *cobra.Command {
	name := strings.ToLower(verb)
	var images bool
	var project, before string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:          name,
		Short:        short,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !images {
				return fmt.Errorf("db %s requires --images", name)
			}
			jsonOutput := outputFormat(cmd) == "json"
			if jsonOutput && !yes && !dryRun {
				return fmt.Errorf("--format json requires --yes for db %s --images", name)
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			filter := db.StripImagesFilter{Project: project, Before: before}
			if dryRun {
				report, err := preview(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				return writeDBImageReport(cmd.OutOrStdout(), report, jsonOutput, "Image "+operation+" preview.")
			}
			if !yes {
				report, err := preview(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				if err := writeDBImageReport(cmd.ErrOrStderr(), report, false, "Image "+operation+" preview."); err != nil {
					return err
				}
				if name == "migrate" {
					fmt.Fprintln(cmd.ErrOrStderr(),
						"Migrated images require a backup of the assets directory beside the archive.")
				}
				if !confirm(cmd.InOrStdin(), cmd.ErrOrStderr(),
					fmt.Sprintf("%s images from %d sessions?", verb, report.Sessions)) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			report, err := apply(cmd.Context(), cfg, filter)
			if err != nil {
				if report.Sessions > 0 {
					_ = writeDBImageReport(cmd.ErrOrStderr(), report, false,
						"Image "+operation+" stopped early. Counts below cover the sessions that committed:")
				}
				return err
			}
			out := cmd.OutOrStdout()
			if err := writeDBImageReport(out, report, jsonOutput, "Image "+operation+" completed."); err != nil {
				return err
			}
			if !jsonOutput {
				if name == "migrate" {
					fmt.Fprintln(out, "Migrated images live in the assets directory beside the archive; back them up together.")
					fmt.Fprintln(out, "Run db compact separately to measure SQLite file-space reclamation.")
				} else {
					fmt.Fprintln(out, "Run db compact separately to measure file-space reclamation.")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&images, "images", false, imagesHelp)
	cmd.Flags().StringVar(&project, "project", "", "Sessions whose project contains this substring")
	cmd.Flags().StringVar(&before, "before", "", "Sessions that ended before this date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report matching rows without changing the archive")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func previewDBStrip(
	ctx context.Context, cfg config.Config, filter db.StripImagesFilter,
) (db.StripImagesReport, error) {
	database, err := openReadOnlyDB(cfg)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("opening archive for image strip preview: %w", err)
	}
	defer database.Close()
	report, err := database.PreviewStripToolImages(ctx, filter)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("previewing image strip: %w", err)
	}
	return report, nil
}

func runDBStrip(
	ctx context.Context, cfg config.Config, filter db.StripImagesFilter,
) (db.StripImagesReport, error) {
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return db.StripImagesReport{}, fmt.Errorf("opening archive for image strip: %w", err)
	}
	defer closeWriteDB(database, lock)
	report, err := database.StripToolImages(ctx, filter)
	if err != nil {
		return report, fmt.Errorf("stripping images: %w", err)
	}
	return report, nil
}

func writeDBImageReport(
	out io.Writer, report db.StripImagesReport, jsonOutput bool, heading string,
) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(report)
	}
	fmt.Fprintln(out, heading)
	fmt.Fprintf(out, "  Sessions: %d\n", report.Sessions)
	fmt.Fprintf(out, "  Changed: %d\n", report.Changed)
	fmt.Fprintf(out, "  Image payloads: %d\n", report.Payloads)
	fmt.Fprintf(out, "  Stored content bytes: %s\n", formatBytes(report.StoredBytes))
	fmt.Fprintf(out, "  Decoded image bytes: %s\n", formatBytes(report.DecodedBytes))
	if len(report.Projects) > 0 {
		fmt.Fprintln(out, "By project:")
		for _, project := range report.Projects {
			fmt.Fprintf(out,
				"  %s: %d sessions, %d changed, %d payloads, %s stored, %s decoded\n",
				project.Project, project.Sessions, project.Changed, project.Payloads,
				formatBytes(project.StoredBytes), formatBytes(project.DecodedBytes))
		}
	}
	return nil
}
