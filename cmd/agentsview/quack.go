package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
)

const quackSyncStateTarget = "quack"

type QuackPushConfig struct {
	Full            bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
}

func newQuackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "quack",
		Short:        "Quack sync and serve commands",
		GroupID:      groupData,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newQuackPushCommand())
	cmd.AddCommand(newQuackStatusCommand())
	cmd.AddCommand(newQuackServeCommand())
	return cmd
}

func newQuackPushCommand() *cobra.Command {
	var cfg QuackPushConfig
	cmd := &cobra.Command{
		Use:          "push",
		Short:        "Push local data to Quack",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runQuackPush(cfg)
		},
	}
	cmd.Flags().BoolVar(&cfg.Full, "full", false, "Force full local resync and Quack push")
	cmd.Flags().StringVar(&cfg.ProjectsFlag, "projects", "", "Comma-separated list of projects to push (inclusive)")
	cmd.Flags().StringVar(&cfg.ExcludeProjects, "exclude-projects", "", "Comma-separated list of projects to exclude from push")
	cmd.Flags().BoolVar(&cfg.AllProjects, "all-projects", false, "Ignore configured project filters for this run")
	cmd.Flags().BoolVar(&cfg.Watch, "watch", false, "Continue watching local files and pushing changes")
	cmd.Flags().DurationVar(&cfg.Debounce, "debounce", defaultWatchDebounce, "Coalesce window after a change before pushing (--watch only)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", defaultWatchInterval, "Periodic floor push interval (--watch only)")
	return cmd
}

func newQuackStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "Show Quack sync status",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			runQuackStatus()
		},
	}
}

func newQuackServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve from Quack (read-only)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			appCfg, basePath, err := loadQuackServeConfig(cmd)
			if err != nil {
				fatal("%v", err)
			}
			runQuackServe(appCfg, basePath)
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

func runQuackPush(cfg QuackPushConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	quackCfg, err := appCfg.ResolveQuack()
	if err != nil {
		fatal("quack push: %v", err)
	}
	if quackCfg.URL == "" {
		fatal("quack push: url not configured")
	}
	projects, excludeProjects, err := resolveQuackPushProjects(quackCfg, cfg)
	if err != nil {
		fatal("quack push: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		fatal("opening writer: %v", err)
	}
	defer cleanup()

	if cfg.Watch {
		fmt.Printf(
			"agentsview quack watch: pushing to Quack "+
				"(debounce %s, floor %s)\n",
			cfg.Debounce, cfg.Interval,
		)
		err := backend.QuackPushWatch(
			ctx, quackCfg, cfg, projects, excludeProjects,
			cfg.Debounce, cfg.Interval,
		)
		if err != nil {
			fatal("quack watch: %v", err)
		}
		return
	}

	result, err := backend.QuackPush(
		ctx, quackCfg, cfg, projects, excludeProjects,
	)
	if err != nil {
		fatal("quack push: %v", err)
	}
	fmt.Printf(
		"Pushed %d sessions, %d messages to Quack in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		result.Duration.Round(time.Millisecond),
	)
	if result.Errors > 0 {
		fatal("quack push: %d session(s) failed", result.Errors)
	}
}

func runQuackStatus() {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	database, err := openReadOnlyDB(appCfg)
	if err != nil {
		log.Printf(
			"warning: reading local quack status watermark: %v",
			err,
		)
		database = nil
	}
	if database != nil {
		defer database.Close()
	}

	quackCfg, err := appCfg.ResolveQuack()
	if err != nil {
		fatal("quack status: %v", err)
	}
	if quackCfg.URL == "" {
		fatal("quack status: url not configured")
	}
	lastPush := ""
	if database != nil {
		lastPush, err = duckdbsync.ReadLastPushAt(
			database,
			quackSyncStateTarget,
		)
		if err != nil {
			log.Printf("warning: reading duckdb last push: %v", err)
			lastPush = ""
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	duckCfg, err := quackDuckDBConfig(quackCfg)
	if err != nil {
		fatal("quack status: %v", err)
	}
	status, err := duckdbsync.ReadStatusFromConfig(
		ctx, duckCfg, lastPush,
	)
	if err != nil {
		fatal("quack status: %v", err)
	}
	fmt.Printf("Last push:      %s\n", valueOrNever(status.LastPushAt))
	fmt.Printf("Quack sessions: %d\n", status.DuckDBSessions)
	fmt.Printf("Quack messages: %d\n", status.DuckDBMessages)
}

func loadQuackServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadDuckDBServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

func runQuackServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("quack serve: generating auth token: %v", err)
		}
	}
	if err := validateServeConfig(appCfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	quackCfg, err := appCfg.ResolveQuack()
	if err != nil {
		fatal("quack serve: %v", err)
	}
	if quackCfg.URL == "" {
		fatal("quack serve: url not configured")
	}
	duckCfg, err := quackDuckDBConfig(quackCfg)
	if err != nil {
		fatal("quack serve: %v", err)
	}
	applyClassifierConfig(appCfg)
	store, err := duckdbsync.NewStoreFromConfig(duckCfg)
	if err != nil {
		fatal("quack serve: %v", err)
	}
	defer store.Close()
	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}
	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(appCfg.CursorSecret)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		store.SetCursorSecret(secret)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	if err := duckdbsync.CheckSchemaCompat(ctx, store.DB()); err != nil {
		fatal("quack serve: schema incompatible: %v\n"+
			"Run 'agentsview quack push --full' to repopulate the Quack target.", err)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          "quack-serve",
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		fatal("quack serve: %v", err)
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
		opts = append(opts, server.WithBasePath(basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)
	rt, err := startServerWithOptionalCaddy(ctx, appCfg, srv, rtOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("quack serve: %v", err)
	}
	if _, sfErr := WriteDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		log.Printf(
			"warning: could not write daemon runtime record: %v"+
				" (quack serve daemon may not be discoverable by CLI)",
			sfErr,
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}
	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Printf("Auth token: %s\n", rt.Cfg.AuthToken)
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (quack read-only) at %s\n",
			version,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (quack read-only) backend at %s, public at %s\n",
			version,
			rt.LocalURL,
			rt.PublicURL,
		)
	}
	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("quack serve: %v", err)
	}
}

func resolveQuackPushProjects(
	quackCfg config.QuackConfig, cfg QuackPushConfig,
) (projects, exclude []string, err error) {
	return resolveDuckDBPushProjects(config.DuckDBConfig{
		Projects:        quackCfg.Projects,
		ExcludeProjects: quackCfg.ExcludeProjects,
	}, DuckDBPushConfig{
		ProjectsFlag:    cfg.ProjectsFlag,
		ExcludeProjects: cfg.ExcludeProjects,
		AllProjects:     cfg.AllProjects,
	})
}

func quackDuckDBConfig(quackCfg config.QuackConfig) (config.DuckDBConfig, error) {
	duckCfg := quackCfg.AsDuckDBConfig()
	if duckCfg.MachineName == "" {
		host, err := os.Hostname()
		if err != nil {
			return duckCfg, fmt.Errorf(
				"os.Hostname failed (%w)", err,
			)
		}
		duckCfg.MachineName = host
	}
	return duckCfg, nil
}

func logQuackWatchPushResult(res duckdbsync.PushResult, reason pushReason) {
	if res.Errors > 0 {
		log.Printf(
			"quack watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed,
			res.Errors, reason,
		)
		return
	}
	log.Printf(
		"quack watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
}
