package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/importer"
)

type ImportConfig struct {
	Type string
	Path string
}

func runImport(cfg ImportConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// Handle zip files.
	dir, cleanup, err := resolveImportSource(cfg.Path)
	if err != nil {
		log.Fatalf("Error: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	var stats importer.ImportStats

	switch cfg.Type {
	case "claude-ai":
		stats, err = runClaudeAIImport(ctx, database, dir)
	case "chatgpt":
		assetsDir := filepath.Join(appCfg.DataDir, "assets")
		stats, err = runChatGPTImport(
			ctx, database, dir, assetsDir,
		)
	default:
		log.Fatalf(
			"Unknown import type: %s (use claude-ai or chatgpt)",
			cfg.Type,
		)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr)
		log.Fatalf("Import failed: %v", err)
	}

	printImportSummary(stats)

	if stats.Errors > 0 {
		os.Exit(1)
	}
}

func runClaudeAIImport(
	ctx context.Context, database *db.DB, path string,
) (importer.ImportStats, error) {
	jsonPath := path
	info, err := os.Stat(path)
	if err != nil {
		return importer.ImportStats{}, err
	}
	if info.IsDir() {
		jsonPath = filepath.Join(path, "conversations.json")
	}

	f, err := os.Open(jsonPath)
	if err != nil {
		return importer.ImportStats{},
			fmt.Errorf("opening %s: %w", jsonPath, err)
	}
	defer f.Close()

	return importer.ImportClaudeAI(
		ctx, database, f, &importer.ImportCallbacks{
			OnProgress: func(s importer.ImportStats) {
				n := s.Imported + s.Updated + s.Skipped
				fmt.Fprintf(
					os.Stderr,
					"\r%d conversations processed...", n,
				)
			},
			OnIndexing: func() {
				fmt.Fprintf(
					os.Stderr,
					"\rRebuilding search index...   ",
				)
			},
		},
	)
}

func runChatGPTImport(
	ctx context.Context, database *db.DB,
	dir, assetsDir string,
) (importer.ImportStats, error) {
	return importer.ImportChatGPT(
		ctx, database, dir, assetsDir,
		&importer.ImportCallbacks{
			OnProgress: func(s importer.ImportStats) {
				n := s.Imported + s.Skipped
				fmt.Fprintf(
					os.Stderr,
					"\r%d conversations processed...", n,
				)
			},
			OnIndexing: func() {
				fmt.Fprintf(
					os.Stderr,
					"\rRebuilding search index...   ",
				)
			},
		},
	)
}

func printImportSummary(stats importer.ImportStats) {
	total := stats.Imported + stats.Updated + stats.Skipped
	fmt.Fprintf(os.Stderr, "\rDone: %d processed", total)
	var parts []string
	if stats.Imported > 0 {
		parts = append(
			parts, fmt.Sprintf("%d new", stats.Imported),
		)
	}
	if stats.Updated > 0 {
		parts = append(
			parts, fmt.Sprintf("%d updated", stats.Updated),
		)
	}
	if stats.Skipped > 0 {
		parts = append(
			parts, fmt.Sprintf("%d skipped", stats.Skipped),
		)
	}
	if len(parts) > 0 {
		fmt.Fprintf(
			os.Stderr, " (%s)", strings.Join(parts, ", "),
		)
	}
	fmt.Fprintln(os.Stderr)
	if stats.Errors > 0 {
		fmt.Fprintf(os.Stderr, "  %d errors\n", stats.Errors)
	}
}

// resolveImportSource handles zip extraction. If the path is
// a .zip file, it extracts to a temp dir and returns the dir
// path with a cleanup function. Otherwise returns the original
// path with nil cleanup.
func resolveImportSource(
	path string,
) (string, func(), error) {
	if strings.HasSuffix(strings.ToLower(path), ".zip") {
		return importer.ExtractZip(path)
	}
	if _, err := os.Stat(path); err != nil {
		return "", nil,
			fmt.Errorf("cannot access %s: %w", path, err)
	}
	return path, nil, nil
}
