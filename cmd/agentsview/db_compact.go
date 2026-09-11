package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func newDBCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Short:   "Maintain the local archive",
		GroupID: groupData,
		Args:    cobra.NoArgs,
	}
	cmd.AddCommand(newDBCompactCommand())
	cmd.AddCommand(newDBStripCommand())
	cmd.AddCommand(newDBMigrateCommand())
	cmd.AddCommand(newDBAdoptMachineCommand())
	return cmd
}

func newDBCompactCommand() *cobra.Command {
	var options db.CompactOptions
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:          "compact",
		Short:        "Reclaim free SQLite pages and truncate the WAL",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			jsonOutput := outputFormat(cmd) == "json"
			if jsonOutput && !yes && !dryRun {
				return fmt.Errorf("--format json requires --yes for db compact")
			}
			cfg, err := config.LoadMinimal()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if dryRun {
				return runDBCompactDryRun(ctx, cfg, cmd.OutOrStdout(), jsonOutput)
			}
			if !yes {
				estimate, err := estimateDBCompact(ctx, cfg)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Estimated reclaimable space: %s (database pages %s, WAL %s).\n",
					formatBytes(estimate.EstimatedReclaimBytes),
					formatBytes(estimate.FreeListBytes),
					formatBytes(estimate.WALBytes),
				)
				if !confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), "Continue with staged archive compaction?") {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			result, err := runDBCompact(ctx, cfg, options)
			if err != nil {
				return err
			}
			return writeDBCompactResult(cmd.OutOrStdout(), result, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&options.StagingDir, "staging-dir", "",
		"Directory for the staged compact database and backup")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Only report the estimate; do not modify the archive")
	cmd.Flags().BoolVar(&options.KeepBackup, "keep-backup", false,
		"Keep the original database backup after installation")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"Skip the confirmation prompt")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func estimateDBCompact(ctx context.Context, cfg config.Config) (db.CompactEstimate, error) {
	database, err := openReadOnlyDB(cfg)
	if err != nil {
		return db.CompactEstimate{}, fmt.Errorf("opening archive for compaction estimate: %w", err)
	}
	defer database.Close()
	estimate, err := database.EstimateCompact(ctx)
	if err != nil {
		return db.CompactEstimate{}, fmt.Errorf("estimating archive compaction: %w", err)
	}
	return estimate, nil
}

func runDBCompactDryRun(
	ctx context.Context, cfg config.Config, out io.Writer, jsonOutput bool,
) error {
	estimate, err := estimateDBCompact(ctx, cfg)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(out).Encode(estimate)
	}
	fmt.Fprintln(out, "Archive compaction estimate.")
	fmt.Fprintf(out, "  Database: %s\n", formatBytes(estimate.DatabaseBytes))
	fmt.Fprintf(out, "  WAL:      %s\n", formatBytes(estimate.WALBytes))
	fmt.Fprintf(out, "  SHM:      %s\n", formatBytes(estimate.SHMBytes))
	fmt.Fprintf(out, "  Free pages: %s\n", formatBytes(estimate.FreeListBytes))
	fmt.Fprintf(out, "  Estimated compacted DB: %s\n", formatBytes(estimate.EstimatedDatabaseBytes))
	fmt.Fprintf(out, "  Estimated reclaim: %s\n", formatBytes(estimate.EstimatedReclaimBytes))
	fmt.Fprintf(out, "  Peak extra space on one filesystem: %s\n", formatBytes(estimate.StagingRequiredBytes))
	if estimate.EstimatedReclaimBytes == 0 {
		fmt.Fprintln(out, "  Live tool-result content remains stored; compaction will not compress it.")
	}
	return nil
}

func runDBCompact(
	ctx context.Context, cfg config.Config, options db.CompactOptions,
) (db.CompactResult, error) {
	// Probe for an existing daemon without auto-starting one: a daemon booted
	// only for maintenance would still run its deferred startup sync at
	// archive scale and then stay resident. With no daemon, the command owns
	// the archive directly through the write lock.
	tr, err := detectTransportContext(
		ctx, cfg.DataDir, cfg.AuthToken, backgroundAutoStartReadyTimeout,
	)
	if err != nil {
		return db.CompactResult{}, fmt.Errorf("resolving archive transport: %w", err)
	}
	delegate, err := decideCompactRoute(tr, options.StagingDir)
	if err != nil {
		return db.CompactResult{}, err
	}
	if delegate {
		return requestDBCompact(ctx, tr, cfg.AuthToken, options)
	}
	return runDBCompactDirect(ctx, cfg, options)
}

// decideCompactRoute picks between delegating to a writable SQLite daemon and
// compacting the archive directly. A read-only `pg serve` or `duckdb serve`
// runtime does not own the local SQLite archive, so it is treated like no
// daemon; the direct path's write-owner lock still refuses if a writable
// daemon actually holds the archive.
func decideCompactRoute(tr transport, stagingDir string) (delegate bool, err error) {
	if tr.Mode == transportHTTP && !tr.ReadOnly {
		if stagingDir != "" {
			return false, fmt.Errorf(
				"--staging-dir requires direct archive access; stop the daemon before using it",
			)
		}
		return true, nil
	}
	if tr.Mode == transportDirect && (tr.DirectReadOnly || tr.DirectIncompatible) {
		reason := tr.DirectReason
		if reason == "" {
			reason = "a local daemon owns the archive"
		}
		if stagingDir != "" {
			return false, fmt.Errorf(
				"--staging-dir requires direct archive access: %s; stop the daemon before using it",
				reason,
			)
		}
		return false, fmt.Errorf(
			"cannot compact directly: %s; use the daemon or stop it first", reason,
		)
	}
	return false, nil
}

func runDBCompactDirect(
	ctx context.Context, cfg config.Config, options db.CompactOptions,
) (db.CompactResult, error) {
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return db.CompactResult{}, fmt.Errorf("opening archive for compaction: %w", err)
	}
	defer closeWriteDB(database, lock)
	return database.Compact(ctx, options)
}

func requestDBCompact(
	ctx context.Context, tr transport, authToken string, options db.CompactOptions,
) (db.CompactResult, error) {
	body, err := json.Marshal(struct {
		KeepBackup bool `json:"keep_backup,omitempty"`
	}{KeepBackup: options.KeepBackup})
	if err != nil {
		return db.CompactResult{}, err
	}
	endpoint := strings.TrimSuffix(tr.URL, "/") + "/api/v1/data/compact"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return db.CompactResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Mutating API routes require a recognized Origin, including for
	// loopback callers. The CLI is a trusted local caller, so identify
	// the daemon origin explicitly instead of weakening the server's
	// CSRF protection.
	if parsed, parseErr := url.Parse(tr.URL); parseErr == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Path = ""
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		req.Header.Set("Origin", parsed.String())
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return db.CompactResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var api struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&api)
		if api.Error == "" {
			api.Error = resp.Status
		}
		return db.CompactResult{}, fmt.Errorf("archive compaction: %s", api.Error)
	}
	var result db.CompactResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return db.CompactResult{}, fmt.Errorf("decode archive compaction result: %w", err)
	}
	return result, nil
}

func writeDBCompactResult(out io.Writer, result db.CompactResult, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(result)
	}
	fmt.Fprintln(out, "Archive compaction completed.")
	fmt.Fprintln(out, "Before:")
	fmt.Fprintf(out, "  Database: %s\n", formatBytes(result.Before.DatabaseBytes))
	fmt.Fprintf(out, "  WAL:      %s\n", formatBytes(result.Before.WALBytes))
	fmt.Fprintf(out, "  SHM:      %s\n", formatBytes(result.Before.SHMBytes))
	fmt.Fprintf(out, "  Free pages: %s\n", formatBytes(result.Before.FreeListBytes))
	fmt.Fprintln(out, "After:")
	fmt.Fprintf(out, "  Database: %s\n", formatBytes(result.After.DatabaseBytes))
	fmt.Fprintf(out, "  WAL:      %s\n", formatBytes(result.After.WALBytes))
	fmt.Fprintf(out, "  SHM:      %s\n", formatBytes(result.After.SHMBytes))
	fmt.Fprintf(out, "Reclaimed: %s\n", formatBytes(result.ReclaimedBytes))
	fmt.Fprintf(out, "Elapsed:   %dms\n", result.DurationMillis)
	if result.ReclaimedBytes <= 0 {
		fmt.Fprintln(out, "Live tool-result content remains stored; compaction does not compress or deduplicate it.")
	}
	if result.BackupPath != "" {
		fmt.Fprintf(out, "Backup:    %s\n", result.BackupPath)
	}
	return nil
}
