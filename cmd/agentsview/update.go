package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
	ExplicitPort     *int
	RequireAuth      bool
	RequireAuthKnown bool
	NoSync           bool
}

func runUpdate(ctx context.Context, cfg UpdateConfig) {
	dataDir, err := config.ResolveDataDir()
	if err != nil {
		log.Fatalf("resolving data dir: %v", err)
	}

	rollingRepo := update.RollingRepo()
	if rollingRepo != "" {
		fmt.Printf("Checking rolling builds from %s...\n", rollingRepo)
	}

	info, err := update.CheckForUpdate(ctx,
		version, forceUpdateCheck(cfg, rollingRepo), dataDir,
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

	info, err = installableUpdate(ctx, info,
		func(ctx context.Context) (*update.UpdateInfo, error) {
			return update.CheckForUpdate(ctx, version, true, dataDir)
		},
	)
	if err != nil {
		log.Fatalf("checking for updates: %v", err)
	}
	if info == nil {
		fmt.Println("Up to date.")
		return
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

	progress := newDownloadProgress(os.Stdout, time.Now)

	if err := performUpdateWithDaemonLifecycle(ctx,
		info,
		progress.update,
		loadDaemonConfigForUpdate,
		stopWritableDaemonsForUpdate,
		update.PerformUpdate,
		restartDaemonAfterUpdate,
	); err != nil {
		fmt.Println()
		log.Fatal(err)
	}
}

// forceUpdateCheck reports whether an explicit update command skips the
// update cache. Rolling builds always check fresh, because the rolling
// channel publishes on every push and a cached answer can be stale.
func forceUpdateCheck(cfg UpdateConfig, rollingRepo string) bool {
	return cfg.Force || rollingRepo != ""
}

// installableUpdate returns info with the download details an install
// needs. Display-only results, such as cached checks, are re-fetched
// with recheck, which returns nil when no update remains.
func installableUpdate(ctx context.Context,
	info *update.UpdateInfo,
	recheck func(context.Context) (*update.UpdateInfo, error),
) (*update.UpdateInfo, error) {
	if !info.NeedsRefetch() {
		return info, nil
	}
	return recheck(ctx)
}

func performUpdateWithDaemonLifecycle(ctx context.Context,
	info *update.UpdateInfo,
	progressFn func(downloaded, total int64),
	loadDaemonConfig func() (config.Config, error),
	stopDaemons func(context.Context, config.Config) (updateDaemonStopResult, error),
	perform func(context.Context, *update.UpdateInfo, func(int64, int64)) error,
	restartDaemon func(context.Context, config.Config, updateDaemonStopResult) error,
) error {
	daemonCfg, err := loadDaemonConfig()
	if err != nil {
		return fmt.Errorf("loading daemon config before update: %w", err)
	}
	stopResult, err := stopDaemons(ctx, daemonCfg)
	if err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(ctx, daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"stopping daemon before update: %w "+
						"(also failed to restart daemon: %v)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("stopping daemon before update: %w", err)
	}

	if err := perform(ctx, info, progressFn); err != nil {
		if stopResult.Stopped {
			if restartErr := restartDaemon(ctx, daemonCfg, stopResult); restartErr != nil {
				return fmt.Errorf(
					"update failed: %w (also failed to restart daemon: %w)",
					err, restartErr,
				)
			}
		}
		return fmt.Errorf("update failed: %w", err)
	}

	if stopResult.Stopped {
		if err := restartDaemon(ctx, daemonCfg, stopResult); err != nil {
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

func restartDaemonAfterUpdate(ctx context.Context,
	cfg config.Config, stopResult updateDaemonStopResult,
) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, restartDaemonAfterUpdateArgs(cfg, stopResult)...)
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
	if stopResult.ExplicitPort != nil {
		args = append(args, "--port", strconv.Itoa(*stopResult.ExplicitPort))
	} else if stopResult.Port > 0 {
		args = append(args, "--restart-port", strconv.Itoa(stopResult.Port))
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
