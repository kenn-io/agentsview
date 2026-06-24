// ABOUTME: CLI subcommand that syncs session data into the database
// ABOUTME: without starting the HTTP server.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/ssh"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full bool
	Host string
	User string
	Port int
	// CPUProfile, MemProfile, and Trace are hidden flags that capture a
	// pprof CPU profile, allocation snapshot, and runtime trace for the
	// sync pass. Empty strings disable each independently.
	CPUProfile string
	MemProfile string
	Trace      string
}

func runSync(cfg SyncConfig) {
	if doSync(cfg) {
		os.Exit(1)
	}
}

// doSync performs the sync run and reports whether any configured
// remote host failed. It owns the deferred cleanup (profile stop,
// db close) so runSync can translate the result into a non-zero
// exit code without skipping that cleanup.
func doSync(cfg SyncConfig) (hadRemoteFailures bool) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}

	setupLogFile(appCfg.DataDir)

	stopProfile := startSyncProfile(cfg)
	defer stopProfile()

	applyClassifierConfig(appCfg)
	var remoteHosts []config.RemoteHost
	includeLocal := cfg.Host == ""
	if cfg.Host == "" {
		remoteHosts = append(remoteHosts, appCfg.RemoteHosts...)
	} else {
		remoteHosts = append(remoteHosts, config.RemoteHost{
			Host: cfg.Host,
			User: cfg.User,
			Port: cfg.Port,
		})
	}
	if len(remoteHosts) > 0 {
		if err := (config.Config{RemoteHosts: remoteHosts}).ValidateRemoteHosts(); err != nil {
			fatal("invalid remote host: %v", err)
		}
	}

	if includeLocal || len(remoteHosts) > 0 {
		tr, err := ensureTransport(
			&appCfg, transportIntentArchiveWrite, 0,
		)
		if err != nil {
			fatal("detecting daemon: %v", err)
		}
		if tr.Mode == transportHTTP {
			useDaemon := useDaemonForSync(tr)
			if useDaemon && len(remoteHosts) > 0 {
				failures, err := runDaemonRemoteSync(
					context.Background(), tr, appCfg.AuthToken,
					remoteHosts, cfg.Full, includeLocal,
				)
				if err != nil {
					fatal("daemon remote sync: %v", err)
				}
				reportRemoteFailures(failures)
				return len(failures) > 0
			}
			if useDaemon {
				start := time.Now()
				stats, err := runDaemonSync(
					context.Background(), tr, appCfg.AuthToken, cfg.Full,
				)
				if err != nil {
					fatal("daemon sync: %v", err)
				}
				printSyncSummary(stats, start)
				return false
			}
			// Read-only mirror daemons do not own the local SQLite
			// archive. Remote sync can still proceed through the direct
			// path below, which will take the write-owner lock before
			// writing imported remote sessions.
		}
		if tr.DirectReadOnly {
			fatal(
				"local daemon owns the SQLite archive but is not " +
					"responding; refusing to sync directly",
			)
		}
	}

	database, writeLock, err := openWriteDB(context.Background(), appCfg)
	if err != nil {
		fatal("opening database: %v", err)
	}
	defer closeWriteDB(database, writeLock)

	if cfg.Host != "" {
		runRemoteSync(appCfg, database, cfg)
		return false
	}

	failures := syncLocalAndRemotes(
		appCfg.RemoteHosts, cfg.Full,
		func() bool {
			return runLocalSync(
				context.Background(), appCfg, database, cfg.Full,
			)
		},
		func(rh config.RemoteHost, full bool) error {
			return runRemoteSyncOnce(appCfg, database, rh, full)
		},
	)
	reportRemoteFailures(failures)
	return len(failures) > 0
}

func useDaemonForSync(tr transport) bool {
	if tr.Mode != transportHTTP {
		return false
	}
	if tr.ReadOnly {
		return false
	}
	return true
}

// syncLocalAndRemotes runs the local sync, then the configured
// remote hosts. A local resync (forced via --full or an automatic
// data-version resync) forces every remote sync full as well, so
// remote sessions are re-parsed rather than skipped via the remote
// skip cache. localSync and remoteSync are injected for testing;
// localSync returns whether a full resync was performed.
func syncLocalAndRemotes(
	hosts []config.RemoteHost, cfgFull bool,
	localSync func() bool,
	remoteSync func(config.RemoteHost, bool) error,
) []remoteHostFailure {
	didResync := localSync()
	full := cfgFull || didResync
	return runRemoteHosts(hosts, full, remoteSync)
}

func runRemoteSync(
	appCfg config.Config, database *db.DB, cfg SyncConfig,
) {
	rh := config.RemoteHost{
		Host: cfg.Host,
		User: cfg.User,
		Port: cfg.Port,
	}
	if err := runRemoteSyncOnce(
		appCfg, database, rh, cfg.Full,
	); err != nil {
		fatal("remote sync: %v", err)
	}
}

// runRemoteSyncOnce syncs a single remote host and returns any
// error instead of exiting, so it backs both the single-host
// --host path and the configured-hosts fan-out.
func runRemoteSyncOnce(
	appCfg config.Config, database *db.DB,
	rh config.RemoteHost, full bool,
) error {
	rs := &ssh.RemoteSync{
		Host:                    rh.Host,
		User:                    rh.User,
		Port:                    rh.Port,
		Full:                    full,
		DB:                      database,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	}
	_, err := rs.Run(context.Background())
	return err
}

// remoteHostFailure records a configured remote host that failed
// to sync. It keeps the full RemoteHost (not just the name) so
// duplicate hostnames that differ by user/port stay distinct.
type remoteHostFailure struct {
	Host config.RemoteHost
	Err  error
}

// runRemoteHosts syncs each configured host in declared order via
// syncFn, continuing past failures, and returns the collected
// failures. It performs no logging so it can be unit-tested
// without capturing the global logger; callers own all output.
func runRemoteHosts(
	hosts []config.RemoteHost, full bool,
	syncFn func(config.RemoteHost, bool) error,
) []remoteHostFailure {
	var failures []remoteHostFailure
	for _, rh := range hosts {
		if err := syncFn(rh, full); err != nil {
			failures = append(failures, remoteHostFailure{
				Host: rh,
				Err:  err,
			})
		}
	}
	return failures
}

// reportRemoteFailures writes per-host failures to the debug log
// and a summary to stderr, so unattended (cron) runs surface them
// even though setupLogFile redirects log output to a file.
func reportRemoteFailures(failures []remoteHostFailure) {
	if len(failures) == 0 {
		return
	}
	for _, f := range failures {
		log.Printf("remote sync %s failed: %v", f.Host.Host, f.Err)
	}
	fmt.Fprintf(os.Stderr,
		"sync: %d remote host(s) failed:\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", f.Host.Host, f.Err)
	}
}

// runLocalSync runs a local sync (incremental or full resync).
// It returns true if a full resync was performed, which callers
// can use to force a full PG push (watermarks become stale after
// a local resync).
func runLocalSync(
	ctx context.Context, appCfg config.Config, database *db.DB, full bool,
) bool {
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
		AgentDirs:               appCfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})

	didResync := full || database.NeedsResync()
	if didResync {
		runInitialResync(ctx, engine)
	} else {
		runInitialSync(ctx, engine)
	}
	engine.PhaseStats().Log("sync")

	fmt.Println()
	stats, err := database.GetStats(
		ctx, false, false,
	)
	if err == nil {
		fmt.Printf(
			"Database: %d sessions, %d messages\n",
			stats.SessionCount, stats.MessageCount,
		)
	}
	return didResync
}

func runDaemonSync(
	ctx context.Context,
	tr transport,
	authToken string,
	full bool,
) (sync.SyncStats, error) {
	endpoint := "/api/v1/sync"
	if full {
		endpoint = "/api/v1/resync"
	}
	baseURL := strings.TrimSuffix(tr.URL, "/")
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, baseURL+endpoint, nil,
	)
	if err != nil {
		return sync.SyncStats{}, err
	}
	req.Header.Set("Origin", baseURL)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sync.SyncStats{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return sync.SyncStats{}, fmt.Errorf(
			"HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)),
		)
	}
	if strings.HasPrefix(
		resp.Header.Get("Content-Type"), "application/json",
	) {
		var stats sync.SyncStats
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			return sync.SyncStats{}, err
		}
		return stats, nil
	}
	return parseDaemonSyncSSE(resp.Body)
}

func runDaemonRemoteSync(
	ctx context.Context,
	tr transport,
	authToken string,
	hosts []config.RemoteHost,
	full bool,
	includeLocal bool,
) ([]remoteHostFailure, error) {
	body, err := json.Marshal(struct {
		Full         bool                `json:"full"`
		IncludeLocal bool                `json:"include_local"`
		Hosts        []config.RemoteHost `json:"hosts"`
	}{
		Full:         full,
		IncludeLocal: includeLocal,
		Hosts:        hosts,
	})
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSuffix(tr.URL, "/")
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		baseURL+"/api/v1/sync/remotes",
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", baseURL)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)),
		)
	}
	var out struct {
		Failures []struct {
			Host config.RemoteHost `json:"host"`
			Err  string            `json:"error"`
		} `json:"failures"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	failures := make([]remoteHostFailure, 0, len(out.Failures))
	for _, f := range out.Failures {
		failures = append(failures, remoteHostFailure{
			Host: f.Host,
			Err:  errors.New(f.Err),
		})
	}
	return failures, nil
}

func parseDaemonSyncSSE(r io.Reader) (sync.SyncStats, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var event string
	var data strings.Builder
	var lastNonDoneData string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event == "done" {
				var stats sync.SyncStats
				if err := json.Unmarshal(
					[]byte(data.String()), &stats,
				); err != nil {
					return sync.SyncStats{}, err
				}
				return stats, nil
			}
			if data.Len() > 0 {
				lastNonDoneData = data.String()
			}
			event = ""
			data.Reset()
			continue
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
			continue
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return sync.SyncStats{}, err
	}
	if event != "done" && data.Len() > 0 {
		lastNonDoneData = data.String()
	}
	if event == "done" && data.Len() > 0 {
		var stats sync.SyncStats
		if err := json.Unmarshal([]byte(data.String()), &stats); err != nil {
			return sync.SyncStats{}, err
		}
		return stats, nil
	}
	if lastNonDoneData != "" {
		return sync.SyncStats{}, fmt.Errorf(
			"daemon sync error: %s", lastNonDoneData,
		)
	}
	return sync.SyncStats{}, fmt.Errorf("daemon sync response missing done event")
}

func valueOrNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}
