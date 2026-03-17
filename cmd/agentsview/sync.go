// ABOUTME: CLI subcommand that syncs session data into the database
// ABOUTME: without starting the HTTP server.
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
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/pgsync"
	"github.com/wesm/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full     bool
	PG       bool
	PGStatus bool
}

func parseSyncFlags(args []string) (SyncConfig, error) {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	full := fs.Bool(
		"full", false,
		"Force a full resync (local) and bypass message skip heuristic (PG push)",
	)
	pg := fs.Bool(
		"pg", false,
		"Push to PostgreSQL now",
	)
	pgStatus := fs.Bool(
		"pg-status", false,
		"Show PG sync status",
	)

	if err := fs.Parse(args); err != nil {
		return SyncConfig{}, err
	}

	if fs.NArg() > 0 {
		return SyncConfig{}, fmt.Errorf(
			"unexpected arguments: %s",
			strings.Join(fs.Args(), " "),
		)
	}

	return SyncConfig{
		Full:     *full,
		PG:       *pg,
		PGStatus: *pgStatus,
	}, nil
}

func runSync(args []string) {
	cfg, err := parseSyncFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	setupLogFile(appCfg.DataDir)

	var database *db.DB
	database, err = db.Open(appCfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer database.Close()

	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(appCfg.CursorSecret)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		database.SetCursorSecret(secret)
	}

	if cfg.PG {
		// Run a local sync first so newly discovered sessions are
		// available for the PG push. This is best-effort: even if
		// local sync encounters errors, we proceed with the push
		// so the user can export existing data.
		runLocalSync(appCfg, database, cfg.Full)
		runPGSync(appCfg, database, cfg)
		return
	}

	if cfg.PGStatus {
		runPGSync(appCfg, database, cfg)
		return
	}

	runLocalSync(appCfg, database, cfg.Full)
}

func runPGSync(
	appCfg config.Config, database *db.DB, cfg SyncConfig,
) {
	pgCfg, err := appCfg.ResolvePGSync()
	if err != nil {
		fatal("pg sync: %v", err)
	}
	if pgCfg.PostgresURL == "" {
		fatal("pg sync: postgres_url not configured")
	}

	var interval time.Duration
	if cfg.PG {
		interval, err = time.ParseDuration(pgCfg.Interval)
		if err != nil {
			fatal("pg sync: invalid interval %q: %v",
				pgCfg.Interval, err)
		}
	} else {
		// Status-only path: interval is stored but unused.
		interval = 0
	}

	ps, err := pgsync.New(
		pgCfg.PostgresURL, database, pgCfg.MachineName,
		interval, pgCfg.AllowInsecurePG,
	)
	if err != nil {
		fatal("pg sync: %v", err)
	}
	defer ps.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var pushErrors int
	if cfg.PG {
		if err := ps.EnsureSchema(ctx); err != nil {
			fatal("pg sync schema: %v", err)
		}
		result, err := ps.Push(ctx, cfg.Full)
		if err != nil {
			fatal("pg sync push: %v", err)
		}
		fmt.Printf(
			"Pushed %d sessions, %d messages in %s\n",
			result.SessionsPushed,
			result.MessagesPushed,
			result.Duration.Round(time.Millisecond),
		)
		pushErrors = result.Errors
	}

	if cfg.PGStatus {
		status, err := ps.Status(ctx)
		if err != nil {
			fatal("pg sync status: %v", err)
		}
		fmt.Printf("Machine:     %s\n", status.Machine)
		fmt.Printf("Last push:   %s\n", valueOrNever(status.LastPushAt))
		fmt.Printf("PG sessions: %d\n", status.PGSessions)
		fmt.Printf("PG messages: %d\n", status.PGMessages)
	}

	if pushErrors > 0 {
		fatal("pg sync: %d session(s) failed to push", pushErrors)
	}
}

func runLocalSync(
	appCfg config.Config, database *db.DB, full bool,
) {
	for _, def := range parser.Registry {
		if !appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(
			appCfg.ResolveDirs(def.Type),
			string(def.Type),
		)
	}

	cleanResyncTemp(appCfg.DBPath)

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: appCfg.AgentDirs,
		Machine:   "local",
	})

	ctx := context.Background()
	if full || database.NeedsResync() {
		runInitialResync(ctx, engine)
	} else {
		runInitialSync(ctx, engine)
	}

	fmt.Println()
	stats, err := database.GetStats(context.Background(), false)
	if err == nil {
		fmt.Printf(
			"Database: %d sessions, %d messages\n",
			stats.SessionCount, stats.MessageCount,
		)
	}
}

func valueOrNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}
