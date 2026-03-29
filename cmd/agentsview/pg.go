package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/postgres"
	"github.com/wesm/agentsview/internal/server"
)

func runPG(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr,
			"usage: agentsview pg <push|status|serve>")
		os.Exit(1)
	}
	switch args[0] {
	case "push":
		runPGPush(args[1:])
	case "status":
		runPGStatus(args[1:])
	case "serve":
		runPGServe(args[1:])
	default:
		fmt.Fprintf(os.Stderr,
			"unknown pg command: %s\n", args[0])
		os.Exit(1)
	}
}

func runPGPush(args []string) {
	fs := flag.NewFlagSet("pg push", flag.ExitOnError)
	full := fs.Bool("full", false,
		"Force full local resync and PG push")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
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
	didResync := runLocalSync(appCfg, database, *full)
	forceFull := *full || didResync

	ps, err := postgres.New(
		pgCfg.URL, pgCfg.Schema, database,
		pgCfg.MachineName, pgCfg.AllowInsecure,
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
	result, err := ps.Push(ctx, forceFull)
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

func runPGStatus(args []string) {
	fs := flag.NewFlagSet("pg status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

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

func loadPGServeConfig(args []string) (config.Config, string, error) {
	fs := flag.NewFlagSet("pg serve", flag.ContinueOnError)
	basePath := fs.String("base-path", "",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)")
	config.RegisterServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return config.Config{}, "", fmt.Errorf("parsing flags: %w", err)
	}

	cfg, err := config.LoadPGServe(fs)
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, *basePath, nil
}

func runPGServe(args []string) {
	appCfg, basePath, err := loadPGServeConfig(args)
	if err != nil {
		log.Fatalf("%v", err)
	}
	setupLogFile(appCfg.DataDir)
	// Enable remote access with auth when binding to a
	// non-loopback address; keep it off for localhost.
	if !isLoopbackHost(appCfg.Host) {
		appCfg.RemoteAccess = true
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("pg serve: generating auth token: %v", err)
		}
	} else {
		appCfg.RemoteAccess = false
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

	store, err := postgres.NewStore(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		fatal("pg serve: %v", err)
	}
	defer store.Close()

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

	if rt.Cfg.RemoteAccess && rt.Cfg.AuthToken != "" {
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
