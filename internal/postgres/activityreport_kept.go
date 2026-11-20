package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
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

func (s *Store) refreshActivityReport(selection, version, identity string, previousRevision, revision int64, checkpoint string) {
	entries, ok := s.runtimeState().activityKept.Get(selection, version)
	if !ok || identity == "" || entries[0].checkpoint == "" || entries[0].revision != revision || entries[0].checkpoint != checkpoint {
		return
	}
	entry := entries[0]
	stale := entry.identity != identity
	entry.identity = identity
	key := selection + "|" + identity + "|" + version
	if !stale && entry.revision == previousRevision {
		s.activityReports.Keep(key, identity+"|"+version, entry.encode)
	} else {
		s.saveActivityReport(selection, version, entry)
	}
	s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{entry})
	s.activityReports.Sweep(time.Now())
}

func (s *Store) activityReportRestricted(ctx context.Context) bool {
	var restricted bool
	err := s.pg.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('sessions', 'messages', 'tool_result_events', 'usage_events', 'sync_metadata', 'usage_session_changes', 'model_pricing', 'model_pricing_bands', 'genai_pricing', 'source_archives', 'source_project_identity_observations') AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND (c.relkind IN ('v', 'm', 'f') OR row_security_active(c.oid) OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attnum > 0 AND NOT a.attisdropped AND NOT has_column_privilege(c.oid, a.attnum, 'SELECT'))))`).Scan(&restricted)
	return err != nil || restricted
}

func (s *Store) activityReportEligible(q activity.Query) bool {
	if q.Loc == nil || !s.activityReports.Enabled() {
		return false
	}
	local := time.Now().In(q.Loc)
	return !q.RangeEnd.After(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc))
}

func (s *Store) activityReportIdentity(ctx context.Context) string {
	identity, _ := s.activitySourceIdentity(ctx, "")
	return identity
}

func activityTimezoneKey(q activity.Query) string {
	h := sha256.New()
	for cursor := q.RangeStart; cursor.Before(q.RangeEnd); {
		local := cursor.In(q.Loc)
		name, offset := local.Zone()
		_, end := local.ZoneBounds()
		fmt.Fprintf(h, "%s|%d|%d|", name, offset, end.Unix())
		if end.IsZero() || !end.After(cursor) {
			break
		}
		cursor = end
	}
	return hex.EncodeToString(h.Sum(nil))
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

func (s *Store) validateKeptActivityReport(ctx context.Context, selection, version string, entries []pgActivityKept, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string) (kept.Report, int64, string, bool, error) {
	hadEntry := len(entries) > 0
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
	var snapshotChanges []string
	entry, revision, checkpoint, hit, err := reader.keptActivityReport(ctx, entries, f, rangeStart, rangeEnd, lower, upper, &snapshotChanges)
	if err != nil {
		return kept.Report{}, revision, checkpoint, false, err
	}
	if !hit {
		return kept.Report{}, revision, checkpoint, false, nil
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return kept.Report{}, 0, "", false, err
		}
	}
	if s.activityReportRestricted(ctx) {
		s.runtimeState().activityKept.DeleteSlots(func(slot string) bool { return slot == selection })
		return kept.Report{}, revision, checkpoint, false, nil
	}
	if hit {
		// A restore can replace the history while validation holds its snapshot.
		// Do not advance the memo until the live journal still matches it.
		if hadEntry && entry.revision != entries[0].revision && entries[0].checkpoint != "" {
			if tx != nil {
				through, changes, current, previous, checkErr := s.loadUsageChangesWithHistory(ctx, entries[0].revision)
				if checkErr == nil && through == revision && current == checkpoint && previous == entries[0].checkpoint && slices.Equal(changes, snapshotChanges) {
					s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{entry})
				}
			}
		} else {
			s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{entry})
		}
	}
	return entry.report, revision, checkpoint, hit, nil
}

func (s *Store) keptActivityReport(ctx context.Context, entries []pgActivityKept, f db.AnalyticsFilter, rangeStart, rangeEnd, lower, upper string, snapshotChanges *[]string) (pgActivityKept, int64, string, bool, error) {
	after := int64(math.MaxInt64)
	if len(entries) > 0 {
		after = entries[0].revision
	}
	through, changed, current, previous, err := s.loadUsageChangesWithHistory(ctx, after)
	*snapshotChanges = changed
	if isInsufficientPrivilege(err) || isUndefinedTable(err) {
		return pgActivityKept{}, 0, "", false, nil
	}
	if err != nil || through == 0 || len(entries) == 0 {
		return pgActivityKept{}, through, current, false, err
	}
	entry := entries[0]
	if entry.checkpoint != "" && entry.checkpoint != previous {
		return pgActivityKept{}, through, current, false, nil
	}
	if through == entry.revision {
		if entry.checkpoint != "" {
			var token string
			if err := s.pg.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_token'), '')`).Scan(&token); err != nil {
				return pgActivityKept{}, through, current, false, err
			}
			if token != entry.checkpoint {
				return pgActivityKept{}, through, current, false, nil
			}
		}
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

func (s *Store) loadUsageChangesWithHistory(ctx context.Context, afterRevision int64) (int64, []string, string, string, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT r.revision, c.session_id,
 COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_checkpoint:' || r.revision::text), ''),
 COALESCE((SELECT value FROM sync_metadata WHERE key='usage_history_checkpoint:' || $2::bigint::text), '')
 FROM (SELECT `+supportedUsageChangeRevisionSQL+` AS revision) r
 LEFT JOIN usage_session_changes c ON r.revision > 0 AND c.revision > $2 AND c.revision <= r.revision AND c.session_id <> ''
 ORDER BY c.revision, c.session_id`, strconv.Itoa(usageChangeJournalVersionPG), afterRevision)
	if err != nil {
		return 0, nil, "", "", err
	}
	defer rows.Close()
	var revision int64
	var current, previous string
	var ids []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&revision, &id, &current, &previous); err != nil {
			return 0, nil, "", "", err
		}
		if id.Valid {
			ids = append(ids, id.String)
		}
	}
	return revision, ids, current, previous, rows.Err()
}
