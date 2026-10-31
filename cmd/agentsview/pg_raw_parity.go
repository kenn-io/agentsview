package main

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawparity"
)

func newPGRawParityCommand() *cobra.Command {
	var scope rawparity.Scope
	cmd := &cobra.Command{Use: "raw-parity [target]", Short: "Compare hosted raw projections with retained pushed sessions", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
			return errors.New("raw-parity requires raw_tenant in the selected PG target")
		}
		// OpenHosted uses the existing CheckHostedTenant runtime-role gate. No
		// owner-mode bypass, schema provisioning, worker or local archive open.
		database, err := postgres.OpenHosted(pg.URL, pg.Schema, pg.RawTenant, pg.AllowInsecure)
		if err != nil {
			return err
		}
		defer database.Close()
		tx, err := database.BeginTx(cmd.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		report, err := rawparity.Run(cmd.Context(), postgres.RawParityReader{Tx: tx, Version: rawProcessingVersion()}, scope)
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return writeRawParityReport(cmd.OutOrStdout(), cmd.ErrOrStderr(), report, outputFormat(cmd) == "json")
	}}
	cmd.Flags().StringVar(&scope.Device, "device", "", "Select a source device ID")
	cmd.Flags().StringVar(&scope.Provider, "provider", "", "Select a provider")
	registerFormatFlags(cmd.Flags())
	return cmd
}

func writeRawParityReport(out, summary io.Writer, report rawparity.Report, asJSON bool) error {
	if asJSON {
		if err := json.MarshalWrite(out, report); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	} else {
		summary = out
	}
	projected, pending, failed, unsupported := 0, 0, 0, 0
	for _, r := range report.Readiness {
		projected += r.Projected
		pending += r.Pending
		failed += r.Failed
		unsupported += r.Unsupported
	}
	_, err := fmt.Fprintf(summary, "parity pass=%t equal=%d differs=%d raw_only=%d legacy_only=%d ambiguous=%d unmatchable=%d projected=%d pending=%d failed=%d unsupported=%d\n", report.Pass, report.Counts["equal"], report.Counts["differs"], report.Counts["raw_only"], report.Counts["legacy_only"], report.Counts["ambiguous"], report.Counts["unmatchable"], projected, pending, failed, unsupported)
	return err
}
