package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

type rawSyncServerStatusConfig struct {
	Server            string
	DeviceID          string
	AllowInsecureHTTP bool
	Health            *rawsync.JobHealthQuery
}

func newRawSyncServerStatusCommand() *cobra.Command {
	return newRawSyncServerReportCommand(false)
}

func newRawSyncServerHealthCommand() *cobra.Command { return newRawSyncServerReportCommand(true) }

func newRawSyncServerReportCommand(health bool) *cobra.Command {
	cfg := rawSyncServerStatusConfig{}
	name := "server-status"
	if health {
		name = "server-health"
		cfg.Health = &rawsync.JobHealthQuery{}
	}
	cmd := &cobra.Command{
		Use:   name,
		Short: "Show hosted raw-sync status",
		Long: "Show hosted raw-sync status. The device credential is read only from " +
			"AGENTSVIEW_RAW_SYNC_CREDENTIAL; it cannot be passed as an argument. " +
			"An HTTP 404 is an error; use 'raw-sync status' for local state.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runRawSyncServerStatus(
				cmd.Context(), cfg, cmd.OutOrStdout(),
			); err != nil {
				return fmt.Errorf("raw-sync %s: %w", name, err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.Server, "server", "", "Raw-sync server URL (or AGENTSVIEW_RAW_SYNC_URL)")
	cmd.Flags().StringVar(&cfg.DeviceID, "device-id", "", "Provisioned device ID (or AGENTSVIEW_RAW_SYNC_DEVICE_ID)")
	cmd.Flags().BoolVar(&cfg.AllowInsecureHTTP, "allow-insecure-http", false, "Allow HTTP only for a loopback raw-sync server")
	if cfg.Health != nil {
		cmd.Short = "Show hosted raw-sync health"
		cmd.Long = "Show hosted raw-sync health using a device status token and explicit thresholds."
		cmd.Flags().Int32Var(&cfg.Health.MaxAttempts, "max-attempts", 0, "Maximum job attempts")
		cmd.Flags().Int32Var(&cfg.Health.StaleAfterSeconds, "stale-after-seconds", 0, "Source acceptance age threshold in seconds")
		_ = cmd.MarkFlagRequired("max-attempts")
		_ = cmd.MarkFlagRequired("stale-after-seconds")
	}
	return cmd
}

func runRawSyncServerStatus(
	ctx context.Context,
	cfg rawSyncServerStatusConfig,
	out io.Writer,
) error {
	if cfg.Health != nil {
		if err := cfg.Health.Validate(); err != nil {
			return err
		}
	}
	cfg.Server = firstNonempty(cfg.Server, os.Getenv("AGENTSVIEW_RAW_SYNC_URL"))
	cfg.DeviceID = firstNonempty(
		cfg.DeviceID, os.Getenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID"),
	)
	credential := os.Getenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL")
	if err := validateRawSyncConnection(
		cfg.Server, cfg.DeviceID, credential, cfg.AllowInsecureHTTP,
	); err != nil {
		return err
	}
	client, err := rawclient.NewStatusClient(rawclient.Config{
		BaseURL: cfg.Server, DeviceID: cfg.DeviceID, Credential: credential,
	})
	if err != nil {
		return fmt.Errorf("creating server status client: %w", err)
	}
	kind := "status"
	if cfg.Health != nil {
		kind = "health"
	}
	var status rawsync.Status
	var report any
	if cfg.Health != nil {
		report, err = client.Health(ctx, *cfg.Health)
	} else {
		status, err = client.Status(ctx)
	}
	if err != nil {
		var apiErr rawclient.APIError
		if rawclient.AsAPIError(err, &apiErr) {
			// Keep server diagnostics without echoing the device credential.
			apiErr.Code = strings.ReplaceAll(apiErr.Code, credential, "[redacted]")
			apiErr.Message = strings.ReplaceAll(apiErr.Message, credential, "[redacted]")
			if apiErr.Status == http.StatusNotFound {
				return fmt.Errorf("server does not provide raw-sync %s (HTTP 404); use 'raw-sync status' for local state: %w", kind, &apiErr)
			}
			return fmt.Errorf("server %s request failed (HTTP %d): %w", kind, apiErr.Status, &apiErr)
		}
		return fmt.Errorf("server %s request failed: %w", kind, err)
	}
	if cfg.Health != nil {
		return writeRawSyncStatus(out, report)
	}
	return writeRawSyncStatus(out, rawSyncServerStatus{
		Status:                  status,
		PipelineDepth:           status.ParseJobs.Ready + status.ParseJobs.Leased + status.ParseJobs.Retrying,
		LastParseLatencySeconds: rawSyncLastParseLatencySeconds(status.SourceHeads),
	})
}

type rawSyncServerStatus struct {
	rawsync.Status          `json:",inline"`
	PipelineDepth           int64    `json:"pipeline_depth"`
	LastParseLatencySeconds *float64 `json:"last_parse_latency_seconds"`
}

func rawSyncLastParseLatencySeconds(heads []rawsync.SourceHeadStatus) *float64 {
	var latest *rawsync.SourceHeadStatus
	for i := range heads {
		head := &heads[i]
		if head.LastAcceptedAt == nil || head.LastParseCompletedAt == nil {
			continue
		}
		if latest == nil || head.LastParseCompletedAt.After(*latest.LastParseCompletedAt) {
			latest = head
		}
	}
	if latest == nil {
		return nil
	}
	return new(latest.LastParseCompletedAt.Sub(*latest.LastAcceptedAt).Seconds())
}
