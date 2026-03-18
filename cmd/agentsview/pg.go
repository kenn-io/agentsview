package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
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

	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		fatal("pg push: %v", err)
	}
	if pgCfg.URL == "" {
		fatal("pg push: url not configured")
	}

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

func runPGServe(args []string) {
	fs := flag.NewFlagSet("pg serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1",
		"Host to bind to")
	port := fs.Int("port", 8080,
		"Port to listen on")
	basePath := fs.String("base-path", "",
		"URL prefix for reverse-proxy subpath (e.g. /agentsview)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	setupLogFile(appCfg.DataDir)

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

	if err := postgres.CheckSchemaCompat(
		ctx, store.DB(),
	); err != nil {
		fatal("pg serve: schema incompatible: %v", err)
	}

	appCfg.Host = *host
	// Enable remote access with auth when binding to a
	// non-loopback address; keep it off for localhost.
	if !isLoopbackHost(*host) {
		appCfg.RemoteAccess = true
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("pg serve: generating auth token: %v", err)
		}
		fmt.Printf("Auth token: %s\n", appCfg.AuthToken)
	} else {
		appCfg.RemoteAccess = false
	}
	appCfg.Port = server.FindAvailablePort(*host, *port)
	if appCfg.Port != *port {
		fmt.Printf("Port %d in use, using %d\n",
			*port, appCfg.Port)
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
	if *basePath != "" {
		opts = append(opts, server.WithBasePath(*basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)

	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- srv.ListenAndServe()
	}()
	if err := waitForLocalPort(
		ctx, appCfg.Host, appCfg.Port,
		5*time.Second, serveErrCh,
	); err != nil {
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		fatal("pg serve: server failed to start: %v", err)
	}

	fmt.Printf(
		"agentsview %s (pg read-only) at http://%s:%d\n",
		version, appCfg.Host, appCfg.Port,
	)

	select {
	case err := <-serveErrCh:
		if err != nil && err != http.ErrServerClosed {
			fatal("pg serve: server error: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil &&
			err != http.ErrServerClosed {
			fatal("pg serve: shutdown error: %v", err)
		}
	}
}
