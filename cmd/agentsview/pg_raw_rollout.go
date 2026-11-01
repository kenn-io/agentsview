package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
)

func newPGRawRolloutCommand() *cobra.Command {
	root := &cobra.Command{Use: "raw-rollout", Short: "Inspect and change hosted rollout scopes"}
	for _, action := range []string{"list", "shadow", "clear", "activate"} {
		var device, provider string
		var accept bool
		cmd := &cobra.Command{Use: action + " [target]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadMinimalFor(config.ScopePG)
			if err != nil {
				return err
			}
			applyClassifierConfig(cfg)
			target := ""
			if len(args) > 0 {
				target = args[0]
			}
			pg, err := cfg.ResolvePGTarget(target)
			if err != nil {
				return err
			}
			if pg.RawTenant == "" {
				return errors.New("raw-rollout requires raw_tenant in the selected owner target")
			}
			database, err := postgres.Open(pg.URL, pg.Schema, pg.AllowInsecure)
			if err != nil {
				return errors.New("opening hosted owner connection failed")
			}
			defer database.Close()
			if action == "activate" {
				// This base predates the sanctioned embedding-aware PGConfig seam. Keep
				// unknown explicit until loaded cfg.ResolvePGTarget(target) can attest the
				// actual RawEmbeddings setting. Absence is never evidence of disabled.
				if _, err := postgres.ActivateRawRollout(cmd.Context(), database, pg.Schema, pg.RawTenant, device, provider, postgres.RawActivationOptions{Version: rawProcessingVersion(), AcceptDifferences: accept, Embeddings: postgres.RawEmbeddingsUnknown}); err != nil {
					return err
				}
			} else if action != "list" {
				if err := postgres.ChangeRawRollout(cmd.Context(), database, pg.Schema, pg.RawTenant, device, provider, action); err != nil {
					return err
				}
			}
			scopes, err := postgres.ListRawRollout(cmd.Context(), database, pg.Schema, pg.RawTenant, device, provider)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "DEVICE\tPROVIDER\tSTATE\tSERVED\tSHADOW\tSUPERSEDED\tPARITY")
			for _, scope := range scopes {
				parity := string(scope.Parity)
				if parity == "" {
					parity = "null"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%q\t%q\t%s\t%d\t%d\t%d\t%s\n", scope.DeviceID, scope.Provider, scope.State, scope.Served, scope.Shadow, scope.Superseded, parity)
			}
			return nil
		}}
		cmd.Flags().StringVar(&device, "device", "", "Select a device (empty means all)")
		cmd.Flags().StringVar(&provider, "provider", "", "Select a provider (empty means all)")
		if action == "activate" {
			cmd.Flags().BoolVar(&accept, "accept-differences", false, "Waive only content differences and record their original counts")
		}
		root.AddCommand(cmd)
	}
	return root
}
