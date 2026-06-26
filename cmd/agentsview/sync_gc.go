package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/artifact"
)

const defaultArtifactGCGrace = 7 * 24 * time.Hour

// SyncGCConfig holds parsed options for artifact sync garbage collection.
type SyncGCConfig struct {
	Target string
	Grace  time.Duration
	DryRun bool
}

func newSyncGCCommand() *cobra.Command {
	var cfg SyncGCConfig
	cfg.Grace = defaultArtifactGCGrace
	cmd := &cobra.Command{
		Use:   "gc <artifact-folder>",
		Short: "Garbage collect superseded sync artifacts",
		Long: "Garbage collect superseded artifact sync files from a local\n" +
			"artifact folder. GC keeps the latest checkpoint per origin,\n" +
			"keeps every manifest, segment, and raw artifact reachable from\n" +
			"that checkpoint, and only removes unreferenced files older than\n" +
			"the grace window. Origins without checkpoints are skipped.",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Target = args[0]
			return runSyncGC(cmd, cfg)
		},
	}
	cmd.Flags().DurationVar(
		&cfg.Grace,
		"grace",
		defaultArtifactGCGrace,
		"Minimum age before unreferenced artifacts can be deleted",
	)
	cmd.Flags().BoolVar(
		&cfg.DryRun,
		"dry-run",
		false,
		"Log artifacts that would be deleted without removing them",
	)
	return cmd
}

func runSyncGC(cmd *cobra.Command, cfg SyncGCConfig) error {
	if !artifact.IsFolderTarget(cfg.Target) {
		return fmt.Errorf(
			"artifact gc currently supports local folder targets only: %s",
			cfg.Target,
		)
	}
	res, err := artifact.GarbageCollect(cmd.Context(), artifact.GCOptions{
		Root:   cfg.Target,
		Grace:  cfg.Grace,
		DryRun: cfg.DryRun,
		Logf:   log.Printf,
	})
	if err != nil {
		return fmt.Errorf("artifact gc: %w", err)
	}
	printArtifactGCSummary(cmd.OutOrStdout(), res)
	return nil
}

// autoGCAfterFolderSync garbage-collects superseded artifacts from the local
// store and the shared folder target after a successful folder sync. Both are
// collected with the same synced checkpoint view so a later set-union exchange
// cannot re-propagate the deleted files. GC is opportunistic: failures are
// logged, not fatal. Non-folder targets (a remote peer) collect themselves, so
// only the local store is collected for those.
func autoGCAfterFolderSync(ctx context.Context, dataDir, target string, grace time.Duration) {
	if grace <= 0 {
		grace = defaultArtifactGCGrace
	}
	roots := []string{filepath.Join(dataDir, "artifacts")}
	if artifact.IsFolderTarget(target) {
		roots = append(roots, target)
	}
	for _, root := range roots {
		res, err := artifact.GarbageCollect(ctx, artifact.GCOptions{
			Root:  root,
			Grace: grace,
			Logf:  log.Printf,
		})
		if err != nil {
			log.Printf("artifact gc (%s): %v", root, err)
			continue
		}
		if res.Deleted > 0 {
			log.Printf(
				"artifact gc (%s): deleted %d artifact(s) (%s)",
				root, res.Deleted, formatBytes(res.BytesDeleted),
			)
		}
	}
}

func printArtifactGCSummary(w io.Writer, res artifact.GCResult) {
	action := "deleted"
	count := res.Deleted
	bytes := res.BytesDeleted
	if res.DryRun {
		action = "would delete"
		count = res.Eligible
		bytes = res.BytesEligible
	}
	fmt.Fprintf(
		w,
		"Artifact GC: scanned %d origin(s), skipped %d without checkpoints, found %d unreferenced artifact(s), kept %d within grace, %s %d artifact(s) (%s)\n",
		res.Origins,
		res.SkippedOrigins,
		res.Candidates,
		res.KeptByGrace,
		action,
		count,
		formatBytes(bytes),
	)
}
