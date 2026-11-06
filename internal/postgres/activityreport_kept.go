package postgres

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type pgActivityKept struct {
	report                    kept.Report
	revision                  int64
	candidates, peers, labels []string
}

func (s *Store) activityKeptVersion(ctx context.Context, pricing []export.EffectivePricingRow) (string, error) {
	// Include the table identity so recreating the journal
	// cannot validate a report built against the previous schema.
	var oid uint32
	err := s.pg.QueryRowContext(ctx, `SELECT 'usage_session_changes'::regclass::oid`).Scan(&oid)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading kept activity version: %w", err)
	}
	digest, err := export.EffectivePricingDigest(pricing)
	return fmt.Sprintf("%d|%s", oid, digest), err
}

func (s *Store) keptActivityReport(ctx context.Context, selection, version string, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string) (kept.Report, int64, bool, error) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	after := int64(math.MaxInt64)
	if ok {
		after = entries[0].revision
	}
	through, changed, err := s.LoadUsageChanges(ctx, after)
	if err != nil || through == 0 || !ok {
		return kept.Report{}, through, false, err
	}
	entry := entries[0]
	if through < entry.revision {
		return kept.Report{}, through, false, nil
	}
	for _, id := range changed {
		_, candidate := slices.BinarySearch(entry.candidates, id)
		_, peer := slices.BinarySearch(entry.peers, id)
		if candidate || peer {
			return kept.Report{}, through, false, nil
		}
	}
	if len(changed) > 0 {
		_, ids, err := s.activityReportSessions(ctx, f, rangeStart, rangeEnd, changed)
		if err != nil || len(ids) > 0 {
			return kept.Report{}, through, false, err
		}
		var overlaps bool
		err = s.pg.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM messages m JOIN sessions s ON s.id = m.session_id
   WHERE m.session_id = ANY($1)
    AND (`+activityReportClaudePeerPredicate("$2", "$3", "$4")+`)
    AND COALESCE(m.timestamp, s.started_at) >= $3::timestamptz
    AND COALESCE(m.timestamp, s.started_at) <= $4::timestamptz)`,
			changed, entry.candidates, lower, upper).Scan(&overlaps)
		if err != nil {
			return kept.Report{}, through, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
			return kept.Report{}, through, false, nil
		}
	}
	projects, err := s.BuildProjectIdentityMap(ctx, entry.labels)
	if err != nil {
		return kept.Report{}, through, false, err
	}
	if !reflect.DeepEqual(export.ProjectMapForWire(projects), entry.report.Artifacts.Report.Projects) {
		return kept.Report{}, through, false, nil
	}
	entry.revision = through
	memo.Put(selection, version, []pgActivityKept{entry})
	return entry.report, through, true, nil
}
