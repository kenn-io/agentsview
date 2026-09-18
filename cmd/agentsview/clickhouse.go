package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type ClickHousePushConfig struct {
	Full            bool
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
	// WatchBatch and WatchRecovery are internal watch-loop scope. Explicit
	// pushes leave them nil and retain the historical unscoped sync.
	WatchBatch    *syncpkg.WatchBatch
	WatchRecovery *syncpkg.WatchRecoveryScope
}

type ClickHouseStatusConfig struct {
	AllTargets      bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
}

type clickHouseTargetSelection struct {
	Name      string
	Config    config.ClickHouseConfig
	IsDefault bool
}

func (s clickHouseTargetSelection) label() string {
	if s.Name == "" {
		return "default"
	}
	if s.IsDefault {
		return s.Name + " (default)"
	}
	return s.Name
}

func clickHouseTarget(cfg config.ClickHouseConfig) clickhouse.Target {
	return clickhouse.Target{URL: cfg.URL, Database: cfg.Database}
}

func newClickHouseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "clickhouse",
		Short:        "ClickHouse sync and serve commands",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newClickHousePushCommand())
	cmd.AddCommand(newClickHouseStatusCommand())
	cmd.AddCommand(newClickHouseServeCommand())
	cmd.AddCommand(newClickHouseServiceCommand())
	return cmd
}

func newClickHousePushCommand() *cobra.Command {
	var cfg ClickHousePushConfig
	cmd := &cobra.Command{
		Use:          "push [target]",
		Short:        "Push local data to ClickHouse",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if cfg.AllTargets && cfg.Watch {
				return fmt.Errorf(
					"clickhouse push --watch: %w",
					fmt.Errorf("--all cannot be combined with --watch"),
				)
			}
			if cfg.Watch {
				if err := runClickHousePushWatch(cfg, targetName); err != nil {
					return fmt.Errorf("clickhouse push --watch: %w", err)
				}
				return nil
			}
			if cmd.Flags().Changed("debounce") || cmd.Flags().Changed("interval") {
				fmt.Fprintln(os.Stderr,
					"warning: --debounce and --interval have no effect without --watch")
			}
			if err := runClickHousePush(cfg, targetName); err != nil {
				return fmt.Errorf("clickhouse push: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Push every configured ClickHouse target sequentially")
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and ClickHouse push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	cmd.Flags().BoolVar(&cfg.Watch, "watch", false, "Run continuously, pushing on change plus a periodic floor")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultWatchDebounce, "Coalesce window after a change before pushing (--watch only)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultWatchInterval, "Periodic floor push interval (--watch only)")
	return cmd
}

func newClickHouseStatusCommand() *cobra.Command {
	var cfg ClickHouseStatusConfig
	cmd := &cobra.Command{
		Use:          "status [target]",
		Short:        "Show ClickHouse sync status",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetName := ""
			if len(args) == 1 {
				targetName = args[0]
			}
			if err := runClickHouseStatus(targetName, cfg); err != nil {
				return fmt.Errorf("clickhouse status: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cfg.AllTargets, "all", false, "Show status for every configured ClickHouse target")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects whose push status to show")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of excluded projects whose push status to show")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this status")
	return cmd
}

func newClickHouseServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve from ClickHouse (read-only)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			appCfg, basePath, err := loadClickHouseServeConfig(cmd)
			if err != nil {
				fatal("%v", err)
			}
			runClickHouseServe(appCfg, basePath)
		},
	}
	cmd.Flags().String(
		"base-path",
		"",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)",
	)
	config.RegisterServePFlags(cmd.Flags())
	return cmd
}

func runClickHousePush(cfg ClickHousePushConfig, targetName string) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := resolveClickHouseTargetSelections(appCfg, targetName, cfg.AllTargets)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		return fmt.Errorf("opening writer: %w", err)
	}
	defer cleanup()

	var failures []string
	for i, target := range targets {
		if len(targets) > 1 || target.Name != "" {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Target: %s\n", target.label())
		}
		if err := runClickHousePushTarget(ctx, backend, appCfg, cfg, target); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(failures, fmt.Sprintf("%s: %v", target.label(), err))
			fmt.Fprintf(os.Stderr, "warning: clickhouse push target %s failed: %v\n",
				target.label(), err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d clickhouse target(s) failed: %s",
			len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func runClickHousePushTarget(
	ctx context.Context,
	backend archiveWriteBackend,
	appCfg config.Config,
	cfg ClickHousePushConfig,
	target clickHouseTargetSelection,
) error {
	target, err := resolveClickHouseTargetConfig(appCfg, target)
	if err != nil {
		return err
	}
	if target.Config.URL == "" {
		return fmt.Errorf("url not configured")
	}
	if err := clickhouse.CheckTransportSecurity(
		target.Config.URL, target.Config.AllowInsecure,
	); err != nil {
		return err
	}
	projects, excludeProjects, err := resolveClickHousePushProjects(target.Config, cfg)
	if err != nil {
		return err
	}
	result, err := backend.ClickHousePush(ctx, target, cfg, projects, excludeProjects)
	if err != nil {
		return err
	}
	writeClickHousePushSummary(os.Stdout, result)
	if result.Errors > 0 {
		return fmt.Errorf("%d session(s) failed", result.Errors)
	}
	return nil
}

func newClickHousePushProgressPrinter() func(clickhouse.PushProgress) {
	lastPhase := ""
	start := time.Now()
	return func(p clickhouse.PushProgress) {
		phase := p.Phase
		if phase == "" {
			phase = "sessions"
		}
		if lastPhase != "" && phase != lastPhase {
			fmt.Println()
		}
		lastPhase = phase
		fmt.Printf("\r%s (%s elapsed)\x1b[K",
			clickHousePushProgressLine(p), time.Since(start).Round(time.Second))
	}
}

func clickHousePushProgressLine(p clickhouse.PushProgress) string {
	if p.Phase == "preparing" {
		if p.SessionsTotal == 0 {
			return "Preparing push (metadata, fingerprints)..."
		}
		return fmt.Sprintf(
			"Preparing... %d/%d sessions fingerprinted",
			p.SessionsDone, p.SessionsTotal,
		)
	}
	return fmt.Sprintf(
		"Pushing... %d/%d sessions, %d messages",
		p.SessionsDone, p.SessionsTotal, p.MessagesDone,
	)
}

func writeClickHousePushSummary(w io.Writer, result clickhouse.PushResult) {
	dur := result.Duration.Round(time.Millisecond)
	errSuffix := ""
	if result.Errors > 0 {
		errSuffix = fmt.Sprintf(", %d error(s)", result.Errors)
	}
	fmt.Fprintf(w, "Pushed %d sessions, %d messages%s in %s\n",
		result.SessionsPushed, result.MessagesPushed, errSuffix, dur)
	if result.SkippedUnchanged > 0 {
		fmt.Fprintf(w, "Skipped %d unchanged session(s)\n", result.SkippedUnchanged)
	}
	if result.DeletedStale > 0 {
		fmt.Fprintf(w, "Removed %d stale session(s)\n", result.DeletedStale)
	}
}

func runClickHouseStatus(targetName string, cfg ClickHouseStatusConfig) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFile(appCfg.DataDir)

	targets, err := resolveClickHouseTargetSelections(appCfg, targetName, cfg.AllTargets)
	if err != nil {
		return err
	}

	applyClassifierConfig(appCfg)
	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		log.Printf("warning: reading local clickhouse status watermark: %v", err)
		database = nil
	}
	if database != nil {
		defer database.Close()
	}

	var failures []string
	for i, target := range targets {
		if len(targets) > 1 || target.Name != "" {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Target: %s\n", target.label())
		}
		if err := runClickHouseStatusTarget(database, appCfg, target, cfg); err != nil {
			if len(targets) == 1 {
				return err
			}
			failures = append(failures, fmt.Sprintf("%s: %v", target.label(), err))
			fmt.Fprintf(os.Stderr, "warning: clickhouse status target %s failed: %v\n",
				target.label(), err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d clickhouse target(s) failed: %s",
			len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func runClickHouseStatusTarget(
	database *db.DB,
	appCfg config.Config,
	target clickHouseTargetSelection,
	cfg ClickHouseStatusConfig,
) error {
	target, err := resolveClickHouseTargetConfig(appCfg, target)
	if err != nil {
		return err
	}
	if target.Config.URL == "" {
		return fmt.Errorf("url not configured")
	}
	if err := clickhouse.CheckTransportSecurity(
		target.Config.URL, target.Config.AllowInsecure,
	); err != nil {
		return err
	}
	if _, _, err := resolveClickHousePushProjects(target.Config, ClickHousePushConfig{
		ProjectsFlag:    cfg.ProjectsFlag,
		ExcludeProjects: cfg.ExcludeProjects,
		AllProjects:     cfg.AllProjects,
	}); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	archiveID := ""
	if database != nil {
		archiveID, err = database.GetArchiveID(ctx)
		if err != nil {
			log.Printf("warning: reading local archive id: %v", err)
			archiveID = ""
		}
	}
	status, err := clickhouse.ReadStatus(
		ctx, clickHouseTarget(target.Config), target.Config.MachineName, archiveID,
	)
	if err != nil {
		return err
	}
	fmt.Printf("Machine:            %s\n", status.Machine)
	fmt.Printf("Last push:          %s\n", valueOrNever(status.LastPushAt))
	fmt.Printf("Last push machine:  %s\n", status.LastPushMachine)
	fmt.Printf("ClickHouse sessions: %d\n", status.Sessions)
	fmt.Printf("ClickHouse messages: %d\n", status.Messages)
	if status.SchemaMissing {
		fmt.Println("Schema:             not created yet")
	}
	return nil
}

func loadClickHouseServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadClickHouseServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

type clickHouseServeStartup struct {
	cfg     config.Config
	ctx     context.Context
	rtOpts  serveRuntimeOptions
	srv     *server.Server
	cleanup func()
}

var prepareClickHouseServe = prepareClickHouseServeImpl

func prepareClickHouseServeImpl(appCfg config.Config, basePath string) (clickHouseServeStartup, error) {
	if err := validateServeConfig(appCfg); err != nil {
		return clickHouseServeStartup{}, fmt.Errorf("invalid serve config: %w", err)
	}
	chCfg, err := appCfg.ResolveClickHouse()
	if err != nil {
		return clickHouseServeStartup{}, fmt.Errorf("clickhouse serve: %w", err)
	}
	if chCfg.URL == "" {
		return clickHouseServeStartup{}, errors.New("clickhouse serve: url not configured")
	}
	if err := clickhouse.CheckTransportSecurity(chCfg.URL, chCfg.AllowInsecure); err != nil {
		return clickHouseServeStartup{}, fmt.Errorf("clickhouse serve: %w", err)
	}

	applyClassifierConfig(appCfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cleanupStore := func() {}
	cleanup := func() {
		stop()
		cleanupStore()
	}

	target := clickHouseTarget(chCfg)
	if err := clickhouse.EnsureSchema(ctx, target); err != nil {
		if !clickhouse.IsPermissionError(err) {
			cleanup()
			return clickHouseServeStartup{}, fmt.Errorf(
				"clickhouse serve: schema migration failed: %w", err,
			)
		}
	}
	store, err := clickhouse.NewStore(ctx, target)
	if err != nil {
		cleanup()
		return clickHouseServeStartup{}, fmt.Errorf("clickhouse serve: %w", err)
	}
	cleanupStore = func() { _ = store.Close() }
	if err := applyRequiredCursorSecret(store, appCfg); err != nil {
		cleanup()
		return clickHouseServeStartup{}, fmt.Errorf("clickhouse serve: %w", err)
	}
	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          "clickhouse-serve",
		BasePath:      basePath,
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		cleanup()
		return clickHouseServeStartup{}, fmt.Errorf("clickhouse serve: %w", err)
	}
	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
			ReadOnly:  true,
		}),
		server.WithDataDir(appCfg.DataDir),
		server.WithBaseContext(ctx),
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(rtOpts.BasePath))
	}
	return clickHouseServeStartup{
		cfg: appCfg, ctx: ctx, rtOpts: rtOpts,
		srv: server.New(appCfg, store, nil, opts...), cleanup: cleanup,
	}, nil
}

func runClickHouseServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("clickhouse serve: generating auth token: %v", err)
		}
	}
	startup, err := prepareClickHouseServe(appCfg, basePath)
	if err != nil {
		fatal("%v", err)
	}
	defer startup.cleanup()
	appCfg = startup.cfg
	ctx := startup.ctx
	rtOpts := startup.rtOpts
	srv := startup.srv

	rt, err := startServerWithOptionalCaddy(ctx, appCfg, srv, rtOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("clickhouse serve: %v", err)
	}
	if writeClickHouseServeRuntimeRecord(rt) {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}
	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Println("Auth enabled. Token is configured.")
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf("agentsview %s (clickhouse read-only) at %s\n", version, rt.LocalURL)
	} else {
		fmt.Printf("agentsview %s (clickhouse read-only) at %s (public %s)\n",
			version, rt.LocalURL, rt.PublicURL)
	}
	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("clickhouse serve: %v", err)
	}
}

func writeClickHouseServeRuntimeRecord(rt *serveRuntime) bool {
	if _, sfErr := writeDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, rt.PublicURL, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr,
			"clickhouse serve daemon may not be discoverable by CLI", "",
		)
		return false
	}
	return true
}

func resolveClickHousePushProjects(
	chCfg config.ClickHouseConfig, cfg ClickHousePushConfig,
) (projects, exclude []string, err error) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		return nil, nil, fmt.Errorf(
			"--projects and --exclude-projects are mutually exclusive",
		)
	}
	if cfg.AllProjects && (cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		return nil, nil, fmt.Errorf(
			"--all-projects cannot be combined with --projects or --exclude-projects",
		)
	}
	projects = chCfg.Projects
	exclude = chCfg.ExcludeProjects
	if cfg.AllProjects {
		projects = nil
		exclude = nil
	}
	if cfg.ProjectsFlag != "" {
		projects = splitProjectList(cfg.ProjectsFlag)
		exclude = nil
	}
	if cfg.ExcludeProjects != "" {
		exclude = splitProjectList(cfg.ExcludeProjects)
		projects = nil
	}
	if len(projects) > 0 && len(exclude) > 0 {
		return nil, nil, fmt.Errorf(
			"projects and exclude_projects are mutually exclusive",
		)
	}
	return projects, exclude, nil
}

func resolveClickHouseTargetSelections(
	appCfg config.Config, targetName string, allTargets bool,
) ([]clickHouseTargetSelection, error) {
	if allTargets && strings.TrimSpace(targetName) != "" {
		return nil, fmt.Errorf("target name cannot be combined with --all")
	}
	if len(appCfg.ClickHouseTargets) == 0 {
		if strings.TrimSpace(targetName) != "" {
			return nil, fmt.Errorf(
				"clickhouse target %q is not configured; config uses a single legacy [clickhouse] block",
				targetName,
			)
		}
		return []clickHouseTargetSelection{{IsDefault: true}}, nil
	}
	names, defaultName, err := appCfg.ClickHouseTargetNames()
	if err != nil {
		return nil, err
	}
	selections := make([]clickHouseTargetSelection, 0, len(names))
	for _, name := range names {
		selections = append(selections, clickHouseTargetSelection{
			Name:      name,
			IsDefault: name == defaultName,
		})
	}
	if allTargets {
		return selections, nil
	}
	normalizedTarget := strings.TrimSpace(strings.ToLower(targetName))
	if normalizedTarget == "" {
		return selections[:1], nil
	}
	for _, target := range selections {
		if target.Name == normalizedTarget {
			return []clickHouseTargetSelection{target}, nil
		}
	}
	return nil, fmt.Errorf("clickhouse target %q is not configured", targetName)
}

func resolveClickHouseTargetConfig(
	appCfg config.Config, target clickHouseTargetSelection,
) (clickHouseTargetSelection, error) {
	var (
		chCfg config.ClickHouseConfig
		err   error
	)
	if target.Name == "" {
		chCfg, err = appCfg.ResolveClickHouse()
	} else {
		chCfg, err = appCfg.ResolveClickHouseTarget(target.Name)
	}
	if err != nil {
		return clickHouseTargetSelection{}, err
	}
	target.Config = chCfg
	return target, nil
}
