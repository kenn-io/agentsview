package review

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ledger"
)

const defaultDiagnosticLimit = 10000

// LedgerQuerier is the ledger read the diagnostics source needs; *db.DB and
// *postgres.Store implement it (PR 15).
type LedgerQuerier interface {
	QueryLedger(ctx context.Context, q ledger.Query) ([]ledger.ZoneEvents, error)
}

// DigestedSubjects reads friction_digest_sessions by id (Task 4); *db.DB
// and *postgres.Store implement it.
type DigestedSubjects interface {
	FrictionDigestedSubjects(ctx context.Context, ids []string) (map[string]string, error)
}

var _ DiagnosticSource = LedgerDiagnostics{}

// LedgerDiagnostics reads producer-written diagnostic events (§11.4). It
// keeps PR 5's DiagnosticSource contract: it never returns an identity
// already recorded in friction_digest_sessions for a different date.
type LedgerDiagnostics struct {
	Store       LedgerQuerier
	Digested    DigestedSubjects // nil skips the cross-date check (tests only)
	Zones       []string         // [] = every zone the caller configured (the wiring fills it)
	Subsystems  []string         // [] or ["*"] = no filter
	LocalSource string
	LocalLabel  string
	Limit       int
}

// DiagnosticSignalsForDate returns one signal per identity whose earliest
// event falls on local date `date`, sorted by identity, minus identities
// already digested on another date. Errors are returned; PR 5's BuildDate
// then fails the build and the poller retries.
func (l LedgerDiagnostics) DiagnosticSignalsForDate(
	ctx context.Context, date string, loc *time.Location,
) ([]friction.Signal, error) {
	start, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return nil, fmt.Errorf("friction diagnostics: parse date %q: %w", date, err)
	}
	limit := l.Limit
	if limit <= 0 {
		limit = defaultDiagnosticLimit
	}
	var subsystems []string
	if len(l.Subsystems) > 0 && (len(l.Subsystems) != 1 || l.Subsystems[0] != "*") {
		subsystems = l.Subsystems
	}
	health := ledger.ClassHealth
	byIdentity := map[string]friction.Signal{}
	malformed, firstProblem := 0, ""
	for _, zone := range l.Zones {
		res, err := l.Store.QueryLedger(ctx, ledger.Query{
			Since: start.UTC(), Until: start.AddDate(0, 0, 1).UTC(),
			Subsystems: subsystems, Class: &health, Zone: zone, Limit: limit,
		})
		if err != nil {
			return nil, fmt.Errorf("friction diagnostics: query zone %s: %w", zone, err)
		}
		for _, ze := range res {
			if len(ze.Events) >= limit {
				log.Printf("friction diagnostics: zone %s hit the %d-event limit for %s; older events that day were not read", ze.Zone, limit, date)
			}
			for _, e := range ze.Events {
				if _, isDiag, problem := parseDiagnostic(e); isDiag && problem != "" {
					malformed++
					if firstProblem == "" {
						firstProblem = fmt.Sprintf("%s: %s", e.EventID, problem)
					}
					continue
				}
				label := e.Source
				if e.Source == l.LocalSource && l.LocalLabel != "" {
					label = l.LocalLabel
				}
				sig, ok := DiagnosticSignal(e, label)
				if !ok {
					continue
				}
				if prior, seen := byIdentity[sig.SubjectID]; seen && !sig.OccurredAt.Before(prior.OccurredAt) {
					continue
				}
				byIdentity[sig.SubjectID] = sig
			}
		}
	}
	if malformed > 0 {
		log.Printf("friction diagnostics: ignored %d malformed diagnostic event(s) on %s (first: %s)", malformed, date, firstProblem)
	}
	if l.Digested != nil && len(byIdentity) > 0 {
		ids := make([]string, 0, len(byIdentity))
		for id := range byIdentity {
			ids = append(ids, id)
		}
		digested, err := l.Digested.FrictionDigestedSubjects(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("friction diagnostics: digested identities for %s: %w", date, err)
		}
		for id, on := range digested {
			if on != date {
				delete(byIdentity, id)
			}
		}
	}
	out := make([]friction.Signal, 0, len(byIdentity))
	for _, sig := range byIdentity {
		out = append(out, sig)
	}
	slices.SortFunc(out, func(a, b friction.Signal) int {
		switch {
		case a.SubjectID < b.SubjectID:
			return -1
		case a.SubjectID > b.SubjectID:
			return 1
		}
		return 0
	})
	return out, nil
}
