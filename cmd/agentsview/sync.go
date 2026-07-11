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
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/ssh"
	"go.kenn.io/agentsview/internal/sync"
)

// SyncConfig holds parsed CLI options for the sync command.
type SyncConfig struct {
	Full         bool
	RepairMirror bool
	Host         string
	User         string
	Port         int
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
				fmt.Println("Running sync with remotes via daemon...")
				progress := newRemoteProgressPrinter(os.Stdout, time.Now)
				failures, err := runDaemonRemoteSyncWithRepair(
					context.Background(), tr, appCfg.AuthToken,
					remoteHosts, cfg.Full, cfg.RepairMirror, includeLocal, progress.Print,
				)
				progress.Finish()
				if err != nil {
					fatal("daemon remote sync: %v", err)
				}
				reportRemoteFailures(failures)
				return len(failures) > 0
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
			return runRemoteSyncOnceWithRepair(appCfg, database, rh, full, cfg.RepairMirror)
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

type remoteProgressPrinter struct {
	w        io.Writer
	now      func() time.Time
	label    string
	started  time.Time
	inPlace  bool
	finished bool
	progress sync.Progress
}

const remoteLocalSyncProgressLabel = "Syncing local sessions"

func newRemoteProgressPrinter(
	w io.Writer, now func() time.Time,
) *remoteProgressPrinter {
	return &remoteProgressPrinter{w: w, now: now}
}

func (p *remoteProgressPrinter) Print(progress sync.Progress) {
	if p.finished {
		return
	}
	label := strings.TrimSpace(progress.Detail)
	if progress.Phase == sync.PhaseDone {
		p.printFinalInPlaceProgress(progress)
		p.finishCurrent()
		return
	}
	if label == "" && progress.SessionsTotal > 0 &&
		progress.Phase == sync.PhaseSyncing {
		label = remoteLocalSyncProgressLabel
		progress.Detail = label
	}
	if label == "" {
		return
	}
	if strings.HasPrefix(label, "Synced ") {
		p.finishCurrent()
		fmt.Fprintf(p.w, "  %s\n", label)
		return
	}
	if progress.BytesDone > 0 || progress.BytesTotal > 0 {
		if p.label != label {
			p.finishCurrent()
			p.label = label
			p.started = p.now()
		}
		p.inPlace = true
		fmt.Fprintf(p.w, "\r  %s\x1b[K", formatSyncProgress(progress))
		return
	}
	if progress.Phase == sync.PhaseSyncing && progress.SessionsTotal > 0 {
		if p.label != label {
			p.finishCurrent()
			p.label = label
			p.started = p.now()
		}
		if strings.Contains(label, "Processing") {
			p.progress = progress
		}
		p.inPlace = true
		fmt.Fprintf(p.w, "\r  %s\x1b[K", formatSyncProgress(progress))
		return
	}
	if p.label == label {
		return
	}
	p.finishCurrent()
	p.label = label
	p.started = p.now()
	p.inPlace = false
	fmt.Fprintf(p.w, "  %s...\n", strings.TrimSuffix(label, "."))
}

func (p *remoteProgressPrinter) printFinalInPlaceProgress(
	progress sync.Progress,
) {
	if !p.inPlace || p.label == "" || progress.SessionsTotal == 0 {
		return
	}
	if progress.Detail == "" {
		progress.Detail = p.label
	}
	fmt.Fprintf(p.w, "\r  %s\x1b[K", formatSyncProgress(progress))
}

func (p *remoteProgressPrinter) Finish() {
	p.finished = true
	p.finishCurrent()
}

func (p *remoteProgressPrinter) finishCurrent() {
	if p.label == "" {
		return
	}
	if p.inPlace {
		fmt.Fprint(p.w, "\n")
	}
	elapsed := p.now().Sub(p.started).Round(time.Millisecond)
	throughput := ""
	if p.progress.SessionsDone > 0 && elapsed > 0 {
		throughput = fmt.Sprintf(", %.1f sessions/s", float64(p.progress.SessionsDone)/elapsed.Seconds())
	}
	fmt.Fprintf(p.w, "  %s completed in %s%s\n", p.label, elapsed, throughput)
	p.label = ""
	p.started = time.Time{}
	p.inPlace = false
	p.progress = sync.Progress{}
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
	if err := runRemoteSyncOnceWithRepair(
		appCfg, database, rh, cfg.Full, cfg.RepairMirror,
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
	return runRemoteSyncOnceWithRepair(appCfg, database, rh, full, false)
}

func runRemoteSyncOnceWithRepair(
	appCfg config.Config, database *db.DB,
	rh config.RemoteHost, full, repairMirror bool,
) error {
	if repairMirror && rh.Transport == config.RemoteTransportHTTP {
		_, err := runHTTPRemoteSyncWithRepair(context.Background(), appCfg, database, rh, full, true)
		return err
	}
	_, err := runRemoteSyncTransport(
		context.Background(), appCfg, database, rh, full,
	)
	return err
}

func runRemoteSyncTransport(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	rh config.RemoteHost,
	full bool,
) (remotesync.SyncStats, error) {
	switch rh.Transport {
	case "", config.RemoteTransportSSH:
		return runSSHRemoteSync(ctx, appCfg, database, rh, full)
	case config.RemoteTransportHTTP:
		return runHTTPRemoteSync(ctx, appCfg, database, rh, full)
	default:
		return remotesync.SyncStats{}, fmt.Errorf(
			"invalid remote transport %q", rh.Transport,
		)
	}
}

var runSSHRemoteSync = func(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	rh config.RemoteHost,
	full bool,
) (remotesync.SyncStats, error) {
	rs := &ssh.RemoteSync{
		Host:                    rh.Host,
		User:                    rh.User,
		Port:                    rh.Port,
		Full:                    full,
		DB:                      database,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	}
	return rs.Run(ctx)
}

var runHTTPRemoteSync = func(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	rh config.RemoteHost,
	full bool,
) (remotesync.SyncStats, error) {
	return runHTTPRemoteSyncWithRepair(ctx, appCfg, database, rh, full, false)
}

var runHTTPRemoteSyncWithRepair = func(
	ctx context.Context,
	appCfg config.Config,
	database *db.DB,
	rh config.RemoteHost,
	full, repairMirror bool,
) (remotesync.SyncStats, error) {
	token := rh.Token
	if token == "" {
		return remotesync.SyncStats{}, fmt.Errorf(
			"http remote sync token is required for host %q",
			rh.Host,
		)
	}
	return remotesync.HTTPSync{
		Host:                    rh.Host,
		URL:                     rh.URL,
		Token:                   token,
		Full:                    full,
		RepairMirror:            repairMirror,
		DataDir:                 appCfg.DataDir,
		DB:                      database,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	}.Run(ctx)
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
// even though setupLogFile redirects log output to a file. The log
// keeps the raw error; stderr gets the sanitized display form.
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
		fmt.Fprintf(os.Stderr, "  %s: %s\n",
			f.Host.Host, remoteFailureDisplay(f))
	}
}

// remoteFailureDisplay renders a remote failure for user-facing
// output. HTTP failures go through the sanitized summary because
// their raw errors can embed the remote URL, response bodies, or
// echoed tokens from a misbehaving endpoint; SSH errors are local
// tool output and stay verbatim.
func remoteFailureDisplay(f remoteHostFailure) string {
	if f.Host.Transport == config.RemoteTransportHTTP {
		return remotesync.FailureSummary(f.Err)
	}
	return f.Err.Error()
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
		IncludeCwdPrefixes:      appCfg.SyncIncludeCwdPrefixes,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})
	defer engine.Close()

	didResync := full || database.NeedsResync()
	// No startup state writer: this is a one-shot CLI sync, not a
	// daemon startup, so there is no start lock for status to read
	// under.
	if didResync {
		runInitialResync(ctx, engine, nil)
	} else {
		runInitialSync(ctx, engine, nil)
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
	return runDaemonRemoteSyncWithRepair(ctx, tr, authToken, hosts, full, false, includeLocal, onProgress)
}

func runDaemonRemoteSyncWithRepair(
	ctx context.Context,
	tr transport,
	authToken string,
	hosts []config.RemoteHost,
	full bool,
	repairMirror bool,
	includeLocal bool,
	onProgress sync.ProgressFunc,
) ([]remoteHostFailure, error) {
	body, err := json.Marshal(struct {
		Full         bool                `json:"full"`
		RepairMirror bool                `json:"repair_mirror,omitempty"`
		IncludeLocal bool                `json:"include_local"`
		Hosts        []config.RemoteHost `json:"hosts"`
	}{
		Full:         full,
		RepairMirror: repairMirror,
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
