package postgres

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type pgActivityKept struct {
	report       kept.Report
	revision     int64
	read, labels []string
}

func (s *Store) activityKeptVersion(ctx context.Context, pricing []export.EffectivePricingRow) (string, int64, error) {
	revision, err := s.UsageChangeRevision(ctx)
	if err != nil || revision == 0 {
		return "", 0, err
	}
	// Include the table identity so recreating the journal
	// cannot validate a report built against the previous schema.
	var oid uint32
	err = s.pg.QueryRowContext(ctx, `SELECT 'usage_session_changes'::regclass::oid`).Scan(&oid)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	digest, err := export.EffectivePricingDigest(pricing)
	return fmt.Sprintf("%d|%s", oid, digest), revision, err
}

func (s *Store) keptActivityReport(ctx context.Context, selection, version string) (kept.Report, bool, error) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	if !ok {
		return kept.Report{}, false, nil
	}
	entry := entries[0]
	through, changed, err := s.LoadUsageChanges(ctx, entry.revision)
	if err != nil {
		return kept.Report{}, false, err
	}
	if through == 0 || through < entry.revision {
		return kept.Report{}, false, nil
	}
	for _, id := range changed {
		if _, found := slices.BinarySearch(entry.read, id); found {
			return kept.Report{}, false, nil
		}
	}
	if len(changed) > 0 {
		rangeStart, rangeEnd := entry.report.Artifacts.Report.RangeStart, entry.report.Artifacts.Report.RangeEnd
		var overlaps bool
		err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM sessions s WHERE s.id = ANY($1) AND ((`+
			activityReportSessionWindowPredicate("$2", "$3")+`)
   OR EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id
    AND (`+activityReportClaudePeerPredicate("$6", "$4", "$5")+`)
    AND COALESCE(m.timestamp, s.started_at) >= $4::timestamptz
    AND COALESCE(m.timestamp, s.started_at) <= $5::timestamptz)))`,
			changed, rangeStart, rangeEnd, db.PaddedUTCBound(rangeStart, -14), db.PaddedUTCBound(rangeEnd, 14), entry.read).Scan(&overlaps)
		if err != nil {
			return kept.Report{}, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
			return kept.Report{}, false, nil
		}
	}
	projects, err := s.BuildProjectIdentityMap(ctx, entry.labels)
	if err != nil {
		return kept.Report{}, false, err
	}
	if !reflect.DeepEqual(export.ProjectMapForWire(projects), entry.report.Artifacts.Report.Projects) {
		return kept.Report{}, false, nil
	}
	entry.revision = through
	memo.Put(selection, version, []pgActivityKept{entry})
	return entry.report, true, nil
}
