package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/postgres"
	"github.com/wesm/agentsview/internal/server"
)

type PGPushConfig struct {
	Full            bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
}

func runPGPush(cfg PGPushConfig) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		fatal("pg push: --projects and --exclude-projects " +
			"are mutually exclusive")
	}
	if cfg.AllProjects &&
		(cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		fatal("pg push: --all-projects cannot be combined " +
			"with --projects or --exclude-projects")
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg push: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg push: url not configured")
	}

	// CLI flags override config values entirely. When either
	// flag is set, clear both config-derived lists so a CLI
	// include can override a config exclude (and vice versa).
	// --all-projects clears both lists for an unfiltered push.
	projects := pgCfg.Projects
	excludeProjects := pgCfg.ExcludeProjects
	if cfg.AllProjects {
		projects = nil
		excludeProjects = nil
	}
	if cfg.ProjectsFlag != "" {
		projects = splitProjectList(cfg.ProjectsFlag)
		excludeProjects = nil
	}
	if cfg.ExcludeProjects != "" {
		excludeProjects = splitProjectList(cfg.ExcludeProjects)
		projects = nil
	}

	if len(projects) > 0 && len(excludeProjects) > 0 {
		fatal("pg push: projects and exclude_projects " +
			"are mutually exclusive")
	}

	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(
			appCfg.CursorSecret,
		)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		database.SetCursorSecret(secret)
	}

	// Run local sync first so newly discovered sessions
	// are available for push. If a full resync was performed
	// (e.g. due to data version change), force a full PG push
	// since watermarks become stale after a local rebuild.
	didResync := runLocalSync(appCfg, database, cfg.Full)
	forceFull := cfg.Full || didResync

	ps, err := postgres.New(
		pgCfg.URL, pgCfg.Schema, database,
		pgCfg.MachineName, pgCfg.AllowInsecure,
		postgres.SyncOptions{
			Projects:        projects,
			ExcludeProjects: excludeProjects,
		},
	)
	if err != nil {
		fatal("pg push: %v", err)
	}
	defer ps.Close()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	if err := ps.EnsureSchema(ctx); err != nil {
		fatal("pg push schema: %v", err)
	}
	result, err := ps.Push(ctx, forceFull,
		func(p postgres.PushProgress) {
			fmt.Printf(
				"\rPushing... %d/%d sessions, %d messages",
				p.SessionsDone, p.SessionsTotal,
				p.MessagesDone,
			)
		},
	)
	fmt.Print("\r\033[K") // clear progress line
	if err != nil {
		fatal("pg push: %v", err)
	}
	fmt.Printf(
		"Pushed %d sessions, %d messages in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		result.Duration.Round(time.Millisecond),
	)
	if result.Errors > 0 {
		fatal("pg push: %d session(s) failed",
			result.Errors)
	}
}

func runPGStatus() {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	applyClassifierConfig(appCfg)
	database, err := db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg status: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg status: url not configured")
	}

	ps, err := postgres.New(
		pgCfg.URL, pgCfg.Schema, database,
		pgCfg.MachineName, pgCfg.AllowInsecure,
		postgres.SyncOptions{},
	)
	if err != nil {
		fatal("pg status: %v", err)
	}
	defer ps.Close()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt,
	)
	defer stop()

	status, err := ps.Status(ctx)
	if err != nil {
		fatal("pg status: %v", err)
	}
	fmt.Printf("Machine:     %s\n", status.Machine)
	fmt.Printf("Last push:   %s\n",
		valueOrNever(status.LastPushAt))
	fmt.Printf("PG sessions: %d\n", status.PGSessions)
	fmt.Printf("PG messages: %d\n", status.PGMessages)
}

func loadPGServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadPGServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

func runPGServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	// Generate auth token when auth is explicitly required.
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("pg serve: generating auth token: %v", err)
		}
	}

	if err := validateServeConfig(appCfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg serve: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg serve: url not configured")
	}

	applyClassifierConfig(appCfg)
	store, err := postgres.NewStore(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		fatal("pg serve: %v", err)
	}
	defer store.Close()

	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	// Attempt to apply any missing schema migrations before
	// the compatibility check. This handles upgrades (e.g.
	// new tables like tool_result_events) without requiring a
	// manual schema drop. If the PG role is read-only the
	// migration is skipped and the compat check reports what
	// is missing.
	if err := postgres.EnsureSchema(
		ctx, store.DB(), pgCfg.Schema,
	); err != nil {
		if !postgres.IsReadOnlyError(err) {
			fatal("pg serve: schema migration failed: %v", err)
		}
	}

	if err := postgres.CheckSchemaCompat(
		ctx, store.DB(),
	); err != nil {
		fatal("pg serve: schema incompatible: %v\n"+
			"Drop and recreate the PG schema, then run "+
			"'agentsview pg push --full' to repopulate.", err)
	}

	rtOpts := serveRuntimeOptions{
		Mode:          "pg-serve",
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		fatal("pg serve: %v", err)
	}

	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
			ReadOnly:  true,
		}),
		server.WithBaseContext(ctx),
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)

	rt, err := startServerWithOptionalCaddy(
		ctx,
		appCfg,
		srv,
		rtOpts,
	)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("pg serve: %v", err)
	}

	// Write the state file so CLI commands can discover this
	// daemon. ReadOnly=true marks it as pg serve (read-only)
	// so clients can select an appropriate transport.
	if _, sfErr := server.WriteStateFile(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
	); sfErr != nil {
		log.Printf(
			"warning: could not write state file: %v"+
				" (pg serve daemon may not be discoverable by CLI)",
			sfErr,
		)
	} else {
		defer server.RemoveStateFile(rt.Cfg.DataDir, rt.Cfg.Port)
	}

	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Printf("Auth token: %s\n", rt.Cfg.AuthToken)
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (pg read-only) at %s\n",
			version,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (pg read-only) backend at %s, public at %s\n",
			version,
			rt.LocalURL,
			rt.PublicURL,
		)
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("pg serve: %v", err)
	}
}

// splitProjectList splits a comma-separated string into trimmed,
// non-empty project names.
func splitProjectList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
