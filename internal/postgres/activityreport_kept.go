package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"math"
	"reflect"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type pgActivityKept struct {
	identity                  string
	identityUnavailable       bool
	report                    kept.Report
	revision                  int64
	fileRevision              int64 // revision of the saved report; zero until a memo hit advances it
	candidates, peers, labels []string
}

type pgActivitySavedReport struct {
	Revision                  int64
	Candidates, Peers, Labels []string
	Report                    kept.Report
}

// Change this when the saved report format changes.
const pgActivityReportFormat = "pg-activity-report-v1"

const pgActivityRevisionFormat = "pg-activity-revision-v1"

func pgActivityDiskSelection(selection, version, identity string) string {
	return fmt.Sprintf("%q|%q|%q", selection, version, identity)
}

func (s *Store) loadActivityReport(selection, version, identity string) (pgActivityKept, bool) {
	s.sweepActivityReports()
	data, ok := s.activityReports.Get(pgActivityDiskSelection(selection, version, identity), pgActivityReportFormat)
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
	return pgActivityKept{identity: identity, revision: s.savedActivityRevision(selection, version, identity, saved.Revision), fileRevision: saved.Revision, candidates: saved.Candidates, peers: saved.Peers, labels: saved.Labels, report: saved.Report}, true
}

// openActivityReports opens the report namespace and a smaller one for the
// revisions its reports were validated through, so a validation that finds only
// unrelated writes survives a restart without saving the report again.
func (s *Store) openActivityReports(id string) {
	s.activityReports.MaxFiles, s.activityRevisions.MaxFiles = 8, 8
	if err := s.activityReports.Open("postgres-activity-reports", id, 128<<20, 128<<20); err != nil {
		log.Printf("opening activity reports cache: %v", err)
	}
	if err := s.activityRevisions.Open("postgres-activity-revisions", id, 1<<10, 8<<10); err != nil {
		log.Printf("opening activity revisions cache: %v", err)
	}
	s.sweepActivityReports()
}

func (s *Store) sweepActivityReports() {
	now := time.Now()
	s.activityReports.Sweep(now)
	s.activityRevisions.Sweep(now)
}

// advanceActivityRevision saves the revision a memo hit validated beside the
// report file it validated. A rewritten file has another base and ignores it.
func (s *Store) advanceActivityRevision(selection, version string, entry pgActivityKept, through int64) pgActivityKept {
	if entry.identity != "" {
		if entry.fileRevision == 0 {
			entry.fileRevision = entry.revision
		}
		s.activityRevisions.Put(pgActivityDiskSelection(selection, version, entry.identity), pgActivityRevisionFormat, fmt.Appendf(nil, "%d %d", entry.fileRevision, through))
	}
	entry.revision = through
	return entry
}

func (s *Store) savedActivityRevision(selection, version, identity string, saved int64) int64 {
	data, ok := s.activityRevisions.Get(pgActivityDiskSelection(selection, version, identity), pgActivityRevisionFormat)
	var base, through int64
	if _, err := fmt.Sscanf(string(data), "%d %d", &base, &through); !ok || err != nil || base != saved {
		return saved
	}
	return max(saved, through)
}

func encodePGActivityReport(entry pgActivityKept) ([]byte, error) {
	return json.Marshal(pgActivitySavedReport{Revision: entry.revision, Candidates: entry.candidates, Peers: entry.peers, Labels: entry.labels, Report: entry.report}, json.FormatNilSliceAsNull(true))
}

func (s *Store) saveActivityReport(selection, version string, entry pgActivityKept) {
	data, err := encodePGActivityReport(entry)
	if err == nil {
		s.activityReports.Put(pgActivityDiskSelection(selection, version, entry.identity), pgActivityReportFormat, data)
	}
}

// keepActivityReport records the identity a memo hit just resolved. A report
// whose identity was already saved only refreshes its open time; one whose
// identity just became available replaces the stale file saved under it.
func (s *Store) keepActivityReport(selection, version string, entry pgActivityKept, identity string, unavailable bool) {
	saved := entry.identity == identity
	if !saved || entry.identityUnavailable != unavailable {
		entry.identity, entry.identityUnavailable = identity, unavailable
		s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{entry})
	}
	if identity != "" {
		s.refreshActivityReport(selection, version, entry, saved)
	}
}

// refreshActivityReport keeps the selected report before the sweep.
func (s *Store) refreshActivityReport(selection, version string, entry pgActivityKept, saved bool) {
	if saved {
		s.activityReports.Keep(pgActivityDiskSelection(selection, version, entry.identity), pgActivityReportFormat, func() ([]byte, error) { return encodePGActivityReport(entry) })
	} else {
		s.saveActivityReport(selection, version, entry)
	}
	s.sweepActivityReports()
}

func (s *Store) retryActivityIdentity(ctx context.Context, identity string, unavailable bool, oid uint32) (string, bool) {
	if identity != "" || unavailable {
		return identity, unavailable
	}
	var err error
	if oid == 0 {
		identity, err = s.activitySourceIdentity(ctx, "")
	} else {
		identity, err = s.activitySourceIdentityWithOID(ctx, "", oid)
	}
	return identity, errors.Is(err, db.ErrUsageSourceUnavailable)
}

func (s *Store) activityReportIdentity(ctx context.Context, selection, version string, q activity.Query, oid uint32) (identity string, unavailable bool) {
	memo := &s.runtimeState().activityKept
	entries, ok := memo.Get(selection, version)
	if ok {
		identity, unavailable = entries[0].identity, entries[0].identityUnavailable
	}
	if identity != "" || unavailable || q.Loc == nil || !s.activityReports.Enabled() {
		return identity, unavailable
	}
	local := time.Now().In(q.Loc)
	if q.RangeEnd.After(time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, q.Loc)) {
		return identity, unavailable
	}
	identity, unavailable = s.retryActivityIdentity(ctx, identity, unavailable, oid)
	if !ok && identity != "" {
		if entry, found := s.loadActivityReport(selection, version, identity); found {
			memo.Put(selection, version, []pgActivityKept{entry})
		}
	}
	return identity, unavailable
}

func (s *Store) activityKeptVersion(ctx context.Context, pricingDigest string) (string, uint32, error) {
	// Include the table identity so recreating the journal
	// cannot validate a report built against the previous schema.
	oid, err := usageJournalOID(ctx, s.pg)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	return fmt.Sprintf("%d|%s", oid, pricingDigest), oid, nil
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
	entry = s.advanceActivityRevision(selection, version, entry, through)
	memo.Put(selection, version, []pgActivityKept{entry})
	return entry.report, through, true, nil
}
