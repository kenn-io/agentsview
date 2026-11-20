package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

func (s *Store) refreshActivityReport(selection, version, identity string, previousRevision int64) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	if !ok {
		return
	}
	defer s.activityReports.Sweep(time.Now())
	if identity == "" {
		return
	}
	entry := entries[0]
	if entry.identity == "" {
		entry.identity = identity
		memo.Put(selection, version, []pgActivityKept{entry})
		s.saveActivityReport(selection, version, entry)
		return
	}
	var data []byte
	if entry.revision != previousRevision {
		var err error
		data, err = json.Marshal(pgActivitySavedReport{Revision: entry.revision, Checkpoint: entry.checkpoint, Candidates: entry.candidates, Peers: entry.peers, Labels: entry.labels, Report: entry.report}, json.FormatNilSliceAsNull(true))
		if err != nil {
			return
		}
	}
	s.activityReports.Refresh(selection+"|"+identity+"|"+version, identity+"|"+version, data)
}

func (s *Store) activityReportRestricted(ctx context.Context) bool {
	var restricted bool
	err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('sessions', 'messages', 'tool_result_events', 'usage_events', 'cursor_usage_events', 'sync_metadata', 'usage_session_changes', 'model_pricing', 'model_pricing_bands', 'genai_pricing', 'pricing_meta', 'source_archives', 'source_project_identity_observations') AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND (c.relkind IN ('v', 'm', 'f') OR row_security_active(c.oid) OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum > 0 AND NOT a.attisdropped AND NOT has_column_privilege(c.oid, a.attnum, 'SELECT'))))`).Scan(&restricted)
	return err != nil || restricted
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
	identity, err := s.activitySourceIdentity(ctx, "")
	unavailable = errors.Is(err, db.ErrUsageSourceUnavailable)
	if !ok && identity != "" {
		if entry, found := s.loadActivityReport(selection, version, identity); found {
			memo.Put(selection, version, []pgActivityKept{entry})
		}
	}
	return identity, unavailable
}

func activityTimezoneKey(location *time.Location, start, end time.Time) string {
	if activityTimezoneSkipsDay(location, start.Add(-8*24*time.Hour), end) {
		return db.TrendsTimezoneRangeKey(location, start.Add(-8*24*time.Hour), end)
	}
	digest := sha256.New()
	fmt.Fprint(digest, db.TrendsTimezoneRangeKey(location, start, end))
	for _, spec := range []activity.BucketSpec{{Unit: activity.BucketDay, NominalSeconds: 86400}, {Unit: activity.BucketWeek, NominalSeconds: 604800}} {
		windows, _ := activity.BuildBuckets(start, end, spec, location)
		for _, window := range windows {
			fmt.Fprintf(digest, "|%d:%d", window.Start.Unix(), window.End.Unix())
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func activityTimezoneSkipsDay(location *time.Location, start, end time.Time) bool {
	for cursor := start; cursor.Before(end); {
		local := cursor.In(location)
		_, before := local.Zone()
		_, next := local.ZoneBounds()
		if next.IsZero() || !next.After(cursor) || next.After(end) {
			return false
		}
		_, after := next.In(location).Zone()
		if after-before >= 24*60*60 {
			return true
		}
		cursor = next
	}
	return false
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

func (s *Store) validateKeptActivityReport(ctx context.Context, selection, version string, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string) (kept.Report, int64, string, bool, error) {
	reader := s
	var tx *sql.Tx
	if s.readTx == nil {
		var err error
		tx, err = s.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return kept.Report{}, 0, "", false, err
		}
		defer func() { _ = tx.Rollback() }()
		reader = s.readView(tx)
	}
	if reader.activityReportRestricted(ctx) {
		return kept.Report{}, 0, "", false, nil
	}
	entry, revision, checkpoint, hit, err := reader.keptActivityReport(ctx, selection, version, f, rangeStart, rangeEnd, lower, upper)
	if err != nil {
		return kept.Report{}, revision, checkpoint, false, err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return kept.Report{}, 0, "", false, err
		}
	}
	if hit {
		s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{entry})
	}
	return entry.report, revision, checkpoint, hit, nil
}

func (s *Store) keptActivityReport(ctx context.Context, selection, version string, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string) (pgActivityKept, int64, string, bool, error) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	after := int64(math.MaxInt64)
	if ok {
		after = entries[0].revision
	}
	through, changed, current, previous, err := s.loadUsageChangesWithHistory(ctx, after)
	if isInsufficientPrivilege(err) || isUndefinedTable(err) {
		return pgActivityKept{}, 0, "", false, nil
	}
	if err != nil || through == 0 || !ok {
		return pgActivityKept{}, through, current, false, err
	}
	entry := entries[0]
	if entry.checkpoint != "" && entry.checkpoint != previous {
		return pgActivityKept{}, through, current, false, nil
	}
	if through == entry.revision {
		return entry, through, current, true, nil
	}
	if through < entry.revision {
		return pgActivityKept{}, through, current, false, nil
	}
	for _, id := range changed {
		_, candidate := slices.BinarySearch(entry.candidates, id)
		_, peer := slices.BinarySearch(entry.peers, id)
		if candidate || peer {
			return pgActivityKept{}, through, current, false, nil
		}
	}
	if len(changed) > 0 {
		_, ids, err := s.activityReportSessions(ctx, f, rangeStart, rangeEnd, changed)
		if err != nil || len(ids) > 0 {
			return pgActivityKept{}, through, current, false, err
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
			return pgActivityKept{}, through, current, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
			return pgActivityKept{}, through, current, false, nil
		}
	}
	projects, err := s.BuildProjectIdentityMap(ctx, entry.labels)
	if err != nil {
		return pgActivityKept{}, through, current, false, err
	}
	if !reflect.DeepEqual(export.ProjectMapForWire(projects), entry.report.Artifacts.Report.Projects) {
		return pgActivityKept{}, through, current, false, nil
	}
	entry.revision = through
	entry.checkpoint = current
	return entry, through, current, true, nil
}
