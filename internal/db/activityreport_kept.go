package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type activityKept struct {
	report       kept.Report
	size         int64
	revision     int64
	read, labels []string
}

func newActivityKeptMemo() kept.Memo[activityKept] {
	return kept.Memo[activityKept]{
		Size:     func(e activityKept) int64 { return e.size },
		MaxBytes: 128 << 20,
	}
}

func activityKeptSize(e activityKept) int64 {
	size := activity.EstimatedArtifactBytes(e.report.Artifacts) + int64(cap(e.read))*16
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
	id, pricingRevision, err := db.databaseIDAndUsagePricingRevision(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	revision, err := db.UsageChangeRevision(ctx)
	if err != nil {
		return "", 0, err
	}
	if id == "" {
		return "", revision, nil
	}
	return fmt.Sprintf("%s|%d", id, pricingRevision), revision, nil
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
	changed, err := db.LoadUsageChanges(ctx, entry.revision, math.MaxInt64)
	if err != nil {
		return kept.Report{}, false, err
	}
	for _, id := range changed {
		if _, found := slices.BinarySearch(entry.read, id); found {
			return kept.Report{}, false, nil
		}
	}
	if len(changed) > 0 {
		changedJSON, err := json.Marshal(changed)
		if err != nil {
			return kept.Report{}, false, err
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
			return kept.Report{}, false, fmt.Errorf("checking kept activity session changes: %w", err)
		}
		if eligible {
			return kept.Report{}, false, nil
		}

		candidateIDs := make([]string, len(entry.report.Artifacts.Sessions))
		for i, session := range entry.report.Artifacts.Sessions {
			candidateIDs[i] = session.SessionID
		}
		candidateIDsJSON, err := json.Marshal(candidateIDs)
		if err != nil {
			return kept.Report{}, false, err
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
			return kept.Report{}, false, fmt.Errorf("checking kept activity Claude peers: %w", err)
		}
		if peerMatches {
			return kept.Report{}, false, nil
		}
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
