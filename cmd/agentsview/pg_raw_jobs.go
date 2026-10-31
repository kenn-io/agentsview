package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func newPGRawJobsCommand() *cobra.Command {
	root := &cobra.Command{Use: "raw-jobs", Short: "Inspect hosted parse jobs and replay failed jobs"}
	var filter postgres.RawJobRetryFilter
	run := func(retry bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadMinimalFor(config.ScopePG)
			if err != nil {
				return err
			}
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			pg, err := cfg.ResolvePGTarget(target)
			if err != nil {
				return err
			}
			if pg.RawTenant == "" {
				return errors.New("raw-jobs requires raw_tenant")
			}
			store, err := postgres.NewHostedStore(pg.URL, pg.Schema, pg.RawTenant, pg.AllowInsecure)
			if err != nil {
				return err
			}
			defer store.Close()
			sink, err := postgres.NewRawProjectionStore(store.DB(), postgres.RawProjectionOptions{Tenant: pg.RawTenant})
			if err != nil {
				return err
			}
			if retry {
				result, err := sink.RetryFailedRawJobs(cmd.Context(), filter)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Replayed %d failed jobs; superseded %d obsolete jobs\n", result.Replayed, result.Superseded)
				return nil
			}
			counts, err := sink.ListRawJobs(cmd.Context())
			if err != nil {
				return err
			}
			out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(out, "STATE\tERROR CLASS\tCOUNT\tOLDEST FAILED AT")
			for _, row := range counts {
				oldest := "-"
				if row.OldestFailedAt != nil {
					oldest = row.OldestFailedAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(out, "%s\t%s\t%d\t%s\n", row.State, row.ErrorClass, row.Count, oldest)
			}
			return out.Flush()
		}
	}
	list := &cobra.Command{Use: "list [target]", Short: "Count jobs by state and error class", Args: cobra.MaximumNArgs(1), RunE: run(false)}
	retry := &cobra.Command{Use: "retry [target]", Short: "Replay one bounded batch of failed jobs", Args: cobra.MaximumNArgs(1), RunE: run(true)}
	retry.Flags().StringVar(&filter.ErrorClass, "error-class", "", "Replay failures with this stored error class")
	retry.Flags().StringVar(&filter.ManifestID, "manifest", "", "Replay failures for this manifest ID")
	retry.Flags().BoolVar(&filter.All, "all", false, "Replay any failed jobs")
	retry.MarkFlagsOneRequired("error-class", "manifest", "all")
	retry.MarkFlagsMutuallyExclusive("error-class", "manifest", "all")
	root.AddCommand(list, retry)
	return root
}
