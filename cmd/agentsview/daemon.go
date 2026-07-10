// ABOUTME: Implements the canonical daemon lifecycle command.
// ABOUTME: Serializes writable-server start/stop transitions under one lock.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

type daemonLaunchLock interface {
	Unlock() error
}

type daemonLaunchObservation struct {
	LockHeld bool
	Starting bool
	Snapshot *startupState
	Records  []daemon.RuntimeRecord
	Err      error
}

type daemonCommandDeps struct {
	resolveDataDir      func() (string, error)
	mkdirAll            func(string, os.FileMode) error
	loadConfig          func() (config.Config, error)
	loadReadOnlyConfig  func() (config.Config, error)
	acquireLaunchLock   func(string) (daemonLaunchLock, bool)
	waitContendedLaunch func(string) daemonLaunchObservation
	writableRecords     func(string) ([]daemon.RuntimeRecord, error)
	statusRecords       func(string) ([]daemon.RuntimeRecord, error)
	isStarting          func(string) bool
	readStartupState    func(string) *startupState
	startBackground     func(config.Config, []string, serveReplacementOptions, backgroundLaunchPolicy) (backgroundLaunchResult, error)
	stopTargetConfirmed func(daemon.RuntimeRecord, string) bool
	stopProcess         func(daemon.RuntimeRecord, time.Duration) error
	stopCaddy           func(daemon.RuntimeRecord)
	checkDataVersion    func(string) error
	probeRecord         func(daemon.RuntimeRecord, string) bool
	now                 func() time.Time
}

func defaultDaemonCommandDeps() daemonCommandDeps {
	return daemonCommandDeps{
		resolveDataDir:     config.ResolveDataDir,
		mkdirAll:           os.MkdirAll,
		loadConfig:         config.LoadMinimal,
		loadReadOnlyConfig: config.LoadReadOnly,
		acquireLaunchLock: func(dataDir string) (daemonLaunchLock, bool) {
			return acquireBackgroundLaunchLock(dataDir)
		},
		waitContendedLaunch: waitForDaemonLaunchContention,
		writableRecords:     writableDaemonRecords,
		statusRecords:       daemonStatusRecords,
		isStarting:          IsDaemonStarting,
		readStartupState:    readStartupState,
		startBackground:     startServeBackground,
		stopTargetConfirmed: stopTargetConfirmed,
		stopProcess:         stopDaemonProcess,
		stopCaddy:           stopOrphanedCaddyChild,
		checkDataVersion:    db.CheckDataVersion,
		probeRecord: func(rec daemon.RuntimeRecord, token string) bool {
			return daemonRecordPingConfirmed(rec, token)
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
	launchLock, ok := deps.acquireLaunchLock(dataDir)
	if !ok {
		return reportDaemonLaunchContention(w, dataDir, deps.waitContendedLaunch(dataDir), deps.now())
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon start: loading config: %w", err)
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
	if len(observation.Records) > 0 {
		rt := daemonRuntimeFromRecord(observation.Records[0])
		fmt.Fprintf(w, "agentsview already running at %s (pid %d)\n", urlFromDaemonRuntime(rt), rt.Record.PID)
		return nil
	}
	if observation.Starting && observation.Snapshot != nil {
		return daemonPersistentStartupError("daemon start", dataDir, observation.Snapshot, now)
	}
	if observation.LockHeld {
		return fmt.Errorf(
			"daemon start: launch is still in progress under %s; retry later and verify the owning process manually if it persists",
			backgroundLaunchLockPath(dataDir),
		)
	}
	if observation.Starting {
		return daemonPersistentStartupError("daemon start", dataDir, observation.Snapshot, now)
	}
	return errors.New("daemon start: concurrent startup failed without publishing a writable runtime; inspect serve.log before retrying")
}

func waitForDaemonLaunchContention(dataDir string) daemonLaunchObservation {
	deadline := time.Now().Add(backgroundServeReadyTimeout)
	for {
		records, err := writableDaemonRecords(dataDir)
		if err != nil {
			return daemonLaunchObservation{Err: err}
		}
		starting := IsDaemonStarting(dataDir)
		var snapshot *startupState
		if starting {
			snapshot = readStartupState(dataDir)
		}
		lockHeld := isBackgroundLaunchActive(dataDir)
		observation := daemonLaunchObservation{
			LockHeld: lockHeld, Starting: starting, Snapshot: snapshot, Records: records,
		}
		if len(records) > 0 || !lockHeld || time.Now().After(deadline) {
			return observation
		}
		time.Sleep(startProbeTick())
	}
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

func runDaemonStatus(w io.Writer, deps daemonCommandDeps) error {
	cfg, err := deps.loadReadOnlyConfig()
	if err != nil {
		return fmt.Errorf("daemon status: loading config: %w", err)
	}
	records, err := deps.statusRecords(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("daemon status: inspecting runtime store: %w", err)
	}

	var writable, readOnly []daemon.RuntimeRecord
	for _, rec := range records {
		if daemonRuntimeFromRecord(rec).ReadOnly {
			readOnly = append(readOnly, rec)
		} else {
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
	if deps.isStarting(cfg.DataDir) {
		fmt.Fprintln(w, "agentsview daemon is starting up.")
		for _, line := range serveStartingStatusLines(deps.readStartupState(cfg.DataDir), deps.now()) {
			fmt.Fprintln(w, line)
		}
		return nil
	}
	if len(readOnly) > 0 {
		for _, rec := range readOnly {
			for _, line := range serveStatusLines(daemonRuntimeFromRecord(rec)) {
				fmt.Fprintln(w, line)
			}
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
	if !deps.probeRecord(rec, cfg.AuthToken) {
		fmt.Fprintln(w, "agentsview daemon process is running but not responding to health checks.")
		for _, line := range serveStatusLines(rt) {
			fmt.Fprintln(w, line)
		}
		if rec.SourcePath != "" {
			fmt.Fprintf(w, "  runtime: %s\n", rec.SourcePath)
		}
		return
	}
	if compatErr := daemonRuntimeCompatibilityError(rt); compatErr != nil {
		fmt.Fprintln(w, "agentsview found an incompatible running writable daemon.")
		for _, line := range serveStatusLines(rt) {
			fmt.Fprintln(w, line)
		}
		fmt.Fprintf(w, "  compatibility: %v\n", compatErr)
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
	launchLock, ok := deps.acquireLaunchLock(dataDir)
	if !ok {
		return fmt.Errorf("daemon stop: launch lock %s is busy; retry later", backgroundLaunchLockPath(dataDir))
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon stop: loading config: %w", err)
	}
	if deps.isStarting(cfg.DataDir) {
		return daemonPersistentStartupError("daemon stop", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
	}
	records, err := deps.writableRecords(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("daemon stop: inspecting runtime store: %w", err)
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
	launchLock, ok := deps.acquireLaunchLock(dataDir)
	if !ok {
		return fmt.Errorf("daemon restart: launch lock %s is busy; retry later", backgroundLaunchLockPath(dataDir))
	}
	defer func() { _ = launchLock.Unlock() }()

	cfg, err := deps.loadConfig()
	if err != nil {
		return fmt.Errorf("daemon restart: loading config: %w", err)
	}
	if deps.isStarting(cfg.DataDir) {
		return daemonPersistentStartupError("daemon restart", cfg.DataDir, deps.readStartupState(cfg.DataDir), deps.now())
	}
	if err := deps.checkDataVersion(cfg.DBPath); err != nil {
		return fmt.Errorf("daemon restart: checking data version: %w", err)
	}
	records, err := deps.writableRecords(cfg.DataDir)
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

func daemonStatusRecords(dataDir string) ([]daemon.RuntimeRecord, error) {
	migrateLegacyDaemonRuntimes(dataDir)
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
