package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
			"agentsview already running at %s\n",
			urlFromDaemonRuntime(rt),
		)
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

	rt, ready, err := waitForBackgroundServeReady(
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
	if ready {
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

func waitForBackgroundServeReady(
	dataDir string,
	authToken string,
	waitCh <-chan error,
	timeout time.Duration,
) (*DaemonRuntime, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(startProbeTick)
	defer ticker.Stop()

	for {
		if rt := FindDaemonRuntime(dataDir, authToken); rt != nil &&
			!rt.ReadOnly {
			return rt, true, nil
		}

		select {
		case err := <-waitCh:
			if err == nil {
				err = fmt.Errorf("server process exited")
			}
			return nil, false, err
		case <-ticker.C:
		case <-timer.C:
			return nil, false, nil
		}
	}
}
