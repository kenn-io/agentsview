package postgres

import (
	"context"
	"encoding/json/v2"
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
	checkpoint                string
	report                    kept.Report
	revision                  int64
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
	if json.Unmarshal(data, &saved) != nil || saved.Checkpoint == "" {
		return pgActivityKept{}, false
	}
	for key, project := range saved.Report.Artifacts.Report.Projects {
		project.ProjectKey = key
		saved.Report.Artifacts.Report.Projects[key] = project
	}
	return pgActivityKept{identity: identity, revision: saved.Revision, checkpoint: saved.Checkpoint, candidates: saved.Candidates, peers: saved.Peers, labels: saved.Labels, report: saved.Report}, true
}

func (e pgActivityKept) encode() ([]byte, error) {
	return json.Marshal(pgActivitySavedReport{Revision: e.revision, Checkpoint: e.checkpoint, Candidates: e.candidates, Peers: e.peers, Labels: e.labels, Report: e.report}, json.FormatNilSliceAsNull(true))
}

func (s *Store) saveActivityReport(selection, version string, entry pgActivityKept) {
	if entry.checkpoint == "" {
		return
	}
	if data, err := entry.encode(); err == nil {
		s.activityReports.Put(selection+"|"+entry.identity+"|"+version, entry.identity+"|"+version, data)
	}
}

func (s *Store) activityReportRestricted(ctx context.Context) bool {
	var restricted bool
	err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('sessions', 'messages', 'tool_result_events', 'usage_events', 'sync_metadata', 'usage_session_changes', 'model_pricing', 'model_pricing_bands', 'genai_pricing', 'source_archives', 'source_project_identity_observations') AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND (c.relkind IN ('v', 'm', 'f') OR row_security_active(c.oid) OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum > 0 AND NOT a.attisdropped AND NOT has_column_privilege(c.oid, a.attnum, 'SELECT'))))`).Scan(&restricted)
	return err != nil || restricted
}

func (s *Store) activityReportIdentity(ctx context.Context, q activity.Query) string {
	if q.Loc == nil || q.Timezone == "Local" || !s.activityReports.Enabled() {
		return ""
	}
	local := time.Now().In(q.Loc)
	if q.RangeEnd.After(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc)) {
		return ""
	}
	identity, _ := s.activitySourceIdentity(ctx, "")
	return identity
}

func (s *Store) activityTableIdentity(ctx context.Context) (string, error) {
	tables := make([]string, 0, len(usageChangeTablesPG)+3)
	for _, table := range usageChangeTablesPG {
		tables = append(tables, table.table)
	}
	tables = append(tables, "messages", "tool_result_events", "usage_session_changes")
	var files string
	err := s.pg.QueryRowContext(ctx, `SELECT CASE WHEN bool_or(c.relkind = 'p') THEN '' ELSE COALESCE(md5(string_agg(pg_relation_filenode(c.oid)::text, ',' ORDER BY t)), '') END FROM unnest($1::text[]) t JOIN pg_class c ON c.oid = to_regclass(t)`, tables).Scan(&files)
	return files, err
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
	return fmt.Sprintf("%d|%s|%s", oid, pricingDigest, pricing.ConfiguredAliasesDigest()), nil
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
	if through == entry.revision {
		return entry.report, through, true, nil
	}
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
    AND m.claude_message_id != ''
    AND (`+activityReportClaudePeerPredicate("$2", "$3", "$4")+`)
    AND COALESCE(m.timestamp, s.started_at) >= $3::timestamptz
    AND COALESCE(m.timestamp, s.started_at) <= $4::timestamptz)`,
			pgx.QueryExecModeDescribeExec, changed, entry.candidates, lower, upper).Scan(&overlaps)
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
