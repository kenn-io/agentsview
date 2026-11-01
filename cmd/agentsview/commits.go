package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/sync"
)

func commitProvenanceJob(engine *sync.Engine) poller.Job {
	return poller.Job{
		Name: "commit-provenance", Interval: 15 * time.Second, Jitter: time.Second, Cooldown: time.Second, RunAtStart: true,
		Run: func(ctx context.Context) error {
			return engine.RunExclusiveCtx(ctx, func() error { _, err := engine.BackfillCommitProvenance(ctx); return err })
		},
	}
}

func newCommitsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "commits", GroupID: groupData, Short: "Maintain stored commit-session links", Args: cobra.NoArgs}
	cmd.AddCommand(&cobra.Command{Use: "backfill", Short: "Index retained transcripts and local commit trailers", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.LoadPFlagsFor(cmd.Flags(), config.ScopeLocal)
		if err != nil {
			return err
		}
		d, lock, err := openWriteDB(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer lock.Close()
		defer d.Close()
		e := sync.NewEngine(cmd.Context(), d, sync.EngineConfig{Machine: cfg.InstallationID, ScanProtectedPaths: cfg.ScanProtectedPaths, ArchiveContent: cfg.ArchiveContent})
		defer e.Close()
		total := 0
		for {
			n, err := e.BackfillStoredCommitLinks(cmd.Context())
			total += n
			if err != nil {
				return err
			}
			if n == 0 {
				break
			}
		}
		_, err = e.BackfillCommitProvenance(cmd.Context())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Indexed %d retained sessions; local Git discovery advances in bounded batches.\n", total)
		return err
	}})
	return cmd
}
