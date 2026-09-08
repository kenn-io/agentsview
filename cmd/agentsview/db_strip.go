package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBStripCommand() *cobra.Command {
	var images bool
	var project, before string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:          "strip",
		Short:        "Remove retained inline tool-result images",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !images {
				return fmt.Errorf("db strip requires --images")
			}
			jsonOutput := outputFormat(cmd) == "json"
			if jsonOutput && !yes && !dryRun {
				return fmt.Errorf("--format json requires --yes for db strip --images")
			}
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			filter := db.StripImagesFilter{Project: project, Before: before}
			if dryRun {
				report, err := previewDBStrip(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				return writeDBStripReport(cmd.OutOrStdout(), report, jsonOutput, true)
			}
			if !yes {
				report, err := previewDBStrip(cmd.Context(), cfg, filter)
				if err != nil {
					return err
				}
				if err := writeDBStripReport(cmd.ErrOrStderr(), report, false, true); err != nil {
					return err
				}
				if !confirm(cmd.InOrStdin(), cmd.ErrOrStderr(),
					fmt.Sprintf("Strip images from %d sessions?", report.Sessions)) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			report, err := runDBStrip(cmd.Context(), cfg, filter)
			if err != nil {
				return err
			}
			return writeDBStripReport(cmd.OutOrStdout(), report, jsonOutput, false)
		},
	}
	cmd.Flags().BoolVar(&images, "images", false, "Strip inline image payloads")
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
		return db.StripImagesReport{}, fmt.Errorf("stripping images: %w", err)
	}
	return report, nil
}

func writeDBStripReport(
	out io.Writer, report db.StripImagesReport, jsonOutput, preview bool,
) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(report)
	}
	if preview {
		fmt.Fprintln(out, "Image strip preview.")
	} else {
		fmt.Fprintln(out, "Image strip completed.")
	}
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
	if !preview {
		fmt.Fprintln(out, "Run db compact separately to measure file-space reclamation.")
	}
	return nil
}
