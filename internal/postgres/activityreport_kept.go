package postgres

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type pgActivityKept struct {
	report                   kept.Report
	revision                 int64
	read, labels             []string
	rangeStart, rangeEnd     string
	paddedLower, paddedUpper string
}

func (s *Store) activityKeptVersion(ctx context.Context) (string, int64, error) {
	runtime := s.runtimeState()
	runtime.activityKeptOnce.Do(func() {
		runtime.activityKept.Size = func(e pgActivityKept) int64 {
			size := activity.EstimatedArtifactBytes(e.report.Artifacts)
			for _, id := range e.read {
				size += int64(len(id)) + 16
			}
			for _, label := range e.labels {
				size += int64(len(label)) + 16
			}
			return size
		}
		runtime.activityKept.MaxBytes = 128 << 20
	})
	// Read the table identity with the revision so recreating the journal
	// cannot validate a report built against the previous schema.
	var oid uint32
	var revision int64
	err := s.pg.QueryRowContext(ctx, `SELECT 'usage_session_changes'::regclass::oid,
  COALESCE((SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision'
   AND EXISTS (SELECT 1 FROM sync_metadata WHERE key = 'usage_change_journal_version' AND value = $1)), 0)
  FROM (SELECT count(*) FROM usage_session_changes WHERE false) journal`, fmt.Sprint(usageChangeJournalVersionPG)).Scan(&oid, &revision)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	if revision == 0 {
		return "", 0, nil
	}
	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return "", 0, err
	}
	digest, err := export.EffectivePricingDigest(pricing)
	return fmt.Sprintf("%d|%s", oid, digest), revision, err
}

func (s *Store) keptActivityReport(ctx context.Context, selection, version string, revision int64) (kept.Report, bool, error) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	if !ok || revision < entries[0].revision {
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
		var overlaps bool
		err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM sessions s WHERE s.id = ANY($1) AND ((`+
			activityReportSessionWindowPredicate("$2", "$3")+`)
   OR EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id
    AND COALESCE(m.timestamp, s.started_at) >= $4::timestamptz
    AND COALESCE(m.timestamp, s.started_at) <= $5::timestamptz)))`,
			changed, entry.rangeStart, entry.rangeEnd, entry.paddedLower, entry.paddedUpper).Scan(&overlaps)
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
