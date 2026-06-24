// ABOUTME: `agentsview classifier rebuild` — clears the
// ABOUTME: stored classifier hash so the next db.Open runs a
// ABOUTME: full backfill. Recovery path for stale flags.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
)

func newClassifierCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "classifier",
		Short: "Manage the automated-session classifier",
		// Hidden because routine config.toml edits are auto-detected
		// on daemon restart via hash comparison; this group is a
		// recovery hatch (downgrade-then-upgrade, manual PG state
		// surgery) that most users never need.
		Hidden:       true,
		GroupID:      groupMeta,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newClassifierRebuildCommand())
	return cmd
}

func newClassifierRebuildCommand() *cobra.Command {
	var includePG bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Force is_automated re-backfill on next open",
		Long: "Clears the stored classifier hash so the next " +
			"db.Open runs a full is_automated backfill. " +
			"Use after editing [automated] prefixes in " +
			"config.toml or after a downgrade-then-upgrade " +
			"cycle that left flags stale. Pass --pg to also " +
			"clear the configured PostgreSQL sync store.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadPFlags(cmd.Flags())
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			applyClassifierConfig(cfg)
			tr, err := detectTransport(cfg.DataDir, cfg.AuthToken, 0)
			if err != nil {
				return err
			}
			if err := guardClassifierRebuild(tr); err != nil {
				return err
			}
			return runClassifierRebuild(
				cmd.Context(), cfg, cmd.OutOrStdout(), includePG,
			)
		},
	}
	cmd.Flags().BoolVar(
		&includePG, "pg", false,
		"also clear the configured PostgreSQL sync store",
	)
	return cmd
}

// guardClassifierRebuild rejects when the SQLite write lock
// is owned by a daemon we don't control. Pure function for
// testability.
func guardClassifierRebuild(tr transport) error {
	if tr.Mode == transportHTTP {
		return errors.New(
			"local daemon is serving on " + tr.URL +
				"; stop 'agentsview serve' (or 'pg serve') " +
				"before running 'classifier rebuild'",
		)
	}
	if tr.Mode == transportDirect && tr.DirectReadOnly {
		reason := tr.DirectReason
		if reason == "" {
			reason = "local daemon owns the SQLite archive but is not responding"
		}
		return errors.New(
			reason + "; refusing to rebuild to avoid competing for write ownership; " +
				"stop the daemon first",
		)
	}
	return nil
}

// runClassifierRebuild prints the loaded user-prefix list, deletes the
// classifier hash from SQLite stats, and optionally deletes it from PG
// sync_metadata. Returns an error on PG delete failure when PG cleanup is
// explicitly requested.
func runClassifierRebuild(
	ctx context.Context, cfg config.Config, out io.Writer, includePG bool,
) error {
	prefixes := db.UserAutomationPrefixes()
	fmt.Fprintf(out,
		"loaded %d user automation prefix(es) from config:\n",
		len(prefixes),
	)
	for _, p := range prefixes {
		fmt.Fprintf(out, "  - %s\n", p)
	}

	if err := rejectLiveWritableDaemonBeforeDirectWrite(cfg); err != nil {
		return fmt.Errorf("checking SQLite write ownership: %w", err)
	}

	writeLock, err := acquireWriteOwnerLock(ctx, writeLockDataDir(cfg))
	if err != nil {
		return fmt.Errorf("acquiring SQLite write-owner lock: %w", err)
	}
	defer func() { _ = writeLock.Close() }()

	if err := clearSQLiteClassifierHash(cfg.DBPath); err != nil {
		return fmt.Errorf("clearing SQLite hash: %w", err)
	}

	if includePG {
		pgCfg, err := cfg.ResolvePG()
		if err != nil {
			return fmt.Errorf("resolving pg config: %w", err)
		}
		if pgCfg.URL == "" {
			return errors.New(
				"pg url not configured; set AGENTSVIEW_PG_URL, use a legacy [pg].url, or configure default_pg with named [pg.NAME] targets",
			)
		}
		if err := clearPGClassifierHash(ctx, cfg, pgCfg); err != nil {
			return fmt.Errorf(
				"clearing PG hash: %w (SQLite was cleared "+
					"successfully; once PG is reachable, retry "+
					"'agentsview classifier rebuild', or run "+
					"'agentsview pg push --full' to repopulate "+
					"PG from the corrected SQLite side)",
				err,
			)
		}
	}

	fmt.Fprintln(out,
		"classifier hash cleared. Next db.Open will run "+
			"the is_automated backfill.")
	fmt.Fprintln(out,
		"restart any running 'agentsview serve' so write "+
			"paths use the updated prefixes")
	return nil
}

func clearSQLiteClassifierHash(dbPath string) error {
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		// Nothing to clear; first open will write the hash.
		return nil
	}
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Exec(
		`DELETE FROM stats WHERE key = ?`,
		db.ClassifierHashKey,
	)
	return err
}

// clearPGClassifierHash takes the full cfg so the static
// guardrail (Task 7) sees an applyClassifierConfig call in
// the same enclosing body as the postgres.Open trigger.
// The helper is silent and idempotent, so calling it again
// here on top of the RunE-closure call is harmless.
func clearPGClassifierHash(
	ctx context.Context, cfg config.Config, pgCfg config.PGConfig,
) error {
	applyClassifierConfig(cfg)
	pg, err := postgres.Open(
		pgCfg.URL, pgCfg.Schema, pgCfg.AllowInsecure,
	)
	if err != nil {
		return err
	}
	defer pg.Close()
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = $1`,
		db.ClassifierHashKey,
	)
	return err
}
