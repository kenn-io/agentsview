package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func newPGRawReconcileCommand() *cobra.Command {
	var repair bool
	cmd := &cobra.Command{Use: "raw-reconcile [target]", Short: "Check or repair one bounded batch of hosted raw selections", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			return errors.New("raw-reconcile requires raw_tenant")
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
		result, err := sink.ReconcileRawSources(cmd.Context(), repair)
		if err != nil {
			return err
		}
		var findings int64
		if err = store.DB().QueryRowContext(cmd.Context(), `SELECT count(*) FROM raw_custody_findings WHERE tenant_id=$1`, pg.RawTenant).Scan(&findings); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "scanned=%d found=%d repaired=%d unrecoverable=%d wrapped=%t busy=%t historical_custody_findings=%d\n", result.Scanned, result.Found, result.Repaired, result.Unrecoverable, result.Wrapped, result.Busy, findings)
		if findings > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Historical custody findings require investigation; restored bytes may already be available.")
		}
		return nil
	}}
	cmd.Flags().BoolVar(&repair, "repair", false, "Repair selections using durable parser-version evidence")
	return cmd
}
