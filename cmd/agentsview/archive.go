package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	syncer "go.kenn.io/agentsview/internal/sync"
)

func newArchiveCommand() *cobra.Command {
	command := &cobra.Command{Use: "archive", Short: "Retain original session files, reparse them, and move a complete archive", GroupID: groupData}
	var specPath string
	importCmd := &cobra.Command{Use: "import", Short: "Import an immutable capture with explicit original device and root identities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if specPath == "" {
			return errors.New("--spec is required")
		}
		spec, err := rawarchive.LoadImportSpec(specPath)
		if err != nil {
			return err
		}
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Import(cmd.Context(), spec)
			if printErr := writeArchiveJSON(cmd.OutOrStdout(), report); printErr != nil {
				return errors.Join(err, printErr)
			}
			if err == nil && len(report.Gaps) > 0 {
				return errors.New("capture retained with provider coverage gaps; see report")
			}
			return err
		})
	}}
	importCmd.Flags().StringVar(&specPath, "spec", "", "JSON capture root and original device mapping")
	var all bool
	var ids []string
	var budget int64
	reparseCmd := &cobra.Command{Use: "reparse", Short: "Reparse selected accepted sources and publish the complete batch atomically", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Reparse(cmd.Context(), rawarchive.ReparseOptions{All: all, ManifestIDs: ids, ScratchBytes: budget})
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		})
	}}
	reparseCmd.Flags().BoolVar(&all, "all", false, "Reparse all current accepted sources explicitly")
	reparseCmd.Flags().StringSliceVar(&ids, "manifest", nil, "Accepted manifest IDs to reparse")
	reparseCmd.Flags().Int64Var(&budget, "scratch-bytes", 16<<30, "Maximum materialized bytes per source; SQLite batch copy needs additional disk space")
	verifyCmd := &cobra.Command{Use: "verify", Short: "Read and verify every retained object and accepted manifest", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Verify(cmd.Context())
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		})
	}}
	sourcesCmd := &cobra.Command{Use: "sources", Short: "List accepted generations and parse status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadReadOnly()
		if err != nil {
			return err
		}
		// Source acceptance does not depend on the normalized parser version.
		database, err := db.OpenReadOnly(cmd.Context(), cfg.DBPath)
		if err != nil {
			return err
		}
		defer database.Close()
		after := ""
		for {
			page, err := database.ListRawArchiveSources(cmd.Context(), after, 128)
			if err != nil {
				return err
			}
			for _, source := range page {
				source.CanonicalJSON = nil
				if err := writeArchiveJSON(cmd.OutOrStdout(), source); err != nil {
					return err
				}
				after = source.ManifestID
			}
			if len(page) < 128 {
				return nil
			}
		}
	}}
	backupCmd := &cobra.Command{Use: "backup DESTINATION", Short: "Verify and copy the stopped archive, raw vault, assets, and configuration", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadReadOnly()
		if err != nil {
			return err
		}
		database, lock, err := openWriteDB(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer closeWriteDB(database, lock)
		archive, err := rawarchive.Open(cmd.Context(), database, cfg.DataDir, archiveProgress(cmd))
		if err != nil {
			return err
		}
		_, verifyErr := archive.Verify(cmd.Context())
		if err := errors.Join(verifyErr, archive.Close()); err != nil {
			return err
		}
		if err := database.CheckpointWALTruncate(cmd.Context()); err != nil {
			return err
		}
		if err := database.Close(); err != nil {
			return err
		}
		// Keep the writer lock while both storage owners remain closed.
		report, err := rawarchive.Backup(cmd.Context(), cfg.DBPath, cfg.DataDir, args[0])
		return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
	}}
	restoreCmd := &cobra.Command{Use: "restore BACKUP DESTINATION", Short: "Restore into a new directory and verify every accepted source", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		report, err := restoreRawArchive(cmd.Context(), args[0], args[1], archiveProgress(cmd))
		return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
	}}
	extractCmd := &cobra.Command{Use: "extract DESTINATION", Short: "Recover retained native files under their original root IDs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Extract(cmd.Context(), args[0])
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		})
	}}
	command.AddCommand(importCmd, reparseCmd, verifyCmd, sourcesCmd, backupCmd, restoreCmd, extractCmd)
	return command
}

func archiveProgress(cmd *cobra.Command) func(string) {
	return func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), message) }
}

func writeArchiveJSON(w io.Writer, value any) error {
	if err := json.MarshalWrite(w, value); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func withRawArchive(cmd *cobra.Command, run func(*rawarchive.Archive) error) (retErr error) {
	cfg, err := config.LoadReadOnly()
	if err != nil {
		return err
	}
	database, lock, err := openWriteDB(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer closeWriteDB(database, lock)
	archive, err := rawarchive.Open(cmd.Context(), database, cfg.DataDir, archiveProgress(cmd))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	return run(archive)
}

func restoreRawArchive(ctx context.Context, source, target string, progress func(string)) (report rawarchive.Report, retErr error) {
	if _, err := rawarchive.Restore(ctx, source, target); err != nil {
		return report, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(target, "sessions.db"))
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, database.Close()) }()
	var integrity string
	if err := database.Reader().QueryRow(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return report, err
	}
	if integrity != "ok" {
		return report, errors.New("restored SQLite archive failed integrity check")
	}
	archive, err := rawarchive.Open(ctx, database, target, progress)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	report, err = archive.Verify(ctx)
	if err != nil {
		return report, err
	}
	if database.NeedsResync() {
		// A restored archive has no live source obligation. Use the ordinary
		// rebuild's preserved-provider path so older data becomes readable while
		// archived content and identities carry forward without a provider parse.
		var disabled []parser.AgentType
		for _, def := range parser.Registry {
			disabled = append(disabled, def.Type)
		}
		if progress != nil {
			progress("Upgrading the restored database while preserving archived sessions")
		}
		engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{Ephemeral: true, DisabledAgents: disabled, DisableFilesystemProjectDiscovery: true})
		stats, buildErr := engine.ResyncAllWithOptions(ctx, nil, syncer.RebuildOptions{})
		if buildErr == nil && !stats.ArchiveRebuilt {
			buildErr = errors.New("restored archive rebuild was aborted")
		}
		engine.Close()
		if buildErr != nil {
			return report, buildErr
		}
	}
	return report, nil
}
