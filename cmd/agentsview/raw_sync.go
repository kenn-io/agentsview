package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawupload"
	"go.kenn.io/agentsview/internal/rawwatch"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

const (
	defaultRawSyncDebounce   = 2 * time.Second
	defaultRawSyncAudit      = 15 * time.Minute
	defaultRawSyncRetry      = time.Minute
	defaultRawSyncAuditLimit = 128
)

type rawSyncWatchConfig struct {
	Server            string
	DeviceID          string
	AllowInsecureHTTP bool
	Debounce          time.Duration
	Interval          time.Duration
	AuditLimit        int
}

type rawSyncRootRegistrar interface {
	RegisterRoots([]syncpkg.WatchRoot, int) []syncpkg.RecursiveWatchResult
}

type rawSyncLoopWorker interface {
	AuditAll(context.Context) error
	Drain(context.Context) error
}

func newRawSyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "raw-sync",
		Short:        "Capture and upload provider-owned raw session sources",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRawSyncWatchCommand())
	cmd.AddCommand(newRawSyncStatusCommand())
	return cmd
}

func newRawSyncWatchCommand() *cobra.Command {
	cfg := rawSyncWatchConfig{}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Continuously capture and upload raw session sources",
		Long: "Continuously capture and upload raw session sources.\n\n" +
			"The device credential is read only from " +
			"AGENTSVIEW_RAW_SYNC_CREDENTIAL; it cannot be passed as an argument.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(
				cmd.Context(), os.Interrupt, syscall.SIGTERM,
			)
			defer stop()
			if err := runRawSyncWatch(ctx, cfg); err != nil {
				return fmt.Errorf("raw-sync watch: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfg.Server, "server", "", "Raw-sync server URL (or AGENTSVIEW_RAW_SYNC_URL)")
	cmd.Flags().StringVar(&cfg.DeviceID, "device-id", "", "Provisioned device ID (or AGENTSVIEW_RAW_SYNC_DEVICE_ID)")
	cmd.Flags().BoolVar(&cfg.AllowInsecureHTTP, "allow-insecure-http", false, "Allow HTTP only for a loopback raw-sync server")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultRawSyncDebounce, "Coalesce window for filesystem changes")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultRawSyncAudit, "Bounded full-source audit interval")
	cmd.Flags().IntVar(&cfg.AuditLimit, "audit-limit", defaultRawSyncAuditLimit, "Maximum source work per provider audit")
	return cmd
}

func newRawSyncStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show durable laptop raw-sync status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runRawSyncStatus(cmd.Context(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("raw-sync status: %w", err)
			}
			return nil
		},
	}
}

func runRawSyncWatch(ctx context.Context, watchCfg rawSyncWatchConfig) error {
	watchCfg.Server = firstNonempty(watchCfg.Server, os.Getenv("AGENTSVIEW_RAW_SYNC_URL"))
	watchCfg.DeviceID = firstNonempty(
		watchCfg.DeviceID, os.Getenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID"),
	)
	credential := os.Getenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL")
	if err := validateRawSyncWatchConfig(watchCfg, credential); err != nil {
		return err
	}
	appCfg, err := config.LoadReadOnly()
	if err != nil {
		return err
	}
	providers, roots, err := rawSyncProvidersAndRoots(ctx, appCfg)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		return errors.New("no configured provider supports raw capture")
	}
	checkpointPath := rawSyncCheckpointPath(appCfg.DataDir)
	store, err := rawcheckpoint.Open(ctx, checkpointPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.EnsureDevice(ctx, watchCfg.DeviceID); err != nil {
		return err
	}
	client, err := rawclient.NewClient(rawclient.Config{
		BaseURL: watchCfg.Server, DeviceID: watchCfg.DeviceID, Credential: credential,
	})
	if err != nil {
		return err
	}
	capturer := rawcapture.New(store)
	worker := rawwatch.NewWorker(
		providers,
		capturer,
		rawwatch.NewAuditor(store, capturer, watchCfg.AuditLimit),
		rawupload.New(store, client, watchCfg.DeviceID),
	)
	watcher, err := syncpkg.NewWatcherWithCallback(
		watchCfg.Debounce,
		watchCfg.Debounce,
		rawSyncWatchCallback(worker.HandleBatch),
		appCfg.WatchExcludePatterns,
		syncpkg.WatcherOptions{},
	)
	if err != nil {
		return err
	}
	defer watcher.Stop()
	pendingRoots, uncovered := registerRawSyncRoots(watcher, roots)
	if uncovered != 0 {
		log.Printf("warning: raw-sync watcher has %d uncovered roots; registration retry and periodic audit remain active", uncovered)
	}
	if err := watcher.StartCollecting(); err != nil {
		return err
	}
	if err := worker.AuditAll(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("warning: initial raw-sync audit failed; periodic repair remains active")
	}
	watcher.OpenDispatch()
	refreshRoots := func() error {
		var err error
		pendingRoots, err = refreshRawSyncRoots(watcher, pendingRoots)
		return err
	}
	return runRawSyncLoop(
		ctx, worker, watchCfg.Interval, defaultRawSyncRetry, refreshRoots,
	)
}

func runRawSyncLoop(
	ctx context.Context,
	worker rawSyncLoopWorker,
	auditInterval time.Duration,
	retryInterval time.Duration,
	refreshRoots func() error,
) error {
	audit := time.NewTicker(auditInterval)
	retry := time.NewTicker(retryInterval)
	defer audit.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-audit.C:
			if err := worker.AuditAll(ctx); err != nil && ctx.Err() == nil {
				log.Printf("warning: raw-sync audit failed")
			}
		case <-retry.C:
			if refreshRoots != nil {
				if err := refreshRoots(); err != nil && ctx.Err() == nil {
					log.Printf("warning: raw-sync watcher registration retry failed")
				}
			}
			if err := worker.Drain(ctx); err != nil && ctx.Err() == nil {
				log.Printf("warning: raw-sync upload retry failed")
			}
		}
	}
}

func registerRawSyncRoots(
	registrar rawSyncRootRegistrar,
	roots []syncpkg.WatchRoot,
) ([]syncpkg.WatchRoot, int) {
	pending := make([]syncpkg.WatchRoot, 0, len(roots))
	results := registrar.RegisterRoots(roots, recursiveWatchBudget)
	for i, root := range roots {
		if i >= len(results) ||
			(!results[i].MissingRootLifecycleOwned &&
				(!root.Exists || rawSyncRootUncovered(results[i]))) {
			pending = append(pending, root)
		}
	}
	return pending, len(pending)
}

func rawSyncRootUncovered(result syncpkg.RecursiveWatchResult) bool {
	return result.Err != nil || result.Unwatched != 0 ||
		result.BudgetExhausted || result.ResourceExhausted || result.Watched == 0
}

func refreshRawSyncRoots(
	registrar rawSyncRootRegistrar,
	pending []syncpkg.WatchRoot,
) ([]syncpkg.WatchRoot, error) {
	var ready []syncpkg.WatchRoot
	remaining := make([]syncpkg.WatchRoot, 0, len(pending))
	var errs []error
	for _, root := range pending {
		info, err := os.Stat(root.Path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			remaining = append(remaining, root)
		case err != nil:
			remaining = append(remaining, root)
			errs = append(errs, fmt.Errorf("inspect pending watch root: %w", err))
		case !info.IsDir():
			remaining = append(remaining, root)
		default:
			root.Exists = true
			ready = append(ready, root)
		}
	}
	failed, _ := registerRawSyncRoots(registrar, ready)
	remaining = append(remaining, failed...)
	return remaining, errors.Join(errs...)
}

func validateRawSyncWatchConfig(cfg rawSyncWatchConfig, credential string) error {
	if strings.TrimSpace(cfg.Server) == "" {
		return errors.New("--server or AGENTSVIEW_RAW_SYNC_URL is required")
	}
	parsed, err := url.Parse(cfg.Server)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("raw-sync server URL is invalid")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("raw-sync server URL must not contain credentials")
	}
	if parsed.Scheme == "http" {
		if !cfg.AllowInsecureHTTP {
			return errors.New("raw-sync server URL must use HTTPS")
		}
		if !rawSyncLoopbackHost(parsed.Hostname()) {
			return errors.New("insecure raw-sync HTTP is limited to loopback")
		}
	}
	if strings.TrimSpace(cfg.DeviceID) == "" {
		return errors.New("--device-id or AGENTSVIEW_RAW_SYNC_DEVICE_ID is required")
	}
	if credential == "" {
		return errors.New("AGENTSVIEW_RAW_SYNC_CREDENTIAL is required")
	}
	if cfg.Debounce <= 0 || cfg.Interval <= 0 || cfg.AuditLimit <= 0 {
		return errors.New("debounce, interval, and audit-limit must be positive")
	}
	return nil
}

func rawSyncLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type rawSyncWatchError struct {
	retry syncpkg.WatchBatch
}

func (*rawSyncWatchError) Error() string {
	return "raw-sync watcher work failed"
}

func (e *rawSyncWatchError) WatchRetryBatch() syncpkg.WatchBatch {
	retry := e.retry
	retry.Paths = append([]string(nil), retry.Paths...)
	retry.ReconcileRoots = append([]string(nil), retry.ReconcileRoots...)
	return retry
}

func rawSyncWatchRetryBatch(batch syncpkg.WatchBatch) syncpkg.WatchBatch {
	if batch.FullSync || len(batch.Renames) != 0 {
		return syncpkg.WatchBatch{FullSync: true, LostEvents: batch.LostEvents}
	}
	return syncpkg.WatchBatch{
		Paths:          append([]string(nil), batch.Paths...),
		ReconcileRoots: append([]string(nil), batch.ReconcileRoots...),
		LostEvents:     batch.LostEvents,
	}
}

func rawSyncWatchCallback(
	handle func(context.Context, syncpkg.WatchBatch) error,
) func(context.Context, syncpkg.WatchBatch) error {
	return func(ctx context.Context, batch syncpkg.WatchBatch) error {
		if err := handle(ctx, batch); err != nil {
			return &rawSyncWatchError{retry: rawSyncWatchRetryBatch(batch)}
		}
		return nil
	}
}

func rawSyncProvidersAndRoots(
	ctx context.Context,
	cfg config.Config,
) ([]parser.Provider, []syncpkg.WatchRoot, error) {
	var providers []parser.Provider
	rootIndex := make(map[string]int)
	var roots []syncpkg.WatchRoot
	for _, factory := range cfg.LocalProviderFactories() {
		if factory.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			continue
		}
		def := factory.Definition()
		configuredRoots := rawSyncFilesystemRoots(cfg.ResolveDirs(def.Type))
		for index, root := range configuredRoots {
			absoluteRoot, err := filepath.Abs(root)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"raw-sync resolve configured root for %s: %w", def.Type, err,
				)
			}
			configuredRoots[index] = absoluteRoot
		}
		if len(configuredRoots) == 0 {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: configuredRoots, Machine: cfg.LocalMachineName,
			SourceMachines: cfg.SourceMachines[def.Type],
		})
		watchRoots, err := parser.ResolveWatchRoots(ctx, provider)
		if err != nil {
			return nil, nil, fmt.Errorf("raw-sync watch roots for %s: %w", def.Type, err)
		}
		providers = append(providers, provider)
		for _, planned := range watchRoots {
			path := filepath.Clean(planned.Path)
			info, statErr := os.Stat(path)
			exists := statErr == nil && info != nil && info.IsDir()
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("raw-sync inspect watch root: %w", statErr)
			}
			scope := syncpkg.WatchScope{
				Agent:   string(def.Type),
				SyncDir: rawSyncConfiguredRootForPath(configuredRoots, path),
			}
			if index, found := rootIndex[path]; found {
				if index < 0 || index >= len(roots) {
					return nil, nil, fmt.Errorf("raw-sync watch root index is invalid")
				}
				root := &roots[index]
				root.Recursive = root.Recursive || planned.Recursive
				root.Exists = root.Exists || exists
				if !slices.Contains(root.Scopes, scope) {
					root.Scopes = append(root.Scopes, scope)
				}
				continue
			}
			rootIndex[path] = len(roots)
			roots = append(roots, syncpkg.WatchRoot{
				Path: path, Recursive: planned.Recursive, Exists: exists,
				Scopes: []syncpkg.WatchScope{scope},
			})
		}
	}
	return providers, roots, nil
}

func rawSyncFilesystemRoots(roots []string) []string {
	return slices.DeleteFunc(append([]string(nil), roots...), func(root string) bool {
		return root == "" || strings.HasPrefix(strings.ToLower(root), "s3://")
	})
}

func rawSyncConfiguredRootForPath(configuredRoots []string, watchPath string) string {
	best := ""
	for _, candidate := range configuredRoots {
		candidate = filepath.Clean(candidate)
		relative, err := filepath.Rel(candidate, watchPath)
		if err != nil || relative == ".." || strings.HasPrefix(
			relative, ".."+string(filepath.Separator),
		) {
			continue
		}
		if len(candidate) > len(best) {
			best = candidate
		}
	}
	if best != "" {
		return best
	}
	return watchPath
}

func runRawSyncStatus(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadReadOnly()
	if err != nil {
		return err
	}
	path := rawSyncCheckpointPath(cfg.DataDir)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return writeRawSyncStatus(out, rawcheckpoint.ClientStatus{})
	} else if err != nil {
		return err
	}
	store, err := rawcheckpoint.OpenReadOnly(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	status, err := store.ClientStatus(ctx)
	if err != nil {
		return err
	}
	return writeRawSyncStatus(out, status)
}

func writeRawSyncStatus(out io.Writer, status rawcheckpoint.ClientStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(payload))
	return err
}

func rawSyncCheckpointPath(dataDir string) string {
	return filepath.Join(dataDir, "raw-sync", "checkpoint.db")
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
