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

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/ssh"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full           bool
	Init           bool
	Watch          bool
	Debounce       time.Duration
	Interval       time.Duration
	Host           string
	User           string
	Port           int
	ArtifactFolder string
	// Token is the bearer token used for an http(s):// artifact peer target.
	Token string
	// GCGrace is the minimum age before superseded artifacts are auto-collected
	// after a folder sync. NoGC disables that automatic collection.
	GCGrace time.Duration
	NoGC    bool
	// CPUProfile, MemProfile, and Trace are hidden flags that capture a
	// pprof CPU profile, allocation snapshot, and runtime trace for the
	// sync pass. Empty strings disable each independently.
	CPUProfile string
	MemProfile string
	Trace      string
}

func applySyncArtifactTarget(cfg *SyncConfig, args []string, flagChanged bool) error {
	if len(args) == 0 {
		return nil
	}
	if flagChanged {
		return errors.New("artifact folder target cannot be provided both as an argument and --artifact-folder")
	}
	cfg.ArtifactFolder = args[0]
	return nil
}

func validateSyncConfig(cfg SyncConfig) error {
	if cfg.Init && cfg.Host != "" {
		return errors.New("--init cannot be combined with --host")
	}
	if cfg.Init && cfg.ArtifactFolder == "" {
		return errors.New(
			"--init requires an artifact folder target",
		)
	}
	if cfg.Watch && cfg.Host != "" {
		return errors.New("--watch cannot be combined with --host")
	}
	if cfg.Watch && cfg.ArtifactFolder == "" {
		return errors.New("--watch requires an artifact folder target")
	}
	if cfg.Host != "" && cfg.ArtifactFolder != "" {
		// SSH remote sync (--host) returns before artifact sync runs, so a
		// combined invocation would silently ignore the artifact target.
		return errors.New("--host cannot be combined with an artifact target")
	}
	return nil
}

func runSync(cfg SyncConfig) {
	hadRemoteFailures, err := doSync(cfg)
	if err != nil {
		fatal("sync: %v", err)
	}
	if hadRemoteFailures {
		os.Exit(1)
	}
}

// doSync performs the sync run and reports whether any configured remote host
// failed. It owns the deferred cleanup (profile stop, db close) so runSync can
// translate the result into a non-zero exit code without skipping that cleanup.
func doSync(cfg SyncConfig) (hadRemoteFailures bool, err error) {
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
		tr, err := syncTransport(&appCfg, cfg)
		if err != nil {
			fatal("detecting daemon: %v", err)
		}
		if tr.Mode == transportHTTP {
			useDaemon := useDaemonForSync(tr)
			if useDaemon && cfg.ArtifactFolder != "" {
				return false, fmt.Errorf(
					"artifact sync cannot run while a writable daemon owns the SQLite archive; " +
						"run `agentsview serve stop` and retry",
				)
			}
			if useDaemon && len(remoteHosts) > 0 {
				fmt.Println("Running sync with remotes via daemon...")
				onProgress, finishProgress := daemonProgressPrinter()
				failures, err := runDaemonRemoteSync(
					context.Background(), tr, appCfg.AuthToken,
					remoteHosts, cfg.Full, includeLocal, onProgress,
				)
				finishProgress()
				if err != nil {
					fatal("daemon remote sync: %v", err)
				}
				reportRemoteFailures(failures)
				return len(failures) > 0, nil
			}
			if useDaemon {
				start := time.Now()
				var onProgress sync.ProgressFunc
				var progress *resyncProgressPrinter
				if cfg.Full {
					fmt.Println("Running full resync via daemon...")
					progress = newResyncProgressPrinter(os.Stdout, time.Now)
					onProgress = progress.Print
				} else {
					fmt.Println("Running sync via daemon...")
					onProgress = printSyncProgress
				}
				stats, err := runDaemonSync(
					context.Background(), tr, appCfg.AuthToken, cfg.Full,
					onProgress,
				)
				if progress != nil {
					progress.Finish()
				}
				if err != nil {
					fatal("daemon sync: %v", err)
				}
				printSyncSummary(stats, start)
				return false, nil
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
		return false, nil
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
	if cfg.ArtifactFolder != "" {
		runArtifactFolderSync(appCfg, database, cfg.ArtifactFolder, cfg)
	}
	reportRemoteFailures(failures)
	return len(failures) > 0, nil
}

func syncTransport(appCfg *config.Config, cfg SyncConfig) (transport, error) {
	if cfg.ArtifactFolder != "" {
		return detectTransport(appCfg.DataDir, appCfg.AuthToken, 0)
	}
	return ensureTransport(appCfg, transportIntentArchiveWrite, 0)
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

func daemonProgressPrinter() (sync.ProgressFunc, func()) {
	var resyncProgress *resyncProgressPrinter
	var printedInline bool
	onProgress := func(p sync.Progress) {
		if p.Resync {
			if printedInline {
				fmt.Println()
				printedInline = false
			}
			if resyncProgress == nil {
				resyncProgress = newResyncProgressPrinter(os.Stdout, time.Now)
			}
			resyncProgress.Print(p)
			return
		}
		if resyncProgress != nil {
			resyncProgress.Finish()
			resyncProgress = nil
		}
		if formatSyncProgress(p) != "" {
			printedInline = true
		}
		printSyncProgress(p)
	}
	finish := func() {
		if resyncProgress != nil {
			resyncProgress.Finish()
			resyncProgress = nil
		}
		if printedInline {
			fmt.Println()
			printedInline = false
		}
	}
	return onProgress, finish
}

func runArtifactFolderSync(
	appCfg config.Config, database *db.DB, target string, cfg SyncConfig,
) {
	origin, err := appCfg.EnsureArtifactOriginID()
	if err != nil {
		fatal("artifact sync origin: %v", err)
	}
	ctx := context.Background()
	res, err := syncArtifactFolder(
		ctx, appCfg, database, target, origin, artifactPeerToken(cfg), nil,
	)
	if err != nil {
		fatal("artifact sync: %v", err)
	}
	printArtifactSyncSummary(res, cfg.Init)
	if !cfg.NoGC {
		autoGCAfterFolderSync(ctx, appCfg.DataDir, target, cfg.GCGrace)
	}
}

// artifactPeerToken resolves the bearer token for an HTTP peer target. Tokens
// are never inferred from local server auth because an explicit peer URL may
// point at an untrusted endpoint.
func artifactPeerToken(cfg SyncConfig) string {
	return cfg.Token
}

func syncArtifactFolder(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	target string,
	origin string,
	token string,
	onDataChanged func(),
) (artifact.SyncResult, error) {
	if !artifact.IsFolderTarget(target) && !artifact.IsHTTPTarget(target) && !artifact.IsObjectTarget(target) {
		return artifact.SyncResult{}, fmt.Errorf(
			"artifact sync supports local folder, http(s) peer, or s3:// object-store targets: %s",
			target,
		)
	}
	return artifact.Sync(ctx, database, artifact.SyncOptions{
		DataDir:       appCfg.DataDir,
		Target:        target,
		Origin:        origin,
		Token:         token,
		OnDataChanged: onDataChanged,
	})
}

func printArtifactSyncSummary(res artifact.SyncResult, init bool) {
	label := "Artifact sync"
	if init {
		label = "Artifact sync initialized"
	}
	fmt.Printf(
		"%s (%s): exported %d sessions, imported %d sessions / %d messages / %d metadata events\n",
		label, res.Origin, res.ExportedSessions, res.ImportedSessions, res.ImportedMessages, res.ImportedMetadata,
	)
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
	onProgress sync.ProgressFunc,
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
	return parseDaemonSyncSSE(resp.Body, onProgress)
}

func runDaemonRemoteSync(
	ctx context.Context,
	tr transport,
	authToken string,
	hosts []config.RemoteHost,
	full bool,
	includeLocal bool,
	onProgress sync.ProgressFunc,
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
	req.Header.Set("Accept", "text/event-stream")
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
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return parseDaemonRemoteSyncSSE(resp.Body, onProgress)
	}
	var out daemonRemoteSyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return remoteFailuresFromResponse(out), nil
}

type daemonRemoteSyncResponse struct {
	Failures []struct {
		Host config.RemoteHost `json:"host"`
		Err  string            `json:"error"`
	} `json:"failures"`
}

func remoteFailuresFromResponse(
	out daemonRemoteSyncResponse,
) []remoteHostFailure {
	failures := make([]remoteHostFailure, 0, len(out.Failures))
	for _, f := range out.Failures {
		failures = append(failures, remoteHostFailure{
			Host: f.Host,
			Err:  errors.New(f.Err),
		})
	}
	return failures
}

func parseDaemonRemoteSyncSSE(
	r io.Reader, onProgress sync.ProgressFunc,
) ([]remoteHostFailure, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var event string
	var data strings.Builder
	var lastNonDoneData string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			switch event {
			case "done":
				var out daemonRemoteSyncResponse
				if err := json.Unmarshal([]byte(data.String()), &out); err != nil {
					return nil, err
				}
				return remoteFailuresFromResponse(out), nil
			case "progress":
				if data.Len() > 0 {
					if err := reportDaemonSyncProgress(data.String(), onProgress); err != nil {
						return nil, err
					}
				}
			default:
				if data.Len() > 0 {
					lastNonDoneData = data.String()
				}
			}
			if event == "error" && data.Len() > 0 {
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
		return nil, err
	}
	if event == "progress" && data.Len() > 0 {
		if err := reportDaemonSyncProgress(data.String(), onProgress); err != nil {
			return nil, err
		}
	} else if event != "done" && data.Len() > 0 {
		lastNonDoneData = data.String()
	}
	if event == "done" && data.Len() > 0 {
		var out daemonRemoteSyncResponse
		if err := json.Unmarshal([]byte(data.String()), &out); err != nil {
			return nil, err
		}
		return remoteFailuresFromResponse(out), nil
	}
	if lastNonDoneData != "" {
		return nil, fmt.Errorf("daemon remote sync error: %s", lastNonDoneData)
	}
	return nil, fmt.Errorf("daemon remote sync response missing done event")
}

func parseDaemonSyncSSE(
	r io.Reader, progressFns ...sync.ProgressFunc,
) (sync.SyncStats, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var event string
	var data strings.Builder
	var lastNonDoneData string
	var onProgress sync.ProgressFunc
	if len(progressFns) > 0 {
		onProgress = progressFns[0]
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			switch event {
			case "done":
				var stats sync.SyncStats
				if err := json.Unmarshal(
					[]byte(data.String()), &stats,
				); err != nil {
					return sync.SyncStats{}, err
				}
				return stats, nil
			case "progress":
				if data.Len() > 0 {
					if err := reportDaemonSyncProgress(
						data.String(), onProgress,
					); err != nil {
						return sync.SyncStats{}, err
					}
				}
			default:
				if data.Len() > 0 {
					lastNonDoneData = data.String()
				}
			}
			if event == "error" && data.Len() > 0 {
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
	if event == "progress" && data.Len() > 0 {
		if err := reportDaemonSyncProgress(data.String(), onProgress); err != nil {
			return sync.SyncStats{}, err
		}
	} else if event != "done" && data.Len() > 0 {
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

func reportDaemonSyncProgress(raw string, onProgress sync.ProgressFunc) error {
	if onProgress == nil {
		return nil
	}
	var progress sync.Progress
	if err := json.Unmarshal([]byte(raw), &progress); err != nil {
		return fmt.Errorf("decoding daemon sync progress: %w", err)
	}
	onProgress(progress)
	return nil
}

func valueOrNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}
