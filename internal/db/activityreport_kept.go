package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type activityKept struct {
	usage        *activityKeptUsage
	report       kept.Report
	size         int64
	revision     int64
	read, labels []string
}

type activityKeptUsage struct {
	rows       []activity.UsageRow
	pricing    *export.PricingBlock
	from, next time.Time
}

func activityUsageSelection(f AnalyticsFilter, q activity.Query) string {
	q.EffectiveEnd = q.RangeEnd
	return "usage|" + kept.Selection(f, q)
}

func newActivityKeptMemo() kept.Memo[activityKept] {
	return kept.Memo[activityKept]{
		Size:     func(e activityKept) int64 { return e.size },
		MaxBytes: 128 << 20,
	}
}

func activityKeptSize(e activityKept) int64 {
	size := int64(cap(e.read)) * 16
	if e.usage == nil {
		size += activity.EstimatedArtifactBytes(e.report.Artifacts)
	} else {
		size += int64(unsafe.Sizeof(activityKeptUsage{})) + int64(cap(e.usage.rows))*int64(unsafe.Sizeof(activity.UsageRow{}))
		for _, row := range e.usage.rows {
			size += int64(len(row.SessionID) + len(row.SourceSessionID) + len(row.Model) + len(row.ProviderID) + len(row.Timestamp) + len(row.Project) + len(row.Machine) + len(row.UsageSource) + len(row.Agent) + len(row.ClaudeMessageID) + len(row.ClaudeRequestID) + len(row.SourceUUID) + len(row.UsageDedupKey) + len(row.CostSource))
			if row.SessionCost != nil {
				size += int64(unsafe.Sizeof(*row.SessionCost))
			}
		}
		size += kept.JSONBytes(e.usage.pricing)
	}
	for _, id := range e.read {
		size += int64(len(id))
	}
	for _, label := range e.labels {
		size += int64(len(label))
	}
	return size
}

func (db *DB) activityKeptVersion(ctx context.Context) (string, int64, error) {
	// In-memory pricing setters run before writable reports are kept or on
	// read-only handles, which skip this cache.
	databaseID, pricingRevision, err := db.databaseIDAndUsagePricingRevision(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	revision, err := db.UsageChangeRevision(ctx)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%s|%d", databaseID, pricingRevision), revision, nil
}

func (db *DB) keptActivityReport(
	ctx context.Context,
	selection, version string,
	revision int64,
	filter AnalyticsFilter,
	q activity.Query,
) (kept.Report, bool, error) {
	entries, ok := db.activityKept.Get(selection, version)
	if !ok {
		return kept.Report{}, false, nil
	}
	entry := entries[0]
	candidateIDs := make([]string, len(entry.report.Artifacts.Sessions))
	for i, session := range entry.report.Artifacts.Sessions {
		candidateIDs[i] = session.SessionID
	}
	valid, err := db.keptActivityChangesValid(ctx, entry.revision, entry.read, candidateIDs, filter, q)
	if err != nil {
		return kept.Report{}, false, err
	}
	if !valid {
		return kept.Report{}, false, nil
	}
	projects, err := db.BuildProjectIdentityMap(ctx, entry.labels)
	if err != nil {
		return kept.Report{}, false, err
	}
	if !reflect.DeepEqual(export.ProjectMapForWire(projects), entry.report.Artifacts.Report.Projects) {
		return kept.Report{}, false, nil
	}
	if revision > entry.revision {
		entry.revision = revision
		db.activityKept.Put(selection, version, []activityKept{entry})
	}
	return entry.report, true, nil
}

// keptActivityChangesValid checks the usage journal and snapshot peers read by a kept result.
func (db *DB) keptActivityChangesValid(ctx context.Context, oldRevision int64, read, candidateIDs []string, filter AnalyticsFilter, q activity.Query) (bool, error) {
	changed, err := db.LoadUsageChanges(ctx, oldRevision, math.MaxInt64)
	if err != nil {
		return false, err
	}
	for _, id := range changed {
		if _, found := slices.BinarySearch(read, id); found {
			return false, nil
		}
	}
	if len(changed) > 0 {
		changedJSON, err := json.Marshal(changed)
		if err != nil {
			return false, err
		}
		rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
		usageLowerBound := PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
		usageUpperBound := PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
		where, args := activityReportSessionSelectionWhere(
			filter, rangeStartUTC, rangeEndUTC,
		)
		args = append(args, string(changedJSON))
		var eligible bool
		err = db.getReader().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM sessions s
			WHERE `+where+`
				AND s.id IN (SELECT value FROM json_each(?)))`, args...).Scan(&eligible)
		if err != nil {
			return false, fmt.Errorf("checking kept activity session changes: %w", err)
		}
		if eligible {
			return false, nil
		}

		candidateIDsJSON, err := json.Marshal(candidateIDs)
		if err != nil {
			return false, err
		}
		var peerMatches bool
		err = db.getReader().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1
			FROM json_each(?) changed
			JOIN messages m ON m.session_id = changed.value
			JOIN sessions s ON s.id = m.session_id
			WHERE `+usageMessageEligibility+`
				AND COALESCE(NULLIF(m.timestamp, ''), s.started_at, '') >= ?
				AND COALESCE(NULLIF(m.timestamp, ''), s.started_at, '') <= ?
				AND `+activityReportClaudePeerPredicate("(SELECT value FROM json_each(?))")+`)`,
			string(changedJSON), usageLowerBound,
			usageUpperBound, string(candidateIDsJSON), usageLowerBound,
			usageUpperBound).Scan(&peerMatches)
		if err != nil {
			return false, fmt.Errorf("checking kept activity Claude peers: %w", err)
		}
		if peerMatches {
			return false, nil
		}
	}
	return true, nil
}
