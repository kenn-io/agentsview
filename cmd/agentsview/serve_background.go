package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
)

const backgroundServeReadyTimeout = 5 * time.Second

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
	lock := flock.New(backgroundLaunchLockPath(dataDir))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, false
	}
	return lock, true
}

// reportBackgroundLaunchInProgress waits for an in-flight startup to publish
// its runtime record and reports the running server, or notes that a launch
// is still in progress when no record appears in time. authToken may be empty
// for a contender that has not loaded config; a require_auth daemon then
// reports as in-progress rather than by URL.
func reportBackgroundLaunchInProgress(dataDir, authToken string) {
	WaitForDaemonStartup(dataDir, backgroundServeReadyTimeout, authToken)
	if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
		!rt.ReadOnly {
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
func runServeBackgroundCommand(cmd *cobra.Command) {
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

	runServeBackground(mustLoadConfig(cmd), os.Args[1:])
}

// runServeBackground generates the auth token, checks for an existing daemon,
// and spawns the detached child. The caller must already hold the background
// launch lock (see runServeBackgroundCommand). The launch lock is distinct
// from the daemon start lock so the spawned child can still claim the start
// lock during its own (possibly long) startup.
func runServeBackground(cfg config.Config, args []string) {
	if cfg.RequireAuth {
		if err := cfg.EnsureAuthToken(); err != nil {
			fatal("serve background: generating auth token: %v", err)
		}
		if cfg.AuthToken != "" {
			fmt.Printf("Auth enabled. Token: %s\n", cfg.AuthToken)
		}
	}

	if rt := FindDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil &&
		!rt.ReadOnly {
		fmt.Printf(
			"agentsview already running at %s (pid %d)\n",
			urlFromDaemonRuntime(rt),
			rt.Record.PID,
		)
		return
	}

	// A writable daemon (a foreground `serve` or a prior background launch)
	// is mid-startup and holds the start lock but has not yet published a
	// runtime record. Wait for it instead of racing a second server.
	if IsLocalDaemonActive(cfg.DataDir, cfg.AuthToken) {
		reportBackgroundLaunchInProgress(cfg.DataDir, cfg.AuthToken)
		return
	}

	child, logPath, err := startServeBackgroundProcess(cfg, args)
	if err != nil {
		fatal("serve background: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- child.Wait()
	}()

	rt, err := waitForBackgroundServeReady(
		cfg.DataDir,
		cfg.AuthToken,
		waitCh,
		backgroundServeReadyTimeout,
	)
	if err != nil {
		fatal(
			"serve background: server exited before becoming ready: %v\n"+
				"Logs: %s",
			err,
			logPath,
		)
	}
	if rt != nil {
		fmt.Printf(
			"agentsview running at %s (pid %d)\n",
			urlFromDaemonRuntime(rt),
			child.Process.Pid,
		)
		fmt.Printf("Logs: %s\n", logPath)
		return
	}

	fmt.Printf(
		"agentsview starting in background (pid %d)\n",
		child.Process.Pid,
	)
	fmt.Printf("Logs: %s\n", logPath)
}

func startServeBackgroundProcess(
	cfg config.Config,
	args []string,
) (*exec.Cmd, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("finding executable: %w", err)
	}
	logPath := filepath.Join(cfg.DataDir, "serve.log")
	// 0o600: the child writes its startup output here, which can include
	// auth details, so keep the log readable only by the owner.
	logFile, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if err != nil {
		return nil, "", fmt.Errorf("opening log file: %w", err)
	}
	defer logFile.Close()

	if _, err := fmt.Fprintf(
		logFile,
		"\n--- agentsview serve background start %s ---\n",
		time.Now().Format(time.RFC3339),
	); err != nil {
		return nil, "", fmt.Errorf("writing log header: %w", err)
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, "", fmt.Errorf("opening null device: %w", err)
	}
	defer devNull.Close()

	childArgs := serveBackgroundChildArgs(args)
	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), backgroundChildEnvVar+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureServeBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting server: %w", err)
	}
	return cmd, logPath, nil
}

func serveBackgroundChildArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if isBackgroundFlagArg(arg) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// isBackgroundFlagArg reports whether arg is the --background flag in any
// spelling the CLI accepts. The legacy flag normalizer rewrites the
// single-dash form -background to --background before Cobra parses, so the
// raw args handed to the child still carry -background. Stripping both
// spellings stops the child from re-entering background mode and spawning
// itself recursively.
func isBackgroundFlagArg(arg string) bool {
	for _, name := range []string{"--background", "-background"} {
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
	dataDir string,
	authToken string,
	waitCh <-chan error,
	timeout time.Duration,
) (*DaemonRuntime, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(startProbeTick)
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
		case <-ticker.C:
		case <-timer.C:
			return nil, nil
		}
	}
}
