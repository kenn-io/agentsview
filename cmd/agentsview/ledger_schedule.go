package main

import (
	"context"
	"errors"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
)

const (
	ledgerImportJobName  = "ledger-import"
	ledgerImportInterval = 5 * time.Minute
	ledgerImportJitter   = 30 * time.Second
)

// ledgerImportJob follows every zone's import_path the way jilog's
// refresh_from_store does: new segments are verified and appended, bad
// ones are reported and retried on the next run. ok is false when the
// ledger is off or no zone has an import_path.
func ledgerImportJob(cfg config.Config, database *db.DB, runner remoteSyncExclusiveRunner) (poller.Job, bool) {
	if !cfg.Ledger.Enabled {
		return poller.Job{}, false
	}
	type target struct{ zone, dir string }
	var targets []target
	for _, zone := range cfg.Ledger.ZoneIDs() {
		zc, _ := cfg.Ledger.Zone(zone)
		dir, err := zc.ImportSegmentsDir()
		if err != nil {
			log.Printf("ledger-import: zone %s: %v", zone, err)
			continue
		}
		if dir != "" {
			targets = append(targets, target{zone: zone, dir: dir})
		}
	}
	if len(targets) == 0 {
		return poller.Job{}, false
	}
	return poller.Job{
		Name:       ledgerImportJobName,
		Interval:   ledgerImportInterval,
		Jitter:     ledgerImportJitter,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			var errs []error
			for _, t := range targets {
				err := runPricingExclusive(runner, func() error {
					if err := ctx.Err(); err != nil {
						return err
					}
					report, err := runLedgerImport(ctx, database, t.zone, t.dir)
					if err != nil {
						return err
					}
					if len(report.Failed) > 0 || len(report.ListingErrors) > 0 {
						log.Printf("ledger-import [%s]: %d segment(s) indexed, %d failed, %d listing error(s) in %s",
							t.zone, report.SegmentsIndexed, len(report.Failed), len(report.ListingErrors), t.dir)
					}
					return nil
				})
				if err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}, true
}
