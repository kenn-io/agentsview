// ABOUTME: Ledger sinks and the diagnostics source for the friction job:
// ABOUTME: the job sink runs inside the job's exclusive section, never re-entering it.
package main

import (
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/ledger"
)

type frictionLedgerParts struct {
	JobSink     ledger.Sink
	APISink     ledger.Sink
	Source      string
	Diagnostics review.DiagnosticSource
}

// runInline is the exclusive func for appends made by code that already
// holds engine.RunExclusive (the friction-review job, §8.1). Passing
// engine.RunExclusive there would re-lock a non-reentrant mutex.
func runInline(work func() error) error { return work() }

// diagnosticsStore is what the diagnostics source reads: the ledger (PR 15)
// and friction_digest_sessions by id (Task 4).
type diagnosticsStore interface {
	review.LedgerQuerier
	review.DigestedSubjects
}

// frictionLedgerWiring builds the job-scoped sink (inline), the API-scoped
// sink (apiExclusive, e.g. engine.RunExclusive; nil on pg serve, where PG
// takes no lock and relies on primary-key uniqueness plus the Writer's
// bounded seq retry, PR 13) and the diagnostics source. Everything is nil
// when [ledger] is off.
func frictionLedgerWiring(
	cfg config.Config, store ledger.WriterStore, querier diagnosticsStore,
	apiExclusive func(func() error) error,
) frictionLedgerParts {
	if !cfg.Ledger.Enabled {
		return frictionLedgerParts{}
	}
	source := cfg.Ledger.EffectiveSource(cfg.InstallationID)
	zones := cfg.Ledger.ZoneIDs() // PR 13: default zone first, exists without [[ledger.zones]]
	if apiExclusive == nil {
		apiExclusive = runInline
	}
	parts := frictionLedgerParts{
		JobSink: ledger.NewZoneWriters(store, source, zones, runInline),
		APISink: ledger.NewZoneWriters(store, source, zones, apiExclusive),
		Source:  source,
	}
	if d := cfg.Friction.Diagnostics; d.Enabled {
		diagZones := d.Zones
		if len(diagZones) == 0 {
			diagZones = zones
		}
		parts.Diagnostics = review.LedgerDiagnostics{
			Store: querier, Digested: querier, Zones: diagZones, Subsystems: d.Subsystems,
			LocalSource: source, LocalLabel: cfg.LocalMachineName,
		}
	}
	return parts
}
