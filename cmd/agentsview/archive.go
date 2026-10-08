package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawarchive"
)

func newArchiveCommand() *cobra.Command {
	command := &cobra.Command{Use: "archive", Short: "Retain original session files, reparse them, and move a complete archive", GroupID: groupData}
	var captureRoots []string
	var identityFrom string
	var writersStopped bool
	captureCmd := &cobra.Command{Use: "capture DESTINATION", Short: "Copy source files and consistent databases into a new portable capture", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadReadOnly()
		if err != nil {
			return err
		}
		var roots []rawarchive.RootSpec
		for _, value := range captureRoots {
			provider, path, ok := strings.Cut(value, "=")
			if !ok || path == "" {
				return errors.New("--root requires PROVIDER=PATH")
			}
			roots = append(roots, rawarchive.RootSpec{Provider: provider, Path: path})
		}
		report, err := rawarchive.Capture(cmd.Context(), rawarchive.CaptureOptions{
			Destination: args[0], DataDir: cfg.DataDir, Roots: roots, IdentityFrom: identityFrom,
			WritersStopped: writersStopped, ReaderBuild: version, Progress: archiveProgress(cmd),
			Settings: rawarchive.RecoverySettings{ArchiveContent: cfg.ArchiveContent, ToolResultImages: cfg.ToolResultImages, LocalMachineName: cfg.LocalMachineName},
		})
		if err != nil {
			return err
		}
		return writeArchiveJSON(cmd.OutOrStdout(), report)
	}}
	captureCmd.Flags().StringArrayVar(&captureRoots, "root", nil, "Source root: claude=PATH, codex=PATH or files=PATH (repeatable, required)")
	captureCmd.Flags().StringVar(&identityFrom, "identity-from", "", "Previous capture.json whose generated identities should be reused")
	captureCmd.Flags().BoolVar(&writersStopped, "writers-stopped", false, "Record that the operator stopped source writers before capture")
	var specPath string
	var seed bool
	importCmd := &cobra.Command{Use: "import", Short: "Import an immutable capture with explicit original device and root identities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if specPath == "" {
			return errors.New("--spec is required")
		}
		if seed {
			cfg, err := config.LoadReadOnly()
			if err != nil {
				return err
			}
			report, err := rawarchive.Seed(cmd.Context(), specPath, cfg.DataDir, archiveProgress(cmd))
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		}
		spec, err := rawarchive.LoadImportSpec(cmd.Context(), specPath)
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
	importCmd.Flags().BoolVar(&seed, "seed", false, "Preserve the captured database and identity in a new data directory")
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
		applyClassifierConfig(cfg)
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
	backupCmd := &cobra.Command{Use: "backup REPOSITORY", Short: "Back up the stopped archive to a Docbank repository", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadReadOnly()
		if err != nil {
			return err
		}
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Backup(cmd.Context(), args[0], rawarchive.RecoverySettings{
				ArchiveContent: cfg.ArchiveContent, ToolResultImages: cfg.ToolResultImages, LocalMachineName: cfg.LocalMachineName,
			}, version)
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		})
	}}
	var restoreSnapshot string
	restoreCmd := &cobra.Command{Use: "restore REPOSITORY DESTINATION", Short: "Restore a selected snapshot into a new directory", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		report, err := rawarchive.Restore(cmd.Context(), args[0], restoreSnapshot, args[1], archiveProgress(cmd))
		return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
	}}
	restoreCmd.Flags().StringVar(&restoreSnapshot, "snapshot", "", "Snapshot ID from the backup report (required)")
	var verifyRepository, verifySnapshot string
	verifyCmd.Flags().StringVar(&verifyRepository, "repository", "", "Verify a backup repository instead of the local archive")
	verifyCmd.Flags().StringVar(&verifySnapshot, "snapshot", "", "Snapshot ID to verify in the backup repository (required with --repository)")
	verifyLocal := verifyCmd.RunE
	verifyCmd.RunE = func(cmd *cobra.Command, args []string) error {
		if verifyRepository == "" {
			if verifySnapshot != "" {
				return errors.New("--snapshot requires --repository")
			}
			return verifyLocal(cmd, args)
		}
		report, err := rawarchive.VerifyRecovery(cmd.Context(), verifyRepository, verifySnapshot)
		return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
	}
	var extractCapture string
	extractCmd := &cobra.Command{Use: "extract DESTINATION", Short: "Recover retained native files", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withRawArchive(cmd, func(a *rawarchive.Archive) error {
			report, err := a.Extract(cmd.Context(), args[0], extractCapture)
			return errors.Join(err, writeArchiveJSON(cmd.OutOrStdout(), report))
		})
	}}
	extractCmd.Flags().StringVar(&extractCapture, "capture", "", "Capture ID to recover as a portable capture directory")
	command.AddCommand(captureCmd, importCmd, reparseCmd, verifyCmd, sourcesCmd, backupCmd, restoreCmd, extractCmd)
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
	archiveOnly, err := db.ArchiveOnlyAt(cmd.Context(), cfg.DBPath)
	if err != nil {
		return err
	}
	if !archiveOnly {
		return rawarchive.ErrArchiveOnlyRequired
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
