package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/pricing"
)

type pgActivityKept struct {
	identity                  string
	identityUnavailable       bool
	report                    kept.Report
	revision                  int64
	checkpoint                string
	candidates, peers, labels []string
}

type pgActivitySavedReport struct {
	Revision                  int64
	Checkpoint                string
	Candidates, Peers, Labels []string
	Report                    kept.Report
}

func (s *Store) loadActivityReport(selection, version, identity string) (pgActivityKept, bool) {
	s.activityReports.Sweep(time.Now())
	data, ok := s.activityReports.Get(selection+"|"+identity+"|"+version, identity+"|"+version)
	if !ok {
		return pgActivityKept{}, false
	}
	var saved pgActivitySavedReport
	if json.Unmarshal(data, &saved) != nil {
		return pgActivityKept{}, false
	}
	for key, project := range saved.Report.Artifacts.Report.Projects {
		project.ProjectKey = key
		saved.Report.Artifacts.Report.Projects[key] = project
	}
	return pgActivityKept{identity: identity, revision: saved.Revision, checkpoint: saved.Checkpoint, candidates: saved.Candidates, peers: saved.Peers, labels: saved.Labels, report: saved.Report}, true
}

func (s *Store) saveActivityReport(selection, version string, entry pgActivityKept) {
	if entry.checkpoint == "" {
		return
	}
	data, err := json.Marshal(pgActivitySavedReport{Revision: entry.revision, Checkpoint: entry.checkpoint, Candidates: entry.candidates, Peers: entry.peers, Labels: entry.labels, Report: entry.report}, json.FormatNilSliceAsNull(true))
	if err == nil {
		s.activityReports.Put(selection+"|"+entry.identity+"|"+version, entry.identity+"|"+version, data)
	}
}

func (s *Store) activityReportIdentity(ctx context.Context, selection, version string, q activity.Query) (identity string, unavailable bool) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	if ok {
		identity, unavailable = entries[0].identity, entries[0].identityUnavailable
	}
	if identity != "" || unavailable || q.Loc == nil || q.Timezone == "Local" || !s.activityReports.Enabled() {
		return identity, unavailable
	}
	local := time.Now().In(q.Loc)
	if q.RangeEnd.After(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc)) {
		return identity, unavailable
	}
	var restricted bool
	err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relkind IN ('r', 'p') AND (row_security_active(c.oid) OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum > 0 AND NOT a.attisdropped AND NOT has_column_privilege(c.oid, a.attnum, 'SELECT'))))`).Scan(&restricted)
	if err != nil || restricted {
		return "", true
	}
	identity, err = s.activitySourceIdentity(ctx, "")
	unavailable = errors.Is(err, db.ErrUsageSourceUnavailable)
	if !ok && identity != "" {
		if entry, found := s.loadActivityReport(selection, version, identity); found {
			memo.Put(selection, version, []pgActivityKept{entry})
		}
	}
	return identity, unavailable
}

func (s *Store) activityKeptVersion(ctx context.Context, pricingDigest string) (string, error) {
	// Include the table identity so recreating the journal
	// cannot validate a report built against the previous schema.
	oid, err := usageJournalOID(ctx, s.pg)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading kept activity version: %w", err)
	}
	return fmt.Sprintf("%d|%s|%s|%s", oid, pricingDigest, pricing.BillingPolicyVersion(), pricing.ConfiguredAliasesDigest()), nil
}

func (s *Store) keptActivityReport(ctx context.Context, selection, version string, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string) (kept.Report, int64, string, bool, error) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	after := int64(math.MaxInt64)
	if ok {
		after = entries[0].revision
	}
	through, changed, current, previous, err := s.loadUsageChangesWithHistory(ctx, after)
	if isInsufficientPrivilege(err) || isUndefinedTable(err) {
		return kept.Report{}, 0, "", false, nil
	}
	if err != nil || through == 0 || !ok {
		return kept.Report{}, through, current, false, err
	}
	entry := entries[0]
	if entry.checkpoint != "" && entry.checkpoint != previous {
		return kept.Report{}, through, current, false, nil
	}
	if through == entry.revision {
		return entry.report, through, current, true, nil
	}
	if through < entry.revision {
		return kept.Report{}, through, current, false, nil
	}
	for _, id := range changed {
		_, candidate := slices.BinarySearch(entry.candidates, id)
		_, peer := slices.BinarySearch(entry.peers, id)
		if candidate || peer {
			return kept.Report{}, through, current, false, nil
		}
	}
	if len(changed) > 0 {
		_, ids, err := s.activityReportSessions(ctx, f, rangeStart, rangeEnd, changed)
		if err != nil || len(ids) > 0 {
			return kept.Report{}, through, current, false, err
		}
		var overlaps bool
		err = s.pg.QueryRowContext(ctx, `SELECT EXISTS (
   SELECT 1 FROM messages m JOIN sessions s ON s.id = m.session_id
   WHERE m.session_id = ANY($1)
    AND m.claude_message_id != ''
    AND (`+activityReportClaudePeerPredicate("$2", "$3", "$4")+`)
    AND COALESCE(m.timestamp, s.started_at) >= $3::timestamptz
    AND COALESCE(m.timestamp, s.started_at) <= $4::timestamptz)`,
			pgx.QueryExecModeDescribeExec, changed, entry.candidates, lower, upper).Scan(&overlaps)
		if err != nil {
			return kept.Report{}, through, current, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
			return kept.Report{}, through, current, false, nil
		}
	}
	projects, err := s.BuildProjectIdentityMap(ctx, entry.labels)
	if err != nil {
		return kept.Report{}, through, current, false, err
	}
	if !reflect.DeepEqual(export.ProjectMapForWire(projects), entry.report.Artifacts.Report.Projects) {
		return kept.Report{}, through, current, false, nil
	}
	entry.revision = through
	entry.checkpoint = current
	memo.Put(selection, version, []pgActivityKept{entry})
	return entry.report, through, current, true, nil
}
