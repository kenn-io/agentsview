package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBMigrateCommand() *cobra.Command {
	return newDBImageCommand(
		"Migrate", "migration", "Move retained inline tool-result images into the asset store",
		"Migrate inline image payloads into the asset store", previewDBMigrate, runDBMigrate,
	)
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
