package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
	_ "time/tzdata"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/sync"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = ""
)

const (
	periodicSyncInterval  = 15 * time.Minute
	unwatchedPollInterval = 2 * time.Minute
	watcherDebounce       = 500 * time.Millisecond
	browserPollInterval   = 100 * time.Millisecond
	browserPollAttempts   = 60
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "prune":
			runPrune(os.Args[2:])
			return
		case "update":
			runUpdate(os.Args[2:])
			return
		case "serve":
			runServe(os.Args[2:])
			return
		case "sync":
			runSync(os.Args[2:])
			return
		case "version", "--version", "-v":
			fmt.Printf("agentsview %s (commit %s, built %s)\n",
				version, commit, buildDate)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}

	runServe(os.Args[1:])
}

func printUsage() {
	fmt.Printf(`agentsview %s - local web viewer for AI agent sessions

Syncs Claude Code, Codex, Copilot CLI, Gemini CLI, OpenCode, Cursor,
and Amp session data into SQLite, serves an analytics dashboard and
session browser via a local web UI.

Usage:
  agentsview [flags]          Start the server (default command)
  agentsview serve [flags]    Start the server (explicit)
  agentsview sync [flags]     Sync session data without serving
  agentsview prune [flags]    Delete sessions matching filters
  agentsview update [flags]   Check for and install updates
  agentsview version          Show version information
  agentsview help             Show this help

Server flags:
  -host string        Host to bind to (default "127.0.0.1")
  -port int           Port to listen on (default 8080)
  -no-browser         Don't open browser on startup

Sync flags:
  -full              Force a full resync regardless of data version

Prune flags:
  -project string     Sessions whose project contains this substring
  -max-messages int   Sessions with at most N messages (default -1)
  -before string      Sessions that ended before this date (YYYY-MM-DD)
  -first-message str  Sessions whose first message starts with this text
  -dry-run            Show what would be pruned without deleting
  -yes                Skip confirmation prompt

Update flags:
  -check              Check for updates without installing
  -yes                Install without confirmation prompt
  -force              Force check (ignore cache)

Environment variables:
  CLAUDE_PROJECTS_DIR     Claude Code projects directory
  CODEX_SESSIONS_DIR      Codex sessions directory
  COPILOT_DIR             Copilot CLI directory
  GEMINI_DIR              Gemini CLI directory
  OPENCODE_DIR            OpenCode data directory
  CURSOR_PROJECTS_DIR     Cursor projects directory
  AMP_DIR                 Amp threads directory
  AGENT_VIEWER_DATA_DIR   Data directory (database, config)

Multiple directories:
  Add arrays to ~/.agentsview/config.json to scan multiple locations:
  {
    "claude_project_dirs": ["/path/one", "/path/two"],
    "codex_sessions_dirs": ["/codex/a", "/codex/b"]
  }
  When set, these override the default directory. Environment variables
  override config file arrays.

Data is stored in ~/.agentsview/ by default.
`, version)
}

// warnMissingDirs prints a warning to stderr for each
// configured directory that does not exist or is
// inaccessible.
func warnMissingDirs(dirs []string, label string) {
	for _, d := range dirs {
		_, err := os.Stat(d)
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr,
				"warning: %s directory not found: %s\n",
				label, d,
			)
		} else {
			fmt.Fprintf(os.Stderr,
				"warning: %s directory inaccessible: %v\n",
				label, err,
			)
		}
	}
}

func runServe(args []string) {
	start := time.Now()
	cfg := mustLoadConfig(args)
	setupLogFile(cfg.DataDir)
	database := mustOpenDB(cfg)
	defer database.Close()

	for _, def := range parser.Registry {
		if !cfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(
			cfg.ResolveDirs(def.Type),
			string(def.Type),
		)
	}

	// Remove stale temp DB from a prior crashed resync.
	cleanResyncTemp(cfg.DBPath)

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs:               cfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
	})

	if database.NeedsResync() {
		runInitialResync(engine)
	} else {
		runInitialSync(engine)
	}

	stopWatcher, unwatchedDirs := startFileWatcher(cfg, engine)
	defer stopWatcher()

	go startPeriodicSync(engine)
	if len(unwatchedDirs) > 0 {
		go startUnwatchedPoll(engine)
	}

	port := server.FindAvailablePort(cfg.Host, cfg.Port)
	if port != cfg.Port {
		fmt.Printf("Port %d in use, using %d\n", cfg.Port, port)
	}
	cfg.Port = port

	srv := server.New(cfg, database, engine,
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}),
	)

	url := fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	fmt.Printf(
		"agentsview %s listening at %s (started in %s)\n",
		version, url,
		time.Since(start).Round(time.Millisecond),
	)

	if !cfg.NoBrowser {
		go openBrowser(url)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fatal("server error: %v", err)
	}
}

func mustLoadConfig(args []string) config.Config {
	fs := flag.NewFlagSet("agentsview", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(),
			"Usage: agentsview [serve] [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	config.RegisterServeFlags(fs)
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	cfg, err := config.Load(fs)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	return cfg
}

// maxLogSize is the threshold at which the debug log file is
// truncated on startup to prevent unbounded growth.
const maxLogSize = 10 * 1024 * 1024 // 10 MB

func setupLogFile(dataDir string) {
	logPath := filepath.Join(dataDir, "debug.log")
	truncateLogFile(logPath, maxLogSize)
	f, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644,
	)
	if err != nil {
		log.Printf("warning: cannot open log file: %v", err)
		return
	}
	log.SetOutput(f)
}

// truncateLogFile truncates the log file if it exceeds limit
// bytes. Symlinks are skipped to avoid truncating unrelated
// files. Errors are silently ignored since logging is
// best-effort.
func truncateLogFile(path string, limit int64) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Size() <= limit {
		return
	}
	_ = os.Truncate(path, 0)
}

func mustOpenDB(cfg config.Config) *db.DB {
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		fatal("opening database: %v", err)
	}

	if cfg.CursorSecret != "" {
		secret, err := base64.StdEncoding.DecodeString(cfg.CursorSecret)
		if err != nil {
			fatal("invalid cursor secret: %v", err)
		}
		database.SetCursorSecret(secret)
	}

	return database
}

// fatal prints a formatted error to stderr and exits.
// Use instead of log.Fatalf after setupLogFile redirects
// log output to the debug log file.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

// cleanResyncTemp removes leftover temp database files from
// a prior crashed resync.
func cleanResyncTemp(dbPath string) {
	tempPath := dbPath + "-resync"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(tempPath + suffix)
	}
}

func runInitialSync(engine *sync.Engine) {
	fmt.Println("Running initial sync...")
	t := time.Now()
	stats := engine.SyncAll(printSyncProgress)
	printSyncSummary(stats, t)
}

func runInitialResync(engine *sync.Engine) {
	fmt.Println("Data version changed, running full resync...")
	t := time.Now()
	stats := engine.ResyncAll(printSyncProgress)
	printSyncSummary(stats, t)

	// If resync was aborted (swap didn't happen), fall back
	// to a normal incremental sync so the server starts with
	// current file data rather than a potentially stale DB.
	if stats.Aborted {
		fmt.Println("Resync incomplete, running incremental sync...")
		t = time.Now()
		fallback := engine.SyncAll(printSyncProgress)
		printSyncSummary(fallback, t)
	}
}

func printSyncSummary(stats sync.SyncStats, t time.Time) {
	summary := fmt.Sprintf(
		"\nSync complete: %d sessions synced",
		stats.Synced,
	)
	if stats.OrphanedCopied > 0 {
		summary += fmt.Sprintf(
			", %d archived sessions preserved",
			stats.OrphanedCopied,
		)
	}
	if stats.Failed > 0 {
		summary += fmt.Sprintf(", %d failed", stats.Failed)
	}
	summary += fmt.Sprintf(
		" in %s\n", time.Since(t).Round(time.Millisecond),
	)
	fmt.Print(summary)
	for _, w := range stats.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

func printSyncProgress(p sync.Progress) {
	if p.SessionsTotal > 0 {
		fmt.Printf(
			"\r  %d/%d sessions (%.0f%%) · %d messages",
			p.SessionsDone, p.SessionsTotal,
			p.Percent(), p.MessagesIndexed,
		)
	}
}

func startFileWatcher(
	cfg config.Config, engine *sync.Engine,
) (stopWatcher func(), unwatchedDirs []string) {
	t := time.Now()
	onChange := func(paths []string) {
		engine.SyncPaths(paths)
	}
	watcher, err := sync.NewWatcher(watcherDebounce, onChange)
	if err != nil {
		log.Printf(
			"warning: file watcher unavailable: %v"+
				"; will poll every %s",
			err, unwatchedPollInterval,
		)
		return func() {}, []string{"all"}
	}

	type watchRoot struct {
		dir  string
		root string // actual path passed to WatchRecursive
	}

	var roots []watchRoot
	for _, def := range parser.Registry {
		if !def.FileBased {
			continue
		}
		for _, d := range cfg.ResolveDirs(def.Type) {
			if len(def.WatchSubdirs) == 0 {
				if _, err := os.Stat(d); err == nil {
					roots = append(
						roots, watchRoot{d, d},
					)
				}
				continue
			}
			for _, sub := range def.WatchSubdirs {
				watchDir := filepath.Join(d, sub)
				if _, err := os.Stat(watchDir); err == nil {
					roots = append(
						roots, watchRoot{d, watchDir},
					)
				}
			}
		}
	}

	var totalWatched int
	for _, r := range roots {
		watched, uw, _ := watcher.WatchRecursive(r.root)
		totalWatched += watched
		if uw > 0 {
			unwatchedDirs = append(unwatchedDirs, r.dir)
			log.Printf(
				"Couldn't watch %d directories under %s, will poll every %s",
				uw, r.dir, unwatchedPollInterval,
			)
		}
	}

	fmt.Printf(
		"Watching %d directories for changes (%s)\n",
		totalWatched, time.Since(t).Round(time.Millisecond),
	)
	watcher.Start()
	return watcher.Stop, unwatchedDirs
}

func startPeriodicSync(engine *sync.Engine) {
	ticker := time.NewTicker(periodicSyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Running scheduled sync...")
		engine.SyncAll(nil)
	}
}

func startUnwatchedPoll(engine *sync.Engine) {
	ticker := time.NewTicker(unwatchedPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Polling unwatched directories...")
		engine.SyncAll(nil)
	}
}

func openBrowser(url string) {
	for range browserPollAttempts {
		time.Sleep(browserPollInterval)
		resp, err := http.Get(url + "/api/v1/stats")
		if err == nil {
			resp.Body.Close()
			break
		}
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32",
			"url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Run()
}
