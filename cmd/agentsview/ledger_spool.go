// ABOUTME: `ledger spool` subcommands: jilog/opsctl spool interop (emit,
// ABOUTME: ingest, status) over [[ledger.zones]] spool_path.
package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/ledger/spoolrun"
)

type ledgerSpoolFlags struct {
	zone, source, cursorDir string
}

func newLedgerSpoolCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "spool",
		Short:        "Exchange ledger segments through a jilog-compatible spool directory",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE:         func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newLedgerSpoolEmitCommand(), newLedgerSpoolIngestCommand(), newLedgerSpoolStatusCommand())
	return cmd
}

func addSpoolZoneFlag(cmd *cobra.Command, f *ledgerSpoolFlags) {
	cmd.Flags().StringVar(&f.zone, "zone", "", "Only this [[ledger.zones]] id (default: all zones)")
}

func addSpoolSourceFlags(cmd *cobra.Command, f *ledgerSpoolFlags) {
	cmd.Flags().StringVar(&f.source, "source", "", "Ledger source (default: [ledger] source or av-<installation_id>)")
	cmd.Flags().StringVar(&f.cursorDir, "cursor-dir", "", "Emit cursor directory (default: <data_dir>/ledger/spool-cursors)")
}

func spoolSourceAndCursor(cfg config.Config, f ledgerSpoolFlags) (string, string) {
	source := f.source
	if source == "" && (cfg.Ledger.Source != "" || cfg.InstallationID != "") {
		source = cfg.Ledger.EffectiveSource(cfg.InstallationID)
	}
	cursorDir := f.cursorDir
	if cursorDir == "" {
		cursorDir = spoolrun.DefaultCursorDir(cfg.DataDir)
	}
	return source, cursorDir
}

func newLedgerSpoolEmitCommand() *cobra.Command {
	var f ledgerSpoolFlags
	cmd := &cobra.Command{
		Use:          "emit",
		Short:        "Copy this host's sealed ledger segments into the spool's incoming/ directory",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			zones, err := spoolrun.ZonesFromConfig(cfg.Ledger, f.zone, cfg.DataDir)
			if err != nil {
				return err
			}
			database, err := openReadOnlyDB(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer database.Close()
			source, cursorDir := spoolSourceAndCursor(cfg, f)
			return spoolrun.EmitZones(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				zones, database, source, cursorDir)
		},
	}
	addSpoolZoneFlag(cmd, &f)
	addSpoolSourceFlags(cmd, &f)
	return cmd
}

func newLedgerSpoolIngestCommand() *cobra.Command {
	var f ledgerSpoolFlags
	cmd := &cobra.Command{
		Use:          "ingest",
		Short:        "Commit spooled segments into this archive (spool authority only)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			run, err := runLedgerSpoolIngest(cmd.Context(), cfg, f.zone)
			if err != nil {
				return err
			}
			return run.Render(cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addSpoolZoneFlag(cmd, &f)
	return cmd
}

func newLedgerSpoolStatusCommand() *cobra.Command {
	var f ledgerSpoolFlags
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show spool counts and emit cursors per zone",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			zones, err := spoolrun.ZonesFromConfig(cfg.Ledger, f.zone, cfg.DataDir)
			if err != nil {
				return err
			}
			source, cursorDir := spoolSourceAndCursor(cfg, f)
			return spoolrun.StatusZones(cmd.OutOrStdout(), zones, cursorDir, source)
		},
	}
	addSpoolZoneFlag(cmd, &f)
	addSpoolSourceFlags(cmd, &f)
	return cmd
}

// runLedgerSpoolIngest delegates to a writable daemon, else writes directly
// under the write-owner lock, mirroring runDBCompact (db_compact.go:125-146).
func runLedgerSpoolIngest(ctx context.Context, cfg config.Config, zone string) (spoolrun.LedgerSpoolIngestRun, error) {
	tr, err := detectTransportContext(ctx, cfg.DataDir, cfg.AuthToken, backgroundAutoStartReadyTimeout)
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("resolving archive transport: %w", err)
	}
	delegate, err := decideLedgerSpoolRoute(tr)
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, err
	}
	if delegate {
		return requestLedgerSpoolIngest(ctx, tr, cfg.AuthToken, zone)
	}
	zones, err := spoolrun.ZonesFromConfig(cfg.Ledger, zone, cfg.DataDir)
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, err
	}
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("opening archive for spool ingest: %w", err)
	}
	defer closeWriteDB(database, lock)
	return spoolrun.IngestZones(ctx, zones, database)
}

func decideLedgerSpoolRoute(tr transport) (bool, error) {
	if tr.Mode == transportHTTP && !tr.ReadOnly {
		return true, nil
	}
	if tr.Mode == transportDirect && (tr.DirectReadOnly || tr.DirectIncompatible) {
		reason := tr.DirectReason
		if reason == "" {
			reason = "a local daemon owns the archive"
		}
		return false, fmt.Errorf("cannot ingest directly: %s; use the daemon or stop it first", reason)
	}
	return false, nil
}

func requestLedgerSpoolIngest(
	ctx context.Context, tr transport, authToken, zone string,
) (spoolrun.LedgerSpoolIngestRun, error) {
	api, err := apiclient.NewHTTPClient(tr.URL, authToken, &http.Client{Timeout: 0})
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, err
	}
	body := &apiclient.LedgerSpoolIngestInputBody{}
	if zone != "" {
		body.Zone = new(zone)
	}
	response, err := api.PostAPIV1LedgerSpoolIngestWithResponse(ctx,
		&apiclient.PostAPIV1LedgerSpoolIngestRequestOptions{Body: body})
	if response == nil {
		return spoolrun.LedgerSpoolIngestRun{}, err
	}
	if code := response.HTTPResponse.StatusCode; code < http.StatusOK || code >= http.StatusMultipleChoices {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(response.Body, &apiErr)
		if apiErr.Error == "" {
			apiErr.Error = response.HTTPResponse.Status
		}
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("ledger spool ingest: %s", apiErr.Error)
	}
	if err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("decode ledger spool ingest result: %w", err)
	}
	if len(response.Body) == 0 {
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("decode ledger spool ingest result: %w", io.ErrUnexpectedEOF)
	}
	var run spoolrun.LedgerSpoolIngestRun
	if err := json.Unmarshal(response.Body, &run); err != nil {
		return spoolrun.LedgerSpoolIngestRun{}, fmt.Errorf("decode ledger spool ingest result: %w", err)
	}
	return run, nil
}
