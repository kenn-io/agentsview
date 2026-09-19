package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
)

type rawSyncServerStatusConfig struct {
	Server            string
	DeviceID          string
	AllowInsecureHTTP bool
}

func newRawSyncServerStatusCommand() *cobra.Command {
	cfg := rawSyncServerStatusConfig{}
	cmd := &cobra.Command{
		Use:   "server-status",
		Short: "Show hosted raw-sync status",
		Long: "Show hosted raw-sync status. The device credential is read only from " +
			"AGENTSVIEW_RAW_SYNC_CREDENTIAL; it cannot be passed as an argument. " +
			"An HTTP 404 prints local raw-sync status output instead.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runRawSyncServerStatus(
				cmd.Context(), cfg, cmd.OutOrStdout(),
			); err != nil {
				return fmt.Errorf("raw-sync server-status: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.Server, "server", "", "Raw-sync server URL (or AGENTSVIEW_RAW_SYNC_URL)")
	cmd.Flags().StringVar(&cfg.DeviceID, "device-id", "", "Provisioned device ID (or AGENTSVIEW_RAW_SYNC_DEVICE_ID)")
	cmd.Flags().BoolVar(&cfg.AllowInsecureHTTP, "allow-insecure-http", false, "Allow HTTP only for a loopback raw-sync server")
	return cmd
}

func runRawSyncServerStatus(
	ctx context.Context,
	cfg rawSyncServerStatusConfig,
	out io.Writer,
) error {
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
		return errors.New("server status request failed")
	}
	status, err := client.Status(ctx)
	if err != nil {
		var apiErr rawclient.APIError
		if rawclient.AsAPIError(err, &apiErr) {
			if apiErr.Status == http.StatusNotFound {
				return runRawSyncStatus(ctx, out)
			}
			return fmt.Errorf("server request failed: HTTP %d", apiErr.Status)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("server status request failed")
	}
	return writeRawSyncStatus(out, rawSyncServerStatus{
		Status:          status,
		PipelineDepth:   status.ParseJobs.Ready + status.ParseJobs.Leased + status.ParseJobs.Retrying,
		ParseLagSeconds: rawSyncParseLagSeconds(status.SourceHeads),
	})
}

type rawSyncServerStatus struct {
	rawsync.Status  `json:",inline"`
	PipelineDepth   int64    `json:"pipeline_depth"`
	ParseLagSeconds *float64 `json:"parse_lag_seconds"`
}

func rawSyncParseLagSeconds(heads []rawsync.SourceHeadStatus) *float64 {
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
