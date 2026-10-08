package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
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
	Provider parser.Provider
	// ConfigEntries are the configured paths; ConfiguredRoots are the
	// provider's normalized capture roots resolved from them.
	ConfigEntries   []string
	ConfiguredRoots []string
	factory         parser.ProviderFactory
	config          parser.ProviderConfig
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
	cfg.Server = strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
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
		providerConfig, err := rawSyncProviderConfig(cfg, typ)
		if err != nil {
			return nil, errors.New("could not resolve a selected provider root")
		}
		entries := slices.Clone(providerConfig.Roots)
		provider := factory.NewProvider(providerConfig)
		// Bind the provider's normalized roots, such as a Goose home resolved
		// to its sessions directory, so capture plans fall inside the selection.
		roots := entries
		if normalized, ok := provider.(interface{ ConfiguredRoots() []string }); ok {
			roots = rawSyncFilesystemRoots(normalized.ConfiguredRoots())
		}
		if roots, err = uniqueRawSyncBackfillRoots(roots); err != nil {
			return nil, err
		}
		if len(roots) == 0 {
			return nil, errors.New("selected provider has no configured filesystem roots")
		}
		selected = append(selected, rawSyncBackfillProvider{
			Provider: provider, ConfigEntries: entries, ConfiguredRoots: roots,
			factory: factory, config: providerConfig,
		})
	}
	return selected, nil
}

// uniqueRawSyncBackfillRoots keeps the provider's root order, which decides
// the owning root of a file under overlapping roots.
func uniqueRawSyncBackfillRoots(roots []string) ([]string, error) {
	unique := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, errors.New("could not resolve a selected provider root")
		}
		absolute = filepath.Clean(absolute)
		if !slices.Contains(unique, absolute) {
			unique = append(unique, absolute)
		}
	}
	return unique, nil
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
	archiveOnly, err := db.ArchiveOnlyAt(ctx, appCfg.DBPath)
	if err != nil {
		return err
	}
	if archiveOnly {
		return db.ErrArchiveOnly
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
	// Rerunning cannot start a run while a root is unavailable, so this exits
	// with an ordinary error instead of the retryable incomplete code.
	if errors.Is(err, rawcheckpoint.ErrConfiguredRootUnavailable) {
		return fmt.Errorf("raw-sync backfill: %w", err)
	}
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
	for _, item := range selected {
		typ := item.Provider.Definition().Type
		spec.Providers = append(spec.Providers, typ)
		for _, entry := range item.ConfigEntries {
			spec.Entries = append(spec.Entries, rawcheckpoint.BackfillEntry{Provider: typ, Path: entry})
		}
	}
	// An existing run selects its saved roots; BeginBackfill rejects it if the
	// config entries changed.
	if _, err := store.BackfillProgress(ctx, cfg.RunID); err == nil {
		for index := range selected {
			roots, err := resumeRawSyncBackfillProvider(ctx, store, cfg.RunID, &selected[index])
			if err != nil {
				return rawcheckpoint.BackfillRunSpec{}, err
			}
			spec.Roots = append(spec.Roots, roots...)
		}
		return spec, nil
	}
	for index := range selected {
		item := &selected[index]
		typ := item.Provider.Definition().Type
		var roots []rawcheckpoint.BackfillRoot
		for _, path := range item.ConfiguredRoots {
			projectPath, err := parser.RawCaptureProjectPath(item.Provider, path)
			if err != nil {
				return rawcheckpoint.BackfillRunSpec{}, fmt.Errorf(
					"a configured %s project registry could not be read or decoded; repair projects.json and retry: %w",
					typ, rawcheckpoint.ErrConfiguredRootUnavailable,
				)
			}
			root, err := store.ResolveConfiguredRoot(ctx, typ, path)
			if errors.Is(err, rawcheckpoint.ErrConfiguredRootUnavailable) {
				return rawcheckpoint.BackfillRunSpec{}, fmt.Errorf(
					"a configured %s root is unavailable; mount it or remove it "+
						"from the configuration: %w", typ, err,
				)
			}
			if err != nil {
				return rawcheckpoint.BackfillRunSpec{}, rawcheckpoint.ErrBackfillIncomplete
			}
			roots = append(roots, rawcheckpoint.BackfillRoot{
				ConfiguredRoot: root, ProjectPath: projectPath,
			})
		}
		spec.Roots = append(spec.Roots, bindRawSyncBackfillProviderRoots(item, roots)...)
	}
	return spec, nil
}

// resumeRawSyncBackfillProvider uses the saved roots and project metadata even
// when a registry or symlink has changed since the run started.
func resumeRawSyncBackfillProvider(
	ctx context.Context,
	store *rawcheckpoint.Store,
	runID string,
	item *rawSyncBackfillProvider,
) ([]rawcheckpoint.BackfillSelection, error) {
	stored, err := store.BackfillRoots(ctx, runID, item.Provider.Definition().Type)
	if err != nil {
		return nil, rawcheckpoint.ErrBackfillConflict
	}
	return bindRawSyncBackfillProviderRoots(item, stored), nil
}

func bindRawSyncBackfillProviderRoots(item *rawSyncBackfillProvider, roots []rawcheckpoint.BackfillRoot) []rawcheckpoint.BackfillSelection {
	selection := make([]rawcheckpoint.BackfillSelection, 0, len(roots))
	providerConfig := item.config.Clone()
	providerConfig.Roots = make([]string, 0, len(roots))
	providerConfig.RawCaptureProjectDirs = make(map[string]string)
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if seen[root.ID] {
			continue
		}
		seen[root.ID] = true
		selection = append(selection, rawcheckpoint.BackfillSelection{
			Provider: root.Provider, ConfiguredRootID: root.ID, ProjectPath: root.ProjectPath,
		})
		providerConfig.Roots = append(providerConfig.Roots, root.LocalPath)
		if root.ProjectPath != "" {
			providerConfig.RawCaptureProjectDirs[root.LocalPath] = root.ProjectPath
		}
	}
	if item.factory != nil {
		item.Provider = item.factory.NewProvider(providerConfig)
		item.ConfiguredRoots = providerConfig.Roots
	}
	return selection
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
	if err != nil {
		return err
	}
	switch {
	case progress.Failures["rejected"] > 0:
		_, err = fmt.Fprintln(out, "The server rejected a capture; fix the source and start a new run ID.")
	case progress.Failures["capture_lost"] > 0:
		_, err = fmt.Fprintln(out, "A capture was lost before upload; start a new run ID.")
	}
	return err
}

func rawSyncBackfillResultError(cause error, emitted bool) error {
	if cause == nil {
		cause = rawcheckpoint.ErrBackfillIncomplete
	}
	if errors.Is(cause, rawcheckpoint.ErrDestinationMismatch) || errors.Is(cause, rawcheckpoint.ErrDestinationUnknown) {
		return cause
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
