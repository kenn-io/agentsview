package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/trash"
)

// PruneRestoreConfig holds parsed options for `prune restore`.
type PruneRestoreConfig struct {
	Batch string
	Yes   bool
}

// pruneRestoreDeps carries the restore collaborators so tests can run
// against isolated stores and databases.
type pruneRestoreDeps struct {
	store  *trash.Store
	openDB func() (*db.DB, func(), error)
}

// runPruneRestore is the CLI entry point; it wires real dependencies
// and exits non-zero on failure.
func runPruneRestore(cfg PruneRestoreConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	deps := pruneRestoreDeps{
		store: trash.New(appCfg.DataDir),
		openDB: func() (*db.DB, func(), error) {
			return openWriteDB(context.Background(), appCfg)
		},
	}
	if err := pruneRestore(cfg, deps); err != nil {
		log.Fatalf("prune restore: %v", err)
	}
}

// pruneRestore moves the most recent (or requested) trash batch back
// to its original locations and un-excludes the archived rows so the
// next sync re-imports them.
func pruneRestore(cfg PruneRestoreConfig, deps pruneRestoreDeps) error {
	if !cfg.Yes {
		if cfg.Batch == "" {
			fmt.Print("Restore the most recently trashed batch" +
				" to its original locations? [y/N] ")
		} else {
			fmt.Printf(
				"Restore batch %s to its original locations? [y/N] ",
				cfg.Batch)
		}
		if !readConfirmation() {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var (
		restored []trash.Item
		err      error
	)
	if cfg.Batch == "" {
		restored, err = deps.store.RestoreLast()
	} else {
		restored, err = deps.store.RestoreBatch(cfg.Batch)
	}
	if err != nil {
		if errors.Is(err, trash.ErrNotFound) {
			return fmt.Errorf("nothing to restore: %w", err)
		}
		return err
	}

	sessionIDs := make([]string, 0, len(restored))
	for _, item := range restored {
		if item.SessionID != "" {
			sessionIDs = append(sessionIDs, item.SessionID)
		}
	}

	unexcluded := 0
	if len(sessionIDs) > 0 && deps.openDB != nil {
		database, closeFn, err := deps.openDB()
		if err != nil {
			fmt.Printf("Restored %d files (re-import enablement"+
				" skipped: %v).\n", len(restored), err)
			fmt.Println("Run \"agentsview sync\" to re-import them.")
			return nil
		}
		defer closeFn()
		unexcluded, err = database.UnexcludeSessions(sessionIDs)
		if err != nil {
			fmt.Printf("Restored %d files (un-excluding failed: %v).\n",
				len(restored), err)
			return nil
		}
	}

	fmt.Printf("Restored %d files to their original locations"+
		" (%d archive rows re-enabled).\n", len(restored), unexcluded)
	if len(sessionIDs) > 0 {
		fmt.Println("Run \"agentsview sync\" to re-import them.")
	}
	return nil
}

func readConfirmation() bool {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes"
}
