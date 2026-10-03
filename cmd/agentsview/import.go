package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
	"go.kenn.io/agentsview/internal/pathutil"
)

type ImportConfig struct {
	Type    string
	Path    string
	Replace []string
}

func runImport(cfg ImportConfig) {
	if err := importSessions(cfg); err != nil {
		log.Fatal(err)
	}
}

func importSessions(cfg ImportConfig) error {
	if cfg.Type == "gemini-apps" && len(cfg.Replace) > 0 {
		return errors.New("--replace is not supported for gemini-apps imports")
	}
	expandedPath, err := pathutil.ExpandHome(cfg.Path)
	if err != nil {
		return fmt.Errorf("expanding import path: %w", err)
	}
	cfg.Path = expandedPath

	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	database, writeLock, err := openWriteDB(context.Background(), appCfg)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer closeWriteDB(database, writeLock)

	ctx := context.Background()

	// Handle zip files.
	dir, cleanup, err := resolveImportSource(cfg.Path)
	if err != nil {
		return fmt.Errorf("import source: %w", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	assetsDir := filepath.Join(appCfg.DataDir, "assets")
	stats, err := runImportDispatch(
		ctx, database, cfg.Type, dir, assetsDir, appCfg.InstallationID, cfg.Replace...,
	)
	if errors.Is(err, errUnknownImportType) {
		return fmt.Errorf("%w", err)
	}

	if err != nil {
		if summary := formatImportFailureSummary(stats); summary != "" {
			fmt.Fprint(os.Stderr, summary)
		} else {
			fmt.Fprintln(os.Stderr)
		}
		return fmt.Errorf("import failed: %w", err)
	}

	printImportSummary(stats)

	if stats.Errors > 0 {
		return fmt.Errorf("import completed with %d errors", stats.Errors)
	}
	return nil
}

var errUnknownImportType = errors.New("unknown import type")

func runImportDispatch(
	ctx context.Context,
	database *db.DB,
	importType, path, assetsDir, machine string,
	replace ...string,
) (importer.ImportStats, error) {
	switch importType {
	case "claude-ai":
		return runClaudeAIImport(ctx, database, path, machine, replace)
	case "chatgpt":
		return runChatGPTImport(ctx, database, path, assetsDir, machine, replace)
	case "gemini-apps":
		return runGeminiAppsImport(ctx, database, path, machine)
	default:
		return importer.ImportStats{}, fmt.Errorf(
			"%w: %s (use claude-ai, chatgpt, or gemini-apps)",
			errUnknownImportType, importType,
		)
	}
}

func runClaudeAIImport(
	ctx context.Context, database *db.DB, path, machine string, replace []string,
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

	return importer.ImportClaudeAIWithOptions(
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
		}, importer.ImportOptions{Replace: replace}, machine,
	)
}

func runChatGPTImport(
	ctx context.Context, database *db.DB,
	dir, assetsDir, machine string, replace []string,
) (importer.ImportStats, error) {
	return importer.ImportChatGPTWithOptions(
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
		}, importer.ImportOptions{Replace: replace}, machine,
	)
}

func runGeminiAppsImport(
	ctx context.Context, database *db.DB, path, machine string,
) (importer.ImportStats, error) {
	return importer.ImportGeminiApps(
		ctx, database, path,
		&importer.ImportCallbacks{
			OnProgress: func(s importer.ImportStats) {
				n := s.Imported + s.Updated + s.Skipped
				fmt.Fprintf(os.Stderr, "\r%d records processed...", n)
			},
			OnIndexing: func() {
				fmt.Fprintf(os.Stderr, "\rRebuilding search index...   ")
			},
		}, machine,
	)
}

func printImportSummary(stats importer.ImportStats) {
	fmt.Fprint(os.Stderr, formatImportSummary(stats))
}

func formatImportSummary(stats importer.ImportStats) string {
	var summary strings.Builder
	total := stats.Imported + stats.Updated + stats.Skipped
	fmt.Fprintf(&summary, "\rDone: %d processed", total)
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
		fmt.Fprintf(&summary, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintln(&summary)
	if stats.Errors > 0 {
		fmt.Fprintf(&summary, "  %d errors%s\n", stats.Errors, refusalBreakdown(stats.Refusals))
	}
	return summary.String()
}

func formatImportFailureSummary(stats importer.ImportStats) string {
	if stats.Imported+stats.Updated+stats.Skipped+stats.Errors == 0 {
		return ""
	}
	return formatImportSummary(stats)
}

// refusalBreakdown renders refused conversations grouped by reason, e.g. " (2 diverged, 1 transient)".
func refusalBreakdown(refusals []importer.ImportRefusal) string {
	if len(refusals) == 0 {
		return ""
	}
	counts := make(map[importer.RefusalReason]int)
	for _, r := range refusals {
		counts[r.Reason]++
	}
	parts := make([]string, 0, len(counts))
	for _, reason := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[reason], reason))
	}
	return " (" + strings.Join(parts, ", ") + ")"
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
