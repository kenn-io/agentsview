package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const (
	backgroundServeReadyTimeout     = 5 * time.Second
	backgroundAutoStartReadyTimeout = 90 * time.Second
)

var errServeStartupInProgress = errors.New(
	"agentsview serve startup is already in progress",
)

var startServeBackgroundProcessForEnsure = startServeBackgroundProcess
var startServeBackgroundProcessForRun = startServeBackgroundProcess

type backgroundLaunchPolicy struct {
	// ConfigOnly starts exclusively from persistent configuration. In
	// particular, NoSync is a CLI/runtime option rather than a config key.
	ConfigOnly bool
	Operation  string
}

type backgroundLaunchResult struct {
	Runtime  *DaemonRuntime
	Started  bool
	LogPath  string
	childPID int
}

func (p backgroundLaunchPolicy) operation() string {
	if p.Operation != "" {
		return p.Operation
	}
	return "serve background"
}

// backgroundChildEnvVar marks the re-exec'd serve process as the child of a
// background launch. The child reads it to keep the auth token out of
// serve.log; the parent prints the token to the invoking terminal instead.
const backgroundChildEnvVar = "AGENTSVIEW_BACKGROUND_CHILD"

// runningAsBackgroundChild reports whether this process was spawned by
// runServeBackground.
func runningAsBackgroundChild() bool {
	return os.Getenv(backgroundChildEnvVar) == "1"
}

// backgroundLaunchLockPath is the advisory lock that serializes concurrent
// `serve --background` launches for a data dir.
func backgroundLaunchLockPath(dataDir string) string {
	return filepath.Join(dataDir, "serve.background.lock")
}

// acquireBackgroundLaunchLock takes the background launch lock without
// blocking. ok is false when another launch already holds it.
func acquireBackgroundLaunchLock(dataDir string) (*flock.Flock, bool) {
	lock, acquired, _ := acquireBackgroundLaunchLockWithError(dataDir)
	return lock, acquired
}

// acquireBackgroundLaunchLockWithError distinguishes a lock held by another
// lifecycle operation from an I/O failure opening or acquiring the lock.
func acquireBackgroundLaunchLockWithError(
	dataDir string,
) (*flock.Flock, bool, error) {
	lock := flock.New(backgroundLaunchLockPath(dataDir))
	locked, err := lock.TryLock()
	locked, err = classifyBackgroundLaunchLockResult(locked, err)
	if err != nil {
		return nil, false, err
	}
	if !locked {
		return nil, false, nil
	}
	return lock, true, nil
}

func isBackgroundLaunchActive(dataDir string) bool {
	lock, ok := acquireBackgroundLaunchLock(dataDir)
	if ok {
		_ = lock.Unlock()
		return false
	}
	return true
}

// reportBackgroundLaunchInProgress waits for an in-flight startup to publish
// its runtime record and reports the running server, or notes that a launch
// is still in progress when no record appears in time. authToken may be empty
// for a contender that has not loaded config; a require_auth daemon then
// reports as in-progress rather than by URL.
func reportBackgroundLaunchInProgress(dataDir, authToken string) {
	waitForBackgroundLaunchOwner(
		context.Background(), dataDir, authToken, backgroundServeReadyTimeout,
	)
	if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
		!rt.ReadOnly && !shouldUpgradeDaemonRuntime(rt, version) {
		fmt.Printf(
			"agentsview already running at %s (pid %d)\n",
			urlFromDaemonRuntime(rt),
			rt.Record.PID,
		)
		return
	}
	fmt.Println("agentsview serve --background is already in progress.")
}

// runServeBackgroundCommand serializes the launch before loading config.
// Config loading writes config.toml (the cursor secret, and the auth token via
// EnsureAuthToken), so two concurrent launches that loaded config outside the
// lock could clobber each other's writes -- leaving the spawned server using a
// token the parent never printed. Holding the launch lock across both config
// load and token generation makes those writes single-writer.
func runServeBackgroundCommand(
	cmd *cobra.Command, opts serveReplacementOptions,
) {
	dataDir, err := config.ResolveDataDir()
	if err != nil {
		fatal("serve background: resolving data dir: %v", err)
	}
	// The launch lock lives under the data dir, which may not exist on first
	// run.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fatal("serve background: creating data dir: %v", err)
	}

	launchLock, ok := acquireBackgroundLaunchLock(dataDir)
	if !ok {
		// Another launch holds the lock and owns the config writes. Report
		// without loading config so this process never touches config.toml.
		reportBackgroundLaunchInProgress(dataDir, "")
		return
	}
	defer func() { _ = launchLock.Unlock() }()

	runServeBackground(mustLoadConfig(cmd), os.Args[1:], opts)
}

// runServeBackground preserves the serve --background CLI's fatal and output
// behavior around the reusable, error-returning background launch path.
func runServeBackground(
	cfg config.Config, args []string, opts serveReplacementOptions,
) {
	result, err := startServeBackground(
		cfg, args, opts, backgroundLaunchPolicy{},
	)
	if err != nil {
		fatal("%v", err)
	}
	if result.Runtime != nil && !result.Started {
		fmt.Printf(
			"agentsview already running at %s (pid %d)\n",
			urlFromDaemonRuntime(result.Runtime),
			result.Runtime.Record.PID,
		)
		return
	}
	if !result.Started {
		return
	}
	if result.Runtime != nil {
		fmt.Printf(
			"agentsview running at %s (pid %d)\n",
			urlFromDaemonRuntime(result.Runtime),
			result.childPID,
		)
		fmt.Printf("Logs: %s\n", result.LogPath)
		return
	}

	fmt.Printf(
		"agentsview starting in background (pid %d)\n",
		result.childPID,
	)
	fmt.Printf("Logs: %s\n", result.LogPath)
}

// startServeBackground generates the daemon auth token, checks for an existing
// writable daemon, and starts a detached child when needed. The caller must
// already hold the background launch lock. Unlike runServeBackground, this
// lower-level entry point returns launch failures for non-CLI callers.
func startServeBackground(
	cfg config.Config,
	args []string,
	opts serveReplacementOptions,
	policy backgroundLaunchPolicy,
) (backgroundLaunchResult, error) {
	var result backgroundLaunchResult
	operation := policy.operation()
	if policy.ConfigOnly {
		cfg.NoSync = false
	}
	replacementCheckStarted := time.Now()
	if err := ensureServeAuthToken(&cfg); err != nil {
		return result, fmt.Errorf(
			"%s: generating auth token: %w", operation, err,
		)
	}
	if cfg.RequireAuth {
		if cfg.AuthToken != "" {
			fmt.Println("Auth enabled. Token is configured.")
		}
	}
	if err := validateUniqueWritableDaemonSet(cfg.DataDir, cfg.AuthToken); err != nil {
		return result, fmt.Errorf("%s: %w", operation, err)
	}

	decision := decideServeDaemonReplacement(cfg, opts)
	switch decision.Action {
	case serveReplacementNone:
	case serveReplacementUseExisting:
		if rt := decision.Runtime; rt != nil {
			result.Runtime = rt
			result.childPID = rt.Record.PID
		}
		return result, nil
	case serveReplacementAuto, serveReplacementExplicit:
		waitedForExternalStartup := false
		if waited, err := waitForExternalServeStartupBeforeReplacement(
			context.Background(),
			cfg.DataDir,
			cfg.AuthToken,
			backgroundServeReadyTimeout,
		); waited {
			waitedForExternalStartup = true
			if err != nil {
				if errors.Is(err, errServeStartupInProgress) {
					fmt.Println(errServeStartupInProgress.Error() + ".")
					return result, nil
				}
				return result, fmt.Errorf("%s: %w", operation, err)
			}
		}
		decision = refreshServeDaemonReplacementDecision(
			cfg, opts, decision, waitedForExternalStartup,
			replacementCheckStarted,
		)
		switch decision.Action {
		case serveReplacementNone:
		case serveReplacementUseExisting:
			if rt := decision.Runtime; rt != nil {
				result.Runtime = rt
				result.childPID = rt.Record.PID
			}
			return result, nil
		case serveReplacementAuto, serveReplacementExplicit:
			// runServeBackgroundCommand holds the background launch lock across
			// this stop/start sequence, so another CLI launcher cannot race into
			// the replacement gap.
			if err := prepareBackgroundReplacement(
				&cfg, decision.Runtime, !policy.ConfigOnly,
			); err != nil {
				return result, fmt.Errorf("%s: %w", operation, err)
			}
			fmt.Println("Replacing agentsview daemon")
			for _, line := range serveDaemonReplacementLines(decision) {
				fmt.Println(line)
			}
			if err := stopDaemonRuntimeForUpgrade(cfg, decision.Runtime); err != nil {
				return result, fmt.Errorf(
					"%s: stopping daemon before restart: %w",
					operation, err,
				)
			}
		case serveReplacementRefuse:
			return result, fmt.Errorf(
				"%s: %s", operation,
				strings.Join(serveDaemonConflictLines(decision), "\n"),
			)
		default:
			return result, fmt.Errorf(
				"%s: unknown serve replacement action %d",
				operation, decision.Action,
			)
		}
	case serveReplacementRefuse:
		return result, fmt.Errorf(
			"%s: %s", operation,
			strings.Join(serveDaemonConflictLines(decision), "\n"),
		)
	default:
		return result, fmt.Errorf(
			"%s: unknown serve replacement action %d",
			operation, decision.Action,
		)
	}

	// A writable daemon (a foreground `serve` or a prior background launch)
	// is mid-startup and holds the start lock but has not yet published a
	// runtime record. Wait for it instead of racing a second server.
	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		reportBackgroundLaunchInProgress(cfg.DataDir, cfg.AuthToken)
		return result, nil
	}

	if policy.ConfigOnly {
		args = []string{"serve"}
	} else {
		args = serveBackgroundChildArgs(args)
	}
	args = serveBackgroundArgsWithNoSync(args, cfg.NoSync)
	child, logPath, err := startServeBackgroundProcessForRun(cfg, args)
	result.LogPath = logPath
	if err != nil {
		return result, fmt.Errorf("%s: %w", operation, err)
	}
	result.Started = true
	result.childPID = child.Process.Pid

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- child.Wait()
	}()

	rt, err := waitForBackgroundServeReady(
		context.Background(),
		cfg.DataDir,
		cfg.AuthToken,
		waitCh,
		backgroundServeReadyTimeout,
	)
	if err != nil {
		return result, fmt.Errorf(
			"%s: server exited before becoming ready: %w\nLogs: %s",
			operation, err, logPath,
		)
	}
	result.Runtime = rt
	return result, nil
}

func validateUniqueWritableDaemonSet(dataDir, authToken string) error {
	records, err := writableDaemonRecords(dataDir, authToken)
	if err != nil {
		return fmt.Errorf("inspecting writable daemon runtimes: %w", err)
	}
	if len(records) <= 1 {
		return nil
	}
	return fmt.Errorf(
		"multiple writable agentsview daemons are running (pids %s); refusing startup or replacement; run `agentsview daemon status`, then `agentsview daemon stop` before retrying",
		formatRecordPIDList(records),
	)
}

func prepareBackgroundReplacement(
	cfg *config.Config, rt *DaemonRuntime, adoptRuntimeOptions bool,
) error {
	if cfg == nil {
		return errors.New("nil replacement config")
	}
	if err := validateUniqueWritableDaemonSet(cfg.DataDir, cfg.AuthToken); err != nil {
		return err
	}
	if err := checkBackgroundReplacementDataVersion(cfg); err != nil {
		return err
	}
	if adoptRuntimeOptions {
		adoptDaemonRuntimeLaunchOptions(cfg, rt)
	}
	validationCfg := *cfg
	if validationCfg.Host == "" {
		// Config loading supplies the loopback default. Keep direct callers
		// and tests with a zero-value host aligned with that final child
		// configuration rather than treating an omitted host as non-loopback.
		validationCfg.Host = "127.0.0.1"
	}
	if err := validateServeConfig(validationCfg); err != nil {
		return fmt.Errorf("invalid serve configuration: %w", err)
	}
	return nil
}

func ensureBackgroundServe(
	ctx context.Context,
	cfg *config.Config,
	waitTimeout time.Duration,
) (*DaemonRuntime, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if waitTimeout <= 0 {
		waitTimeout = backgroundAutoStartReadyTimeout
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	var launchLock *flock.Flock
	for {
		var ok bool
		launchLock, ok = acquireBackgroundLaunchLock(cfg.DataDir)
		if ok {
			break
		}
		waitForBackgroundLaunchOwner(
			ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
		)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if cfg.AuthToken == "" {
			adoptBackgroundLaunchConfig(cfg)
		}
		if retryLock, retryOK := acquireBackgroundLaunchLock(
			cfg.DataDir,
		); retryOK {
			_ = retryLock.Unlock()
			continue
		}
		if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
			!rt.ReadOnly {
			if shouldUpgradeDaemonRuntime(rt, version) {
				return nil, fmt.Errorf(
					"agentsview serve --background is already in progress",
				)
			}
			return rt, nil
		}
		if _, err := findIncompatibleWritableDaemonRuntime(
			cfg.DataDir, cfg.AuthToken,
		); err != nil {
			return nil, fmt.Errorf(
				"incompatible daemon is already running: %w; run "+
					"`agentsview daemon stop` before starting this version",
				err,
			)
		}
		if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
			return nil, fmt.Errorf(
				"agentsview serve --background is already in progress",
			)
		}
		return nil, fmt.Errorf(
			"agentsview serve --background did not publish a runtime record",
		)
	}
	defer func() { _ = launchLock.Unlock() }()

	if err := ensureServeAuthToken(cfg); err != nil {
		return nil, fmt.Errorf("generating auth token: %w", err)
	}
	if err := validateUniqueWritableDaemonSet(cfg.DataDir, cfg.AuthToken); err != nil {
		return nil, err
	}

probeDaemon:
	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
		!rt.ReadOnly {
		if shouldUpgradeDaemonRuntime(rt, version) {
			if waited, err := waitForExternalServeStartupBeforeReplacement(
				ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
			); waited {
				if err != nil {
					return nil, err
				}
				goto probeDaemon
			}
			if serveReplacementTargetChanged(*cfg, rt) {
				goto probeDaemon
			}
			if err := prepareBackgroundReplacement(cfg, rt, true); err != nil {
				return nil, err
			}
			if err := stopDaemonRuntimeForUpgrade(*cfg, rt); err != nil {
				return nil, fmt.Errorf(
					"stopping older daemon before restart: %w",
					err,
				)
			}
		} else {
			return rt, nil
		}
	}
	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
		!rt.ReadOnly {
		return rt, nil
	}
	if rt, err := findIncompatibleWritableDaemonRuntime(
		cfg.DataDir, cfg.AuthToken,
	); err != nil {
		if rt != nil && shouldUpgradeIncompatibleDaemonRuntime(rt, version) {
			if waited, err := waitForExternalServeStartupBeforeReplacement(
				ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
			); waited {
				if err != nil {
					return nil, err
				}
				goto probeDaemon
			}
			if serveReplacementTargetChanged(*cfg, rt) {
				goto probeDaemon
			}
			if err := prepareBackgroundReplacement(cfg, rt, true); err != nil {
				return nil, err
			}
			if stopErr := stopDaemonRuntimeForUpgrade(*cfg, rt); stopErr != nil {
				return nil, fmt.Errorf(
					"stopping older daemon before restart: %w",
					stopErr,
				)
			}
		} else {
			return nil, fmt.Errorf(
				"incompatible daemon is already running: %w; run "+
					"`agentsview daemon stop` before starting this version",
				err,
			)
		}
	}
	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		WaitForDaemonStartupContext(
			ctx, cfg.DataDir, waitTimeout, cfg.AuthToken,
		)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
			!rt.ReadOnly {
			return rt, nil
		}
		stoppedUpgradeable := false
		if rt, err := findIncompatibleWritableDaemonRuntime(
			cfg.DataDir, cfg.AuthToken,
		); err != nil {
			if rt != nil && shouldUpgradeIncompatibleDaemonRuntime(rt, version) {
				if waited, err := waitForExternalServeStartupBeforeReplacement(
					ctx, cfg.DataDir, cfg.AuthToken, waitTimeout,
				); waited {
					if err != nil {
						return nil, err
					}
					goto probeDaemon
				}
				if serveReplacementTargetChanged(*cfg, rt) {
					goto probeDaemon
				}
				if err := prepareBackgroundReplacement(cfg, rt, true); err != nil {
					return nil, err
				}
				if stopErr := stopDaemonRuntimeForUpgrade(*cfg, rt); stopErr != nil {
					return nil, fmt.Errorf(
						"stopping older daemon before restart: %w",
						stopErr,
					)
				}
				stoppedUpgradeable = true
			} else {
				return nil, fmt.Errorf(
					"incompatible daemon is already running: %w; run "+
						"`agentsview daemon stop` before starting this version",
					err,
				)
			}
		}
		if !stoppedUpgradeable {
			return nil, errLocalDaemonUnreachable
		}
	}

	args := []string{"serve"}
	args = serveBackgroundArgsWithNoSync(args, cfg.NoSync)
	args = serveBackgroundArgsWithSkipInitialSync(args, cfg.SkipInitialSync)
	child, logPath, err := startServeBackgroundProcessForEnsure(*cfg, args)
	if err != nil {
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- child.Wait()
	}()
	rt, err := waitForBackgroundServeReady(
		ctx, cfg.DataDir, cfg.AuthToken, waitCh, waitTimeout,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"server exited before becoming ready: %w; logs: %s",
			err, logPath,
		)
	}
	if rt == nil {
		return nil, fmt.Errorf(
			"server did not become ready within %s; logs: %s",
			waitTimeout, logPath,
		)
	}
	return rt, nil
}

func waitForExternalServeStartup(
	ctx context.Context,
	dataDir string,
	authToken string,
	waitTimeout time.Duration,
) (*DaemonRuntime, bool, error) {
	if !isExternalDaemonStarting(dataDir) {
		return nil, false, nil
	}
	if waitTimeout <= 0 {
		waitTimeout = backgroundServeReadyTimeout
	}
	deadline := time.Now().Add(waitTimeout)
	for isExternalDaemonStarting(dataDir) {
		if err := ctx.Err(); err != nil {
			return nil, true, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, true, errServeStartupInProgress
		}
		wait := min(remaining, startProbeTick())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, true, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	if rt := FindDaemonRuntime(dataDir, authToken); rt != nil && !rt.ReadOnly {
		return rt, true, nil
	}
	if rt, err := findIncompatibleWritableDaemonRuntime(
		dataDir, authToken,
	); rt != nil && err != nil {
		return nil, true, fmt.Errorf(
			"incompatible daemon is already running: %w; run "+
				"`agentsview daemon stop` before starting this version",
			err,
		)
	}
	return nil, true, fmt.Errorf(
		"agentsview serve startup finished without publishing a writable " +
			"runtime record",
	)
}

func waitForExternalServeStartupBeforeReplacement(
	ctx context.Context,
	dataDir string,
	authToken string,
	waitTimeout time.Duration,
) (bool, error) {
	_, waited, err := waitForExternalServeStartup(
		ctx, dataDir, authToken, waitTimeout,
	)
	if !waited {
		return false, nil
	}
	if err != nil && (errors.Is(err, errServeStartupInProgress) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)) {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	return true, nil
}

func refreshServeDaemonReplacementDecision(
	cfg config.Config,
	opts serveReplacementOptions,
	original serveReplacementDecision,
	waitedForExternalStartup bool,
	replacementCheckStarted time.Time,
) serveReplacementDecision {
	if !opts.Replace {
		decision := decideServeDaemonReplacement(cfg, opts)
		if decision.Runtime == nil &&
			replacementTargetStillStopConfirmed(cfg, original.Runtime) {
			return original
		}
		return decision
	}
	decision := decideServeDaemonReplacement(
		cfg, serveReplacementOptions{},
	)
	if decision.Action == serveReplacementUseExisting &&
		!sameDaemonReplacementTarget(original.Runtime, decision.Runtime) {
		return decision
	}
	// A foreground startup may publish its runtime while still holding the
	// start lock. If that startup wins, reuse the daemon it just published
	// instead of treating --replace as permission to stop it.
	if waitedForExternalStartup &&
		decision.Action == serveReplacementUseExisting &&
		daemonRuntimeStartedAfter(decision.Runtime, replacementCheckStarted) {
		return decision
	}
	if decision.Runtime == nil &&
		replacementTargetStillStopConfirmed(cfg, original.Runtime) {
		return original
	}
	return decideServeDaemonReplacement(cfg, opts)
}

func daemonRuntimeStartedAfter(rt *DaemonRuntime, started time.Time) bool {
	return rt != nil &&
		!rt.Record.StartedAt.IsZero() &&
		rt.Record.StartedAt.After(started)
}

func serveReplacementTargetChanged(
	cfg config.Config, original *DaemonRuntime,
) bool {
	decision := decideServeDaemonReplacement(cfg, serveReplacementOptions{})
	if decision.Runtime == nil {
		return !replacementTargetStillStopConfirmed(cfg, original)
	}
	return !sameDaemonReplacementTarget(original, decision.Runtime)
}

func replacementTargetStillStopConfirmed(
	cfg config.Config, original *DaemonRuntime,
) bool {
	return original != nil && stopTargetConfirmed(original.Record, cfg.AuthToken)
}

func sameDaemonReplacementTarget(a, b *DaemonRuntime) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Record.PID == b.Record.PID &&
		a.Record.Address == b.Record.Address
}

func checkBackgroundReplacementDataVersion(cfg *config.Config) error {
	if cfg == nil || cfg.DBPath == "" {
		return nil
	}
	return db.CheckDataVersion(cfg.DBPath)
}

func waitForBackgroundLaunchOwner(
	ctx context.Context,
	dataDir string,
	authToken string,
	waitTimeout time.Duration,
) {
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if isExternalDaemonStarting(dataDir) {
			_, _, _ = waitForExternalServeStartup(
				ctx, dataDir, authToken, time.Until(deadline),
			)
			return
		}
		if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
			!rt.ReadOnly {
			return
		}
		if IsLocalDaemonActive(dataDir, authToken) {
			if WaitForDaemonStartupContext(
				ctx, dataDir, time.Until(deadline), authToken,
			) {
				if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
					!rt.ReadOnly {
					return
				}
			}
			if err := ctx.Err(); err != nil {
				return
			}
			launchLock, ok := acquireBackgroundLaunchLock(dataDir)
			if ok {
				_ = launchLock.Unlock()
				// The parent launch lock can clear before the child has
				// published its writable runtime. Keep waiting through that
				// handoff instead of treating a read-only mirror as success.
				if !IsDaemonStarting(dataDir) {
					return
				}
			}
		} else {
			launchLock, ok := acquireBackgroundLaunchLock(dataDir)
			if ok {
				_ = launchLock.Unlock()
				return
			}
		}
		wait := min(time.Until(deadline), startProbeTick())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func adoptBackgroundLaunchConfig(cfg *config.Config) {
	reloaded, err := config.LoadMinimal()
	if err != nil {
		return
	}
	if reloaded.DataDir != cfg.DataDir {
		return
	}
	cfg.RequireAuth = reloaded.RequireAuth
	cfg.AuthToken = reloaded.AuthToken
}

func startServeBackgroundProcess(
	cfg config.Config,
	args []string,
) (*exec.Cmd, string, error) {
	logPath := serveLogPath(cfg.DataDir)
	exe, err := os.Executable()
	if err != nil {
		return nil, logPath, fmt.Errorf("finding executable: %w", err)
	}
	// 0o600: the child writes its startup output here, which can include
	// auth details, so keep the log readable only by the owner.
	logFile, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if err != nil {
		return nil, logPath, fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()

	if _, err := fmt.Fprintf(
		logFile,
		"\n--- agentsview serve background start %s ---\n",
		time.Now().Format(time.RFC3339),
	); err != nil {
		return nil, logPath, fmt.Errorf("writing log header: %w", err)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, logPath, fmt.Errorf("opening null device: %w", err)
	}
	defer devNull.Close()

	childArgs := serveBackgroundChildArgs(args)
	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), backgroundChildEnvVar+"=1")
	if cfg.DataDir != "" {
		cmd.Env = append(cmd.Env, "AGENTSVIEW_DATA_DIR="+cfg.DataDir)
	}
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureServeBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, logPath, fmt.Errorf("starting server: %w", err)
	}
	return cmd, logPath, nil
}

func serveBackgroundChildArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if isBackgroundChildStrippedFlagArg(arg) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func adoptDaemonRuntimeLaunchOptions(cfg *config.Config, rt *DaemonRuntime) {
	if cfg == nil || rt == nil {
		return
	}
	if rt.NoSync {
		cfg.NoSync = true
	}
}

func serveBackgroundArgsWithNoSync(args []string, noSync bool) []string {
	if !noSync {
		return args
	}
	for _, arg := range args {
		for _, name := range []string{"--no-sync", "-no-sync"} {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return args
			}
		}
	}
	out := append([]string(nil), args...)
	return append(out, "--no-sync")
}

func serveBackgroundArgsWithSkipInitialSync(
	args []string, skipInitialSync bool,
) []string {
	if !skipInitialSync {
		return args
	}
	for _, arg := range args {
		if arg == "--skip-initial-sync" ||
			strings.HasPrefix(arg, "--skip-initial-sync=") {
			return args
		}
	}
	out := append([]string(nil), args...)
	return append(out, "--skip-initial-sync")
}

// isBackgroundChildStrippedFlagArg reports whether arg is a serve flag that
// belongs only to the launching parent. The legacy flag normalizer rewrites
// single-dash forms before Cobra parses, so raw child args still need both
// spellings stripped.
func isBackgroundChildStrippedFlagArg(arg string) bool {
	for _, name := range []string{
		"--background",
		"-background",
		"--replace",
		"-replace",
	} {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

// waitForBackgroundServeReady polls for the spawned child to publish a
// writable runtime record. It returns the runtime once ready, nil on timeout
// (the child is still starting), or an error if the child exits first.
func waitForBackgroundServeReady(
	ctx context.Context,
	dataDir string,
	authToken string,
	waitCh <-chan error,
	timeout time.Duration,
) (*DaemonRuntime, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(startProbeTick())
	defer ticker.Stop()

	for {
		if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
			!rt.ReadOnly {
			return rt, nil
		}

		select {
		case err := <-waitCh:
			if err == nil {
				err = fmt.Errorf("server process exited")
			}
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		case <-timer.C:
			return nil, nil
		}
	}
}
