// ABOUTME: Implements the canonical daemon lifecycle command.
// ABOUTME: Serializes writable-server start/stop transitions under one lock.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

type daemonLaunchLock interface {
	Unlock() error
}

type daemonLaunchObservation struct {
	LockHeld           bool
	Starting           bool
	Snapshot           *startupState
	Records            []daemon.RuntimeRecord
	UnconfirmedRecords []daemon.RuntimeRecord
	Err                error
}

type daemonLaunchWaitDeps struct {
	acquireLaunchLock  func(string) (*flock.Flock, bool, error)
	loadReadOnlyConfig func() (config.Config, error)
	writableRecords    func(string, string) ([]daemon.RuntimeRecord, error)
	confirmed          func(daemon.RuntimeRecord, string) bool
	probe              func(daemon.RuntimeRecord, string) bool
	isStarting         func(string) bool
	readStartupState   func(string) *startupState
	now                func() time.Time
	sleep              func(time.Duration)
	timeout            time.Duration
	tick               time.Duration
	onAttempt          func()
}

func defaultDaemonLaunchWaitDeps() daemonLaunchWaitDeps {
	return daemonLaunchWaitDeps{
		acquireLaunchLock:  acquireBackgroundLaunchLockWithError,
		loadReadOnlyConfig: config.LoadReadOnly,
		writableRecords:    writableDaemonRecords,
		confirmed:          stopTargetConfirmed,
		probe:              daemonRecordPingConfirmed,
		isStarting:         IsDaemonStarting,
		readStartupState:   readStartupState,
		now:                time.Now,
		sleep:              time.Sleep,
		timeout:            backgroundServeReadyTimeout,
		tick:               startProbeTick(),
	}
}

type daemonCommandDeps struct {
	resolveDataDir             func() (string, error)
	mkdirAll                   func(string, os.FileMode) error
	loadConfig                 func() (config.Config, error)
	loadReadOnlyConfig         func() (config.Config, error)
	acquireLaunchLockWithError func(string) (daemonLaunchLock, bool, error)
	waitContendedLaunch        func(string) daemonLaunchObservation
	writableRecords            func(string, string) ([]daemon.RuntimeRecord, error)
	statusRecords              func(string, string) ([]daemon.RuntimeRecord, error)
	isStarting                 func(string) bool
	readStartupState           func(string) *startupState
	startBackground            func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error)
	stopTargetConfirmed        func(daemon.RuntimeRecord, string) bool
	stopProcess                func(daemon.RuntimeRecord, time.Duration) error
	stopCaddy                  func(io.Writer, daemon.RuntimeRecord) error
	validateConfig             func(config.Config) error
	checkDataVersion           func(string) error
	probeRecord                func(daemon.RuntimeRecord, string) (daemon.PingInfo, bool)
	writableRuntime            func(string, string) *DaemonRuntime
	now                        func() time.Time
}

func defaultDaemonCommandDeps() daemonCommandDeps {
	return daemonCommandDeps{
		resolveDataDir:     config.ResolveDataDir,
		mkdirAll:           os.MkdirAll,
		loadConfig:         config.LoadMinimal,
		loadReadOnlyConfig: config.LoadReadOnly,
		acquireLaunchLockWithError: func(dataDir string) (daemonLaunchLock, bool, error) {
			return acquireBackgroundLaunchLockWithError(dataDir)
		},
		waitContendedLaunch: waitForDaemonLaunchContention,
		writableRecords:     writableDaemonRecords,
		statusRecords:       daemonStatusRecords,
		isStarting:          IsDaemonStarting,
		readStartupState:    readStartupState,
		startBackground:     startServeBackground,
		stopTargetConfirmed: stopTargetConfirmed,
		stopProcess:         stopDaemonProcess,
		stopCaddy:           stopOrphanedCaddyChildWithWriter,
		validateConfig:      validateServeConfig,
		checkDataVersion:    db.CheckDataVersion,
		probeRecord:         probeDaemonRecord,
		writableRuntime: func(dataDir, authToken string) *DaemonRuntime {
			return FindWritableDaemonRuntime(dataDir, authToken)
		},
		now: time.Now,
	}
}

func newDaemonCommand() *cobra.Command {
	return newDaemonCommandWithDeps(defaultDaemonCommandDeps())
}

func newDaemonCommandWithDeps(deps daemonCommandDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "daemon",
		Short:        "Manage the background server",
		GroupID:      groupCore,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "start", Short: "Start the background server",
			SilenceUsage: true, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runDaemonStart(cmd.OutOrStdout(), deps)
			},
		},
		&cobra.Command{
			Use: "status", Short: "Show background server status",
			SilenceUsage: true, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runDaemonStatus(cmd.OutOrStdout(), deps)
			},
		},
		&cobra.Command{
			Use: "stop", Short: "Stop the background server",
			SilenceUsage: true, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runDaemonStop(cmd.OutOrStdout(), deps)
			},
		},
		&cobra.Command{
			Use: "restart", Short: "Restart the background server",
			SilenceUsage: true, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runDaemonRestart(cmd.OutOrStdout(), deps)
			},
		},
	)
	return cmd
}

func prepareDaemonMutation(deps daemonCommandDeps) (string, error) {
	dataDir, err := deps.resolveDataDir()
	if err != nil {
		return "", fmt.Errorf("resolving data dir: %w", err)
	}
	if err := deps.mkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("creating data dir: %w", err)
	}
	return dataDir, nil
}

func runDaemonStart(w io.Writer, deps daemonCommandDeps) error {
	dataDir, err := prepareDaemonMutation(deps)
	if err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	launchLock, ok, err := deps.acquireLaunchLockWithError(dataDir)
	if err != nil {
		return fmt.Errorf("daemon start: acquiring launch lock: %w", err)
	}
	if !ok {
		return reportDaemonLaunchContention(w, dataDir, deps.waitContendedLaunch(dataDir), deps.now())
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon start: loading config: %w", err)
	}
	if err := validateLockedDataDir(dataDir, cfg.DataDir); err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	if deps.isStarting(cfg.DataDir) {
		return daemonPersistentStartupError("daemon start", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
	}
	result, err := deps.startBackground(
		cfg, []string{"serve"}, serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon start"},
	)
	if err != nil {
		return backgroundResultError(err, result)
	}
	if result.Started && result.Runtime == nil {
		return daemonSlowStartupError("daemon start", result)
	}
	if !result.Started && result.Runtime == nil {
		if deps.isStarting(cfg.DataDir) {
			return daemonPersistentStartupError("daemon start", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
		}
		return errors.New("daemon start: startup did not publish a writable runtime; inspect serve.log before retrying")
	}
	writeDaemonStartResult(w, result, false)
	return nil
}

func reportDaemonLaunchContention(
	w io.Writer, dataDir string, observation daemonLaunchObservation, now time.Time,
) error {
	if observation.Err != nil {
		return fmt.Errorf("daemon start: inspecting concurrent launch: %w", observation.Err)
	}
	if observation.LockHeld {
		if observation.Starting {
			return daemonPersistentStartupError("daemon start", dataDir, observation.Snapshot, now)
		}
		return fmt.Errorf(
			"daemon start: launch is still in progress under %s; retry later and verify the owning process manually if it persists",
			backgroundLaunchLockPath(dataDir),
		)
	}
	if len(observation.UnconfirmedRecords) > 0 {
		return unconfirmedWritableDaemonError(
			"daemon start", observation.UnconfirmedRecords,
		)
	}
	if len(observation.Records) > 1 {
		return fmt.Errorf(
			"daemon start: multiple writable agentsview daemons are running (pids %s); run `agentsview daemon status`, then `agentsview daemon stop` before retrying",
			formatRecordPIDList(observation.Records),
		)
	}
	if len(observation.Records) > 0 {
		rt := daemonRuntimeFromRecord(observation.Records[0])
		fmt.Fprintf(w, "agentsview already running at %s (pid %d)\n", urlFromDaemonRuntime(rt), rt.Record.PID)
		return nil
	}
	if observation.Starting {
		return daemonPersistentStartupError("daemon start", dataDir, observation.Snapshot, now)
	}
	return errors.New("daemon start: concurrent startup failed without publishing a writable runtime; inspect serve.log before retrying")
}

func waitForDaemonLaunchContention(dataDir string) daemonLaunchObservation {
	return waitForDaemonLaunchContentionWithDeps(
		dataDir, defaultDaemonLaunchWaitDeps(),
	)
}

func waitForDaemonLaunchContentionWithDeps(
	dataDir string, deps daemonLaunchWaitDeps,
) daemonLaunchObservation {
	deadline := deps.now().Add(deps.timeout)
	for {
		lock, acquired, err := deps.acquireLaunchLock(dataDir)
		if deps.onAttempt != nil {
			deps.onAttempt()
		}
		if err != nil {
			return daemonLaunchObservation{Err: err}
		}
		if acquired {
			cfg, configErr := deps.loadReadOnlyConfig()
			if configErr != nil {
				_ = lock.Unlock()
				return daemonLaunchObservation{
					Err: fmt.Errorf("loading read-only config: %w", configErr),
				}
			}
			if dataDirErr := validateLockedDataDir(dataDir, cfg.DataDir); dataDirErr != nil {
				_ = lock.Unlock()
				return daemonLaunchObservation{Err: dataDirErr}
			}
			records, recordsErr := deps.writableRecords(dataDir, cfg.AuthToken)
			if recordsErr != nil {
				_ = lock.Unlock()
				return daemonLaunchObservation{Err: recordsErr}
			}
			confirmed, unconfirmed := partitionConfirmedDaemonRecords(
				records, cfg.AuthToken, deps.confirmed,
			)
			if len(unconfirmed) == 0 {
				for _, rec := range confirmed {
					if !deps.probe(rec, cfg.AuthToken) {
						_ = lock.Unlock()
						return daemonLaunchObservation{Err: fmt.Errorf(
							"writable daemon pid %d is not responding to its health probe; run `agentsview daemon restart` to replace it",
							rec.PID,
						)}
					}
					if compatibilityErr := daemonRuntimeCompatibilityError(
						daemonRuntimeFromRecord(rec),
					); compatibilityErr != nil {
						_ = lock.Unlock()
						return daemonLaunchObservation{Err: fmt.Errorf(
							"writable daemon pid %d is incompatible: %w; run `agentsview daemon restart` to replace it",
							rec.PID, compatibilityErr,
						)}
					}
				}
			}
			starting := deps.isStarting(dataDir)
			var snapshot *startupState
			if starting {
				snapshot = deps.readStartupState(dataDir)
			}
			_ = lock.Unlock()
			return daemonLaunchObservation{
				Starting:           starting,
				Snapshot:           snapshot,
				Records:            confirmed,
				UnconfirmedRecords: unconfirmed,
			}
		}
		if deps.now().After(deadline) {
			starting := deps.isStarting(dataDir)
			var snapshot *startupState
			if starting {
				snapshot = deps.readStartupState(dataDir)
			}
			return daemonLaunchObservation{
				LockHeld: true, Starting: starting, Snapshot: snapshot,
			}
		}
		deps.sleep(deps.tick)
	}
}

func unconfirmedWritableDaemonError(
	operation string, records []daemon.RuntimeRecord,
) error {
	details := make([]string, 0, len(records))
	for _, rec := range records {
		detail := fmt.Sprintf("pid %d", rec.PID)
		if rec.SourcePath != "" {
			detail += " (runtime record " + rec.SourcePath + ")"
		}
		details = append(details, detail)
	}
	return fmt.Errorf(
		"%s: cannot confirm existing writable daemon identity: %s; refusing to launch another writer; verify each process and terminate it manually before retrying",
		operation, strings.Join(details, ", "),
	)
}

func daemonPersistentStartupError(
	operation, dataDir string, st *startupState, now time.Time,
) error {
	var details []string
	if st != nil {
		if st.PID > 0 {
			details = append(details, fmt.Sprintf("pid %d", st.PID))
		}
		if !st.StartedAt.IsZero() && !now.Before(st.StartedAt) {
			details = append(details, fmt.Sprintf("elapsed %s", now.Sub(st.StartedAt).Round(time.Second)))
		}
		if st.Phase != "" {
			details = append(details, "phase "+st.Phase)
		}
		if st.LogPath != "" {
			details = append(details, "log "+st.LogPath)
		}
	}
	detail := ""
	if len(details) > 0 {
		detail = " (" + strings.Join(details, ", ") + ")"
	}
	return fmt.Errorf(
		"%s: startup is still in progress%s; runtime publication may have failed; verify the process and terminate it manually before retrying (startup state: %s)",
		operation, detail, startupStatePath(dataDir),
	)
}

func writeDaemonStartResult(w io.Writer, result backgroundLaunchResult, restarted bool) {
	if result.Runtime != nil && !result.Started {
		fmt.Fprintf(w, "agentsview already running at %s (pid %d)\n", urlFromDaemonRuntime(result.Runtime), result.Runtime.Record.PID)
		return
	}
	verb := "running"
	if restarted {
		verb = "restarted"
	}
	if result.Runtime != nil {
		fmt.Fprintf(w, "agentsview %s at %s (pid %d)\n", verb, urlFromDaemonRuntime(result.Runtime), result.Runtime.Record.PID)
	} else {
		fmt.Fprintf(w, "agentsview %s in background (pid %d)\n", verb, result.childPID)
	}
	if result.LogPath != "" {
		fmt.Fprintf(w, "Logs: %s\n", result.LogPath)
	}
}

func backgroundResultError(err error, result backgroundLaunchResult) error {
	if result.LogPath != "" && !strings.Contains(err.Error(), result.LogPath) {
		return fmt.Errorf("%w\nLogs: %s", err, result.LogPath)
	}
	return err
}

func daemonSlowStartupError(
	operation string, result backgroundLaunchResult,
) error {
	details := []string{fmt.Sprintf("pid %d", result.childPID)}
	if result.LogPath != "" {
		details = append(details, "log "+result.LogPath)
	}
	return fmt.Errorf(
		"%s: startup is still in progress (%s); the child continues running; retry `agentsview daemon status`",
		operation, strings.Join(details, ", "),
	)
}

func validateLockedDataDir(locked, loaded string) error {
	if filepath.Clean(locked) == filepath.Clean(loaded) {
		return nil
	}
	return fmt.Errorf(
		"data dir changed after launch lock: locked %q, loaded %q",
		locked, loaded,
	)
}

func runDaemonStatus(w io.Writer, deps daemonCommandDeps) error {
	cfg, err := deps.loadReadOnlyConfig()
	if err != nil {
		return fmt.Errorf("daemon status: loading config: %w", err)
	}
	records, err := deps.statusRecords(cfg.DataDir, cfg.AuthToken)
	if err != nil {
		return fmt.Errorf("daemon status: inspecting runtime store: %w", err)
	}

	var writable []daemon.RuntimeRecord
	for _, rec := range records {
		if !daemonRuntimeFromRecord(rec).ReadOnly {
			writable = append(writable, rec)
		}
	}
	if len(writable) > 1 {
		fmt.Fprintln(w, "warning: multiple writable agentsview daemons are running; the single-writer invariant is violated.")
		for _, rec := range writable {
			writeDaemonRecordStatus(w, cfg, rec, deps)
		}
		return nil
	}
	if len(writable) == 1 {
		writeDaemonRecordStatus(w, cfg, writable[0], deps)
		return nil
	}
	if deps.writableRuntime != nil {
		if rt := deps.writableRuntime(cfg.DataDir, cfg.AuthToken); rt != nil {
			for _, line := range serveStatusLines(rt) {
				fmt.Fprintln(w, line)
			}
			return nil
		}
	}
	if deps.isStarting(cfg.DataDir) {
		fmt.Fprintln(w, "agentsview daemon is starting up.")
		for _, line := range serveStartingStatusLines(deps.readStartupState(cfg.DataDir), deps.now()) {
			fmt.Fprintln(w, line)
		}
		return nil
	}
	fmt.Fprintln(w, "No agentsview daemon is running.")
	return nil
}

func writeDaemonRecordStatus(
	w io.Writer, cfg config.Config, rec daemon.RuntimeRecord, deps daemonCommandDeps,
) {
	rt := daemonRuntimeFromRecord(rec)
	compatErr := daemonRuntimeCompatibilityError(rt)
	info, responding := deps.probeRecord(rec, cfg.AuthToken)
	if responding && info.Version != "" {
		rt.Record.Version = info.Version
	}
	if compatErr != nil {
		fmt.Fprintln(w, "agentsview found an incompatible running writable daemon.")
		for _, line := range serveStatusLines(rt) {
			fmt.Fprintln(w, line)
		}
		fmt.Fprintf(w, "  compatibility: %v\n", compatErr)
		if !responding {
			fmt.Fprintln(w, "  health: not responding to health checks")
		}
		if rec.SourcePath != "" {
			fmt.Fprintf(w, "  runtime: %s\n", rec.SourcePath)
		}
		return
	}
	if !responding {
		fmt.Fprintln(w, "agentsview daemon process is running but not responding to health checks.")
		for _, line := range serveStatusLines(rt) {
			fmt.Fprintln(w, line)
		}
		if rec.SourcePath != "" {
			fmt.Fprintf(w, "  runtime: %s\n", rec.SourcePath)
		}
		return
	}
	for _, line := range serveStatusLines(rt) {
		fmt.Fprintln(w, line)
	}
}

func runDaemonStop(w io.Writer, deps daemonCommandDeps) error {
	dataDir, err := prepareDaemonMutation(deps)
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	launchLock, ok, err := deps.acquireLaunchLockWithError(dataDir)
	if err != nil {
		return fmt.Errorf("daemon stop: acquiring launch lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("daemon stop: launch lock %s is busy; retry later", backgroundLaunchLockPath(dataDir))
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon stop: loading config: %w", err)
	}
	if err := validateLockedDataDir(dataDir, cfg.DataDir); err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	records, err := deps.writableRecords(cfg.DataDir, cfg.AuthToken)
	if err != nil {
		return fmt.Errorf("daemon stop: inspecting runtime store: %w", err)
	}
	if len(records) == 0 {
		if deps.writableRuntime != nil {
			if rt := deps.writableRuntime(cfg.DataDir, cfg.AuthToken); rt != nil {
				records = []daemon.RuntimeRecord{rt.Record}
			}
		}
	}
	if len(records) == 0 && deps.isStarting(cfg.DataDir) {
		return daemonPersistentStartupError("daemon stop", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
	}
	if len(records) == 0 {
		fmt.Fprintln(w, "agentsview daemon is not running.")
		return nil
	}
	return stopWritableDaemonRecordsSafely(w, cfg, records, daemonStopOperations{
		confirmed: deps.stopTargetConfirmed,
		stop:      deps.stopProcess,
		cleanup:   deps.stopCaddy,
	})
}

func runDaemonRestart(w io.Writer, deps daemonCommandDeps) error {
	dataDir, err := prepareDaemonMutation(deps)
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	launchLock, ok, err := deps.acquireLaunchLockWithError(dataDir)
	if err != nil {
		return fmt.Errorf("daemon restart: acquiring launch lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("daemon restart: launch lock %s is busy; retry later", backgroundLaunchLockPath(dataDir))
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon restart: loading config: %w", err)
	}
	if err := validateLockedDataDir(dataDir, cfg.DataDir); err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	if deps.isStarting(cfg.DataDir) {
		return daemonPersistentStartupError("daemon restart", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
	}
	if err := deps.validateConfig(cfg); err != nil {
		return fmt.Errorf("daemon restart: invalid config: %w", err)
	}
	if err := deps.checkDataVersion(cfg.DBPath); err != nil {
		return fmt.Errorf("daemon restart: checking data version: %w", err)
	}
	records, err := deps.writableRecords(cfg.DataDir, cfg.AuthToken)
	if err != nil {
		return fmt.Errorf("daemon restart: inspecting runtime store: %w", err)
	}
	wasRunning := len(records) > 0
	if wasRunning {
		if err := stopWritableDaemonRecordsSafely(w, cfg, records, daemonStopOperations{
			confirmed: deps.stopTargetConfirmed,
			stop:      deps.stopProcess,
			cleanup:   deps.stopCaddy,
		}); err != nil {
			return fmt.Errorf("daemon restart: %w", err)
		}
	}
	result, err := deps.startBackground(
		cfg, []string{"serve"}, serveReplacementOptions{},
		backgroundLaunchPolicy{ConfigOnly: true, Operation: "daemon restart"},
	)
	if err != nil {
		return backgroundResultError(err, result)
	}
	if result.Started && result.Runtime == nil {
		return daemonSlowStartupError("daemon restart", result)
	}
	if !result.Started && result.Runtime == nil {
		if deps.isStarting(cfg.DataDir) {
			return daemonPersistentStartupError("daemon restart", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
		}
		return errors.New("daemon restart: startup did not publish a writable runtime; inspect serve.log before retrying")
	}
	if wasRunning {
		writeDaemonStartResult(w, result, true)
	} else {
		fmt.Fprintln(w, "agentsview started (was not running).")
		if result.Runtime != nil {
			fmt.Fprintf(w, "agentsview running at %s (pid %d)\n", urlFromDaemonRuntime(result.Runtime), result.Runtime.Record.PID)
		}
		if result.LogPath != "" {
			fmt.Fprintf(w, "Logs: %s\n", result.LogPath)
		}
	}
	return nil
}

func daemonStatusRecords(
	dataDir string, authToken string,
) ([]daemon.RuntimeRecord, error) {
	migrateLegacyDaemonRuntimes(dataDir, authToken)
	store := runtimeStore(dataDir)
	if _, err := store.CleanupDead(); err != nil {
		return nil, fmt.Errorf("clean dead daemon runtime records: %w", err)
	}
	records, err := store.List()
	if err != nil {
		return nil, fmt.Errorf("list daemon runtime records: %w", err)
	}
	alive := make([]daemon.RuntimeRecord, 0, len(records))
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		state := processCreateTimeStateForPID(rec.PID, rec.Metadata[runtimeCreateTime])
		if state == processCreateTimeMismatch {
			if rec.SourcePath != "" {
				if err := os.Remove(rec.SourcePath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("remove mismatched daemon runtime record %s: %w", rec.SourcePath, err)
				}
			}
			continue
		}
		alive = append(alive, rec)
	}
	return alive, nil
}
