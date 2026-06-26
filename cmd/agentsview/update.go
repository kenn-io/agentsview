package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/update"
)

type UpdateConfig struct {
	Check bool
	Yes   bool
	Force bool
}

type updateDaemonStopResult struct {
	Stopped          bool
	Host             string
	Port             int
	RequireAuth      bool
	RequireAuthKnown bool
	NoSync           bool
}

func runUpdate(cfg UpdateConfig) {
	dataDir, err := config.ResolveDataDir()
	if err != nil {
		log.Fatalf("resolving data dir: %v", err)
	}

	info, err := update.CheckForUpdate(
		version, cfg.Force, dataDir,
	)
	if err != nil {
		log.Fatalf("checking for updates: %v", err)
	}

	if info == nil {
		fmt.Printf(
			"agentsview %s is up to date.\n", version,
		)
		return
	}

	if info.IsDevBuild {
		fmt.Printf(
			"Running dev build (%s). "+
				"Latest release: %s\n",
			info.CurrentVersion, info.LatestVersion,
		)
		if cfg.Check {
			return
		}
		// Cache-only results lack download metadata; re-fetch.
		if info.NeedsRefetch() {
			info, err = update.CheckForUpdate(
				version, true, dataDir,
			)
			if err != nil {
				log.Fatalf("checking for updates: %v", err)
			}
			if info == nil {
				fmt.Println("Up to date.")
				return
			}
		}
	} else {
		fmt.Printf(
			"Update available: %s -> %s",
			info.CurrentVersion, info.LatestVersion,
		)
		if info.Size > 0 {
			fmt.Printf(
				" (%s)", update.FormatSize(info.Size),
			)
		}
		fmt.Println()
		if cfg.Check {
			return
		}
	}

	if !cfg.Yes {
		fmt.Print("Install update? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Update cancelled.")
			return
		}
	}

	progressFn := func(downloaded, total int64) {
		if total > 0 {
			pct := float64(downloaded) / float64(total) * 100
			fmt.Printf(
				"\r  %s / %s (%.0f%%)",
				update.FormatSize(downloaded),
				update.FormatSize(total),
				pct,
			)
		}
	}

	if err := performUpdateWithDaemonLifecycle(
		info,
		progressFn,
		loadDaemonConfigForUpdate,
		stopWritableDaemonsForUpdate,
		update.PerformUpdate,
		restartDaemonAfterUpdate,
	); err != nil {
		fmt.Println()
		log.Fatal(err)
	}
}

func performUpdateWithDaemonLifecycle(
	info *update.UpdateInfo,
	progressFn func(downloaded, total int64),
	loadDaemonConfig func() (config.Config, error),
	stopDaemons func(config.Config) (updateDaemonStopResult, error),
	perform func(*update.UpdateInfo, func(int64, int64)) error,
	restartDaemon func(config.Config, updateDaemonStopResult) error,
) error {
	daemonCfg, err := loadDaemonConfig()
	if err != nil {
		return fmt.Errorf("loading daemon config before update: %w", err)
	}
	stopResult, err := stopDaemons(daemonCfg)
	if err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"stopping daemon before update: %w "+
						"(also failed to restart daemon: %v)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("stopping daemon before update: %w", err)
	}

	if err := perform(info, progressFn); err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"update failed: %w (also failed to restart daemon: %v)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("update failed: %w", err)
	}

	if stopResult.Stopped {
		if err := restartDaemon(daemonCfg, stopResult); err != nil {
			return fmt.Errorf("restarting daemon after update: %w", err)
		}
	}
	return nil
}

func loadDaemonConfigForUpdate() (config.Config, error) {
	cfg, err := config.LoadReadOnly()
	if err == nil {
		return cfg, nil
	}
	dataDir, dirErr := config.ResolveDataDir()
	if dirErr != nil {
		return cfg, err
	}
	return config.Config{DataDir: dataDir}, nil
}

func restartDaemonAfterUpdate(
	cfg config.Config, stopResult updateDaemonStopResult,
) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current executable: %w", err)
	}
	cmd := exec.Command(exe, restartDaemonAfterUpdateArgs(cfg, stopResult)...)
	cmd.Env = append(os.Environ(), "AGENTSVIEW_DATA_DIR="+cfg.DataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("serve --background: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func restartDaemonAfterUpdateArgs(
	cfg config.Config, stopResult updateDaemonStopResult,
) []string {
	args := []string{"serve", "--background"}
	if shouldPreserveUpdateRestartHost(cfg, stopResult) {
		args = append(args, "--host", stopResult.Host)
	} else if shouldForceLoopbackUpdateRestartHost(cfg, stopResult) {
		args = append(args, "--host", "127.0.0.1")
	}
	if stopResult.Port > 0 {
		args = append(args, "--port", fmt.Sprint(stopResult.Port))
	}
	if stopResult.RequireAuth ||
		(!stopResult.RequireAuthKnown && cfg.RequireAuth) {
		args = append(args, "--require-auth")
	}
	if stopResult.NoSync {
		args = append(args, "--no-sync")
	}
	return args
}

func shouldForceLoopbackUpdateRestartHost(
	cfg config.Config, stopResult updateDaemonStopResult,
) bool {
	if stopResult.Host == "" || isLoopbackHost(stopResult.Host) {
		return false
	}
	if stopResult.RequireAuthKnown {
		return !stopResult.RequireAuth
	}
	return !stopResult.RequireAuthKnown && !cfg.RequireAuth
}

func shouldPreserveUpdateRestartHost(
	cfg config.Config, stopResult updateDaemonStopResult,
) bool {
	if stopResult.Host == "" {
		return false
	}
	if isLoopbackHost(stopResult.Host) {
		return true
	}
	if stopResult.RequireAuthKnown {
		return stopResult.RequireAuth
	}
	return cfg.RequireAuth
}
