package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/update"
)

type UpdateConfig struct {
	Check bool
	Yes   bool
	Force bool
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

	if err := update.PerformUpdate(info, progressFn); err != nil {
		fmt.Println()
		log.Fatalf("update failed: %v", err)
	}
}
