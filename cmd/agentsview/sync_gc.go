package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
)

const (
	defaultArtifactGCGrace      = 7 * 24 * time.Hour
	defaultArtifactGCMaxObjects = 1024
	defaultArtifactGCMaxBytes   = int64(256 << 20)
)

// SyncGCConfig holds parsed logical-retention and physical-maintenance limits.
type SyncGCConfig struct {
	Grace           time.Duration
	QuarantineGrace time.Duration
	DryRun          bool
	MaxObjects      int
	MaxBytes        int64
	TrashCursor     string
	GCCursor        string
	RepackCursor    string
}

type syncGCDependencies struct {
	findDaemon        func(string, ...string) *DaemonRuntime
	localDaemonActive func(string, ...string) bool
	openRepository    func(context.Context, string) (*artifact.Repository, error)
}

func newSyncGCCommand() *cobra.Command {
	cfg := SyncGCConfig{
		Grace:           defaultArtifactGCGrace,
		QuarantineGrace: defaultArtifactGCGrace,
		MaxObjects:      defaultArtifactGCMaxObjects,
		MaxBytes:        defaultArtifactGCMaxBytes,
	}
	cmd := &cobra.Command{
		Use:          "gc",
		Short:        "Retain logical artifacts and reclaim vault storage",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSyncGC(cmd, cfg)
		},
	}
	cmd.Flags().DurationVar(&cfg.Grace, "grace", defaultArtifactGCGrace,
		"Minimum age before unreachable logical artifacts enter trash")
	cmd.Flags().DurationVar(&cfg.QuarantineGrace, "quarantine-grace", defaultArtifactGCGrace,
		"Minimum diagnostic retention for quarantined artifacts")
	cmd.Flags().BoolVar(&cfg.DryRun, "dry-run", false,
		"Report logical artifacts without trashing or physical reclamation")
	cmd.Flags().IntVar(&cfg.MaxObjects, "max-objects", defaultArtifactGCMaxObjects,
		"Maximum objects processed by each physical maintenance stage")
	cmd.Flags().Int64Var(&cfg.MaxBytes, "max-bytes", defaultArtifactGCMaxBytes,
		"Soft byte budget for each physical maintenance stage")
	cmd.Flags().StringVar(&cfg.TrashCursor, "trash-cursor", "",
		"Resume physical trash emptying from this cursor")
	cmd.Flags().StringVar(&cfg.GCCursor, "gc-cursor", "",
		"Resume physical garbage collection from this cursor")
	cmd.Flags().StringVar(&cfg.RepackCursor, "repack-cursor", "",
		"Resume physical repacking from this cursor")
	return cmd
}

func runSyncGC(cmd *cobra.Command, cfg SyncGCConfig) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return runSyncGCWith(cmd, appCfg, cfg, syncGCDependencies{
		findDaemon:        FindDaemonRuntime,
		localDaemonActive: IsLocalDaemonActive,
		openRepository:    artifact.OpenRepository,
	})
}

func runSyncGCWith(
	cmd *cobra.Command, appCfg config.Config, cfg SyncGCConfig, deps syncGCDependencies,
) (retErr error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Grace < 0 || cfg.QuarantineGrace < 0 ||
		cfg.MaxObjects < 0 || cfg.MaxBytes < 0 {
		return errors.New("artifact maintenance limits must not be negative")
	}
	if deps.findDaemon == nil {
		deps.findDaemon = FindDaemonRuntime
	}
	if deps.localDaemonActive == nil {
		deps.localDaemonActive = IsLocalDaemonActive
	}
	if deps.openRepository == nil {
		deps.openRepository = artifact.OpenRepository
	}
	if runtime := deps.findDaemon(appCfg.DataDir, appCfg.AuthToken); runtime != nil {
		if runtime.ReadOnly {
			return errors.New("a read-only daemon owns artifact access; stop it before maintenance")
		}
		return runSyncGCDaemon(ctx, cmd, runtime, appCfg.AuthToken, cfg)
	}
	if deps.localDaemonActive(appCfg.DataDir, appCfg.AuthToken) {
		return errors.New("the writable daemon owns the artifact vault but is not responding; refusing direct maintenance")
	}
	repository, err := deps.openRepository(ctx, appCfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening artifact repository: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, repository.Close())
	}()
	response, err := runSyncGCDirect(ctx, repository.Store(), cfg)
	if err != nil {
		return err
	}
	printArtifactMaintenanceSummary(cmd.OutOrStdout(), response, cfg)
	return nil
}

type syncGCMaintenanceResponse struct {
	Logical  artifact.GCResult `json:"logical"`
	Physical struct {
		Supported bool                               `json:"supported"`
		Result    artifact.PhysicalMaintenanceResult `json:"result"`
	} `json:"physical"`
}

func runSyncGCDirect(
	ctx context.Context, store artifact.ArtifactStore, cfg SyncGCConfig,
) (syncGCMaintenanceResponse, error) {
	var response syncGCMaintenanceResponse
	maintenanceOpts := artifactMaintenanceOptions(cfg)
	if err := artifact.ValidateArtifactMaintenanceOptions(maintenanceOpts); err != nil {
		return response, fmt.Errorf("artifact physical maintenance: %w", err)
	}
	logical, err := artifact.GarbageCollect(ctx, artifact.GCOptions{
		Store: store, Grace: cfg.Grace, QuarantineGrace: cfg.QuarantineGrace,
		DryRun: cfg.DryRun,
	})
	if err != nil {
		return response, fmt.Errorf("artifact retention: %w", err)
	}
	response.Logical = logical
	maintainer, ok := store.(artifact.ArtifactMaintainer)
	if !ok || cfg.DryRun {
		return response, nil
	}
	response.Physical.Supported = true
	response.Physical.Result, err = artifact.RunPhysicalMaintenance(
		ctx, maintainer, maintenanceOpts)
	if err != nil {
		return response, fmt.Errorf("artifact physical maintenance: %w", err)
	}
	return response, nil
}

func runSyncGCDaemon(
	ctx context.Context,
	cmd *cobra.Command,
	runtime *DaemonRuntime,
	authToken string,
	cfg SyncGCConfig,
) error {
	endpoint, origin, err := daemonArtifactExchangeEndpoint(transportFromRuntime(runtime))
	if err != nil {
		return err
	}
	endpoint = strings.TrimSuffix(endpoint, "/exchange") + "/maintenance"
	body, err := json.Marshal(struct {
		Grace           string `json:"grace"`
		QuarantineGrace string `json:"quarantine_grace"`
		MaxObjects      int    `json:"max_objects"`
		MaxBytes        int64  `json:"max_bytes"`
		DryRun          bool   `json:"dry_run"`
		TrashCursor     string `json:"trash_cursor,omitempty"`
		GCCursor        string `json:"gc_cursor,omitempty"`
		RepackCursor    string `json:"repack_cursor,omitempty"`
	}{
		Grace: cfg.Grace.String(), QuarantineGrace: cfg.QuarantineGrace.String(),
		MaxObjects: cfg.MaxObjects, MaxBytes: cfg.MaxBytes, DryRun: cfg.DryRun,
		TrashCursor: cfg.TrashCursor, GCCursor: cfg.GCCursor, RepackCursor: cfg.RepackCursor,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("daemon artifact maintenance request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("daemon artifact maintenance returned HTTP %d", resp.StatusCode)
	}
	var response syncGCMaintenanceResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&response); err != nil {
		return errors.New("decoding daemon artifact maintenance response")
	}
	printArtifactMaintenanceSummary(cmd.OutOrStdout(), response, cfg)
	return nil
}

func artifactMaintenanceOptions(cfg SyncGCConfig) artifact.ArtifactMaintenanceOptions {
	return artifact.ArtifactMaintenanceOptions{
		TrashGrace: cfg.Grace,
		EmptyTrash: artifact.WorkBudget{MaxObjects: cfg.MaxObjects, Cursor: cfg.TrashCursor},
		GC: artifact.WorkBudget{
			MaxObjects: cfg.MaxObjects, MaxBytes: cfg.MaxBytes, Cursor: cfg.GCCursor,
		},
		Repack: artifact.WorkBudget{
			MaxObjects: cfg.MaxObjects, MaxBytes: cfg.MaxBytes, Cursor: cfg.RepackCursor,
		},
	}
}

func printArtifactMaintenanceSummary(
	w io.Writer, response syncGCMaintenanceResponse, cfg SyncGCConfig,
) {
	result := response.Logical
	action := "trashed"
	count := result.Deleted
	bytes := result.BytesDeleted
	if result.DryRun {
		action = "would trash"
		count = result.Eligible
		bytes = result.BytesEligible
	}
	fmt.Fprintf(w,
		"Artifact retention: scanned %d origin(s), skipped %d unsafe origin(s), %s %d artifact(s) (%s)\n",
		result.Origins, result.SkippedOrigins, action, count, formatBytes(bytes))
	if result.QuarantineSkipped {
		fmt.Fprintln(w, "Artifact quarantine retention: unsupported by this store")
	}
	if response.Physical.Supported {
		physical := response.Physical.Result
		if !physical.EmptyTrash.More && !physical.GarbageCollect.More && !physical.Repack.More {
			fmt.Fprintln(w, "Artifact physical maintenance complete")
			return
		}
		fmt.Fprintln(w, "Artifact physical maintenance: more work remains")
		fmt.Fprint(w, "  agentsview sync gc")
		printArtifactMaintenanceFlag(w, "grace", cfg.Grace.String())
		printArtifactMaintenanceFlag(w, "quarantine-grace", cfg.QuarantineGrace.String())
		printArtifactMaintenanceFlag(w, "max-objects", strconv.Itoa(cfg.MaxObjects))
		printArtifactMaintenanceFlag(w, "max-bytes", strconv.FormatInt(cfg.MaxBytes, 10))
		if cfg.DryRun {
			fmt.Fprint(w, " --dry-run")
		}
		printArtifactMaintenanceResume(w, "trash", physical.EmptyTrash)
		printArtifactMaintenanceResume(w, "gc", physical.GarbageCollect)
		printArtifactMaintenanceResume(w, "repack", physical.Repack)
		fmt.Fprintln(w)
	}
}

func printArtifactMaintenanceFlag(w io.Writer, name, value string) {
	fmt.Fprintf(w, " --%s %s", name, shellQuoteArtifactValue(value))
}

func printArtifactMaintenanceResume(w io.Writer, stage string, result artifact.MaintenanceResult) {
	if !result.More || result.NextCursor == "" {
		return
	}
	fmt.Fprintf(w, " --%s-cursor %s",
		stage, shellQuoteArtifactValue(result.NextCursor))
}

func shellQuoteArtifactValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
