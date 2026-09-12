package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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
)

const (
	defaultRawSyncBackfillBatch = 128
	rawSyncBackfillExitCode     = 2
)

type rawSyncBackfillConfig struct {
	Server            string
	DeviceID          string
	AllowInsecureHTTP bool
	RunID             string
	Providers         []string
	BatchSize         int
	Format            string
}

type rawSyncBackfillProvider struct {
	Provider        parser.Provider
	ConfiguredRoots []string
}

func newRawSyncBackfillCommand() *cobra.Command {
	cfg := rawSyncBackfillConfig{BatchSize: defaultRawSyncBackfillBatch, Format: "human"}
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Upload a finite snapshot of configured raw session sources",
		Long: "Upload a finite snapshot of configured raw session sources.\n\n" +
			"The device credential is read only from AGENTSVIEW_RAW_SYNC_CREDENTIAL; " +
			"it cannot be passed as an argument.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg.Format = outputFormat(cmd)
			cfg.Server = firstNonempty(cfg.Server, os.Getenv("AGENTSVIEW_RAW_SYNC_URL"))
			cfg.DeviceID = firstNonempty(
				cfg.DeviceID, os.Getenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID"),
			)
			credential := os.Getenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL")
			normalized, err := normalizeRawSyncBackfillConfig(cfg, credential)
			if err != nil {
				return fmt.Errorf("raw-sync backfill: %w", err)
			}
			ctx, stop := signal.NotifyContext(
				cmd.Context(), os.Interrupt, syscall.SIGTERM,
			)
			defer stop()
			return runRawSyncBackfill(
				ctx, cmd.OutOrStdout(), normalized, credential,
			)
		},
	}
	cmd.Flags().StringVar(&cfg.Server, "server", "", "Raw-sync server URL (or AGENTSVIEW_RAW_SYNC_URL)")
	cmd.Flags().StringVar(&cfg.DeviceID, "device-id", "", "Provisioned device ID (or AGENTSVIEW_RAW_SYNC_DEVICE_ID)")
	cmd.Flags().BoolVar(&cfg.AllowInsecureHTTP, "allow-insecure-http", false, "Allow HTTP only for a loopback raw-sync server")
	cmd.Flags().StringVar(&cfg.RunID, "run-id", "", "Stable migration run ID")
	cmd.Flags().StringArrayVar(&cfg.Providers, "provider", nil, "Configured provider to backfill (repeatable)")
	cmd.Flags().IntVar(&cfg.BatchSize, "batch-size", defaultRawSyncBackfillBatch, "Maximum source and upload work per batch (1-512)")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func normalizeRawSyncBackfillConfig(
	cfg rawSyncBackfillConfig,
	credential string,
) (rawSyncBackfillConfig, error) {
	connection := rawSyncWatchConfig{
		Server: cfg.Server, DeviceID: cfg.DeviceID,
		AllowInsecureHTTP: cfg.AllowInsecureHTTP,
		Debounce:          time.Second, Interval: time.Second, AuditLimit: 1,
	}
	if err := validateRawSyncWatchConfig(connection, credential); err != nil {
		return cfg, err
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.Server))
	if err != nil || (parsed.Path != "" && parsed.Path != "/") {
		return cfg, errors.New("raw-sync server URL must be an origin")
	}
	parsed.Path, parsed.RawPath = "", ""
	cfg.Server = strings.TrimRight(parsed.String(), "/")
	if !validRawSyncBackfillRunID(cfg.RunID) {
		return cfg, errors.New("--run-id must be 1-128 letters, digits, '_' or '-'")
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > 512 {
		return cfg, errors.New("--batch-size must be between 1 and 512")
	}
	cfg.Format = strings.ToLower(strings.TrimSpace(cfg.Format))
	if cfg.Format != "human" && cfg.Format != "json" {
		return cfg, errors.New("--format must be human or json")
	}
	providers := make([]string, 0, len(cfg.Providers))
	for _, value := range cfg.Providers {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" || !validRawSyncProviderName(name) {
			return cfg, errors.New("--provider requires a configured provider name")
		}
		providers = append(providers, name)
	}
	if len(providers) == 0 {
		return cfg, errors.New("at least one --provider is required")
	}
	slices.Sort(providers)
	cfg.Providers = slices.Compact(providers)
	return cfg, nil
}

func validRawSyncBackfillRunID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func validRawSyncProviderName(value string) bool {
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return value != ""
}

func selectRawSyncBackfillProviders(
	cfg config.Config,
	names []string,
) ([]rawSyncBackfillProvider, error) {
	names = append([]string(nil), names...)
	for index := range names {
		names[index] = strings.ToLower(strings.TrimSpace(names[index]))
	}
	slices.Sort(names)
	names = slices.Compact(names)
	factories := make(map[parser.AgentType]parser.ProviderFactory)
	for _, factory := range cfg.LocalProviderFactories() {
		factories[factory.Definition().Type] = factory
	}
	selected := make([]rawSyncBackfillProvider, 0, len(names))
	for _, name := range names {
		typ := parser.AgentType(name)
		factory, ok := factories[typ]
		if !ok {
			return nil, errors.New("selected provider is not configured")
		}
		if factory.Capabilities().RawCapture.Support != parser.CapabilitySupported {
			return nil, errors.New("selected provider does not support raw capture")
		}
		roots := rawSyncFilesystemRoots(cfg.ResolveDirs(typ))
		for index, root := range roots {
			absolute, err := filepath.Abs(root)
			if err != nil {
				return nil, errors.New("could not resolve a selected provider root")
			}
			roots[index] = filepath.Clean(absolute)
		}
		slices.Sort(roots)
		roots = slices.Compact(roots)
		if len(roots) == 0 {
			return nil, errors.New("selected provider has no configured filesystem roots")
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: roots, Machine: cfg.LocalMachineName,
			SourceMachines: cfg.SourceMachines[typ],
		})
		selected = append(selected, rawSyncBackfillProvider{
			Provider: provider, ConfiguredRoots: roots,
		})
	}
	return selected, nil
}

func runRawSyncBackfill(
	ctx context.Context,
	out io.Writer,
	cfg rawSyncBackfillConfig,
	credential string,
) error {
	appCfg, err := config.LoadReadOnly()
	if err != nil {
		return errors.New("raw-sync backfill: configuration could not be loaded")
	}
	selected, err := selectRawSyncBackfillProviders(appCfg, cfg.Providers)
	if err != nil {
		return fmt.Errorf("raw-sync backfill: %w", err)
	}
	store, err := rawcheckpoint.Open(ctx, rawSyncCheckpointPath(appCfg.DataDir))
	if err != nil {
		return errors.New("raw-sync backfill: checkpoint could not be opened")
	}
	defer store.Close()
	if err := store.EnsureDevice(ctx, cfg.DeviceID); err != nil {
		return errors.New("raw-sync backfill: device does not match the checkpoint")
	}
	spec, err := rawSyncBackfillSpec(ctx, store, cfg, selected)
	if err != nil {
		return rawSyncBackfillResultError(err, false)
	}
	client, err := rawclient.NewClient(rawclient.Config{
		BaseURL: cfg.Server, DeviceID: cfg.DeviceID, Credential: credential,
	})
	if err != nil {
		return errors.New("raw-sync backfill: transport configuration is invalid")
	}
	providers := make([]parser.Provider, 0, len(selected))
	for _, item := range selected {
		providers = append(providers, item.Provider)
	}
	return runRawSyncBackfillAttempt(
		ctx, out, cfg, store, spec, providers, client,
	)
}

func runRawSyncBackfillAttempt(
	ctx context.Context,
	out io.Writer,
	cfg rawSyncBackfillConfig,
	store *rawcheckpoint.Store,
	spec rawcheckpoint.BackfillRunSpec,
	providers []parser.Provider,
	transport rawupload.Transport,
) error {
	progress, runErr := rawwatch.RunBackfill(
		ctx, store, rawcapture.New(store), rawupload.New(store, transport, cfg.DeviceID),
		rawwatch.BackfillOptions{Spec: spec, Providers: providers, BatchSize: cfg.BatchSize},
	)
	progress, runErr = recoverRawSyncBackfillProgress(ctx, store, spec, progress, runErr)
	emitted := progress.RunID != ""
	if emitted {
		if err := writeRawSyncBackfillProgress(out, cfg.Format, progress); err != nil {
			return errors.New("raw-sync backfill: output could not be written")
		}
	}
	if runErr != nil || !progress.Complete {
		return rawSyncBackfillResultError(runErr, emitted)
	}
	return nil
}

func rawSyncBackfillSpec(
	ctx context.Context,
	store *rawcheckpoint.Store,
	cfg rawSyncBackfillConfig,
	selected []rawSyncBackfillProvider,
) (rawcheckpoint.BackfillRunSpec, error) {
	spec := rawcheckpoint.BackfillRunSpec{
		RunID: cfg.RunID, DeviceID: cfg.DeviceID, Destination: cfg.Server,
	}
	if progress, err := store.BackfillProgress(ctx, cfg.RunID); err == nil && progress.Complete {
		for _, item := range selected {
			typ := item.Provider.Definition().Type
			stored, err := store.BackfillRoots(ctx, cfg.RunID, typ)
			if err != nil || !sameRawSyncConfiguredRoots(item.ConfiguredRoots, stored) {
				return rawcheckpoint.BackfillRunSpec{}, rawcheckpoint.ErrBackfillConflict
			}
			spec.Providers = append(spec.Providers, typ)
			for _, root := range stored {
				spec.Roots = append(spec.Roots, rawcheckpoint.BackfillSelection{
					Provider: typ, ConfiguredRootID: root.ID,
				})
			}
		}
		return spec, nil
	}
	for _, item := range selected {
		typ := item.Provider.Definition().Type
		spec.Providers = append(spec.Providers, typ)
		for _, path := range item.ConfiguredRoots {
			root, err := store.ResolveConfiguredRoot(ctx, typ, path)
			if err != nil {
				return rawcheckpoint.BackfillRunSpec{}, rawcheckpoint.ErrBackfillIncomplete
			}
			spec.Roots = append(spec.Roots, rawcheckpoint.BackfillSelection{
				Provider: typ, ConfiguredRootID: root.ID,
			})
		}
	}
	return spec, nil
}

func sameRawSyncConfiguredRoots(
	configured []string,
	stored []rawcheckpoint.ConfiguredRoot,
) bool {
	current := make([]string, 0, len(configured))
	for _, root := range configured {
		current = append(current, historicalRawSyncRootIdentity(root))
	}
	want := make([]string, 0, len(stored))
	for _, root := range stored {
		want = append(want, filepath.Clean(root.LocalPath))
	}
	slices.Sort(current)
	slices.Sort(want)
	return slices.Equal(current, want)
}

// historicalRawSyncRootIdentity resolves symlink prefixes without requiring
// the final target to exist. Completed runs use this only to compare current
// configuration with their durable canonical root identity.
func historicalRawSyncRootIdentity(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	absolute = filepath.Clean(absolute)
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(canonical)
	}

	candidate := absolute
	for range 255 {
		prefix, suffix := candidate, ""
		for {
			info, statErr := os.Lstat(prefix)
			if statErr == nil {
				if info.Mode()&os.ModeSymlink != 0 {
					target, readErr := os.Readlink(prefix)
					if readErr != nil {
						return absolute
					}
					if !filepath.IsAbs(target) {
						target = filepath.Join(filepath.Dir(prefix), target)
					}
					candidate = filepath.Clean(filepath.Join(target, suffix))
					break
				}
				canonical, evalErr := filepath.EvalSymlinks(prefix)
				if evalErr != nil {
					return absolute
				}
				return filepath.Clean(filepath.Join(canonical, suffix))
			}
			parent := filepath.Dir(prefix)
			if parent == prefix {
				return absolute
			}
			suffix = filepath.Join(filepath.Base(prefix), suffix)
			prefix = parent
		}
	}
	return absolute
}

func recoverRawSyncBackfillProgress(
	ctx context.Context,
	store *rawcheckpoint.Store,
	spec rawcheckpoint.BackfillRunSpec,
	progress rawcheckpoint.BackfillProgress,
	runErr error,
) (rawcheckpoint.BackfillProgress, error) {
	if progress.RunID != "" || ctx.Err() == nil {
		return progress, runErr
	}
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := store.BeginBackfill(recovery, spec)
	if err != nil {
		return progress, err
	}
	if err := store.RecordBackfillFailure(recovery, spec.RunID, "cancelled"); err != nil {
		return progress, rawcheckpoint.ErrBackfillIncomplete
	}
	current, err := store.BackfillProgress(recovery, spec.RunID)
	if err != nil {
		return progress, rawcheckpoint.ErrBackfillIncomplete
	}
	return current, rawcheckpoint.ErrBackfillIncomplete
}

func writeRawSyncBackfillProgress(
	out io.Writer,
	format string,
	progress rawcheckpoint.BackfillProgress,
) error {
	if format == "json" {
		payload, err := json.Marshal(progress)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(payload))
		return err
	}
	state := "incomplete"
	if progress.Complete {
		state = "complete"
	}
	_, err := fmt.Fprintf(
		out, "Backfill %s %s: %d captured, %d acknowledged, %d pending.\n",
		progress.RunID, state, progress.Captured, progress.Acknowledged, progress.Pending,
	)
	return err
}

func rawSyncBackfillResultError(cause error, emitted bool) error {
	if cause == nil {
		cause = rawcheckpoint.ErrBackfillIncomplete
	}
	if errors.Is(cause, rawcheckpoint.ErrBackfillConflict) ||
		errors.Is(cause, rawcheckpoint.ErrDeviceMismatch) ||
		errors.Is(cause, rawcheckpoint.ErrDeviceNotConfigured) {
		return errors.New("raw-sync backfill selection conflicts with durable state")
	}
	err := errors.New("raw-sync backfill incomplete")
	if emitted {
		return withSilentExitCode(err, rawSyncBackfillExitCode)
	}
	return withExitCode(err, rawSyncBackfillExitCode)
}
