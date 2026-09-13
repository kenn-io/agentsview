package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/trash"
)

// PruneRestoreConfig holds parsed options for `prune restore`.
type PruneRestoreConfig struct {
	Batch string
	Yes   bool
}

// runPruneRestore moves the most recent (or requested) trash batch
// back to its original locations and un-excludes the archived rows so
// the next sync re-imports them.
func runPruneRestore(cfg PruneRestoreConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	store := trash.New(appCfg.DataDir)
	if !cfg.Yes {
		if cfg.Batch == "" {
			fmt.Print("Restore the most recently trashed batch" +
				" to its original locations? [y/N] ")
		} else {
			fmt.Printf("Restore batch %s to its original locations? [y/N] ",
				cfg.Batch)
		}
		if !readConfirmation() {
			fmt.Println("Aborted.")
			return
		}
	}

	var restored []trash.Item
	if cfg.Batch == "" {
		restored, err = store.RestoreLast()
	} else {
		restored, err = store.RestoreBatch(cfg.Batch)
	}
	if err != nil {
		log.Fatalf("prune restore: %v", err)
	}

	sessionIDs := make([]string, 0, len(restored))
	for _, item := range restored {
		if item.SessionID != "" {
			sessionIDs = append(sessionIDs, item.SessionID)
		}
	}

	unexcluded := 0
	if len(sessionIDs) > 0 {
		database, writeLock, err := openWriteDB(context.Background(), appCfg)
		if err != nil {
			log.Printf("warning: re-import enablement skipped: %v", err)
		} else {
			unexcluded, err = database.UnexcludeSessions(sessionIDs)
			closeWriteDB(database, writeLock)
			if err != nil {
				log.Printf("warning: un-excluding sessions: %v", err)
			}
		}
	}

	fmt.Printf("Restored %d files to their original locations"+
		" (%d archive rows re-enabled).\n", len(restored), unexcluded)
	if len(sessionIDs) > 0 {
		fmt.Println("Run \"agentsview sync\" to re-import them.")
	}
}

func readConfirmation() bool {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}
