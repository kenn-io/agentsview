package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"reflect"
	"slices"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type activityKept struct {
	report                           kept.Report
	revision                         int64
	read, labels                     []string
	filter                           AnalyticsFilter
	rangeStart, rangeEnd             string
	usageLowerBound, usageUpperBound string
}

func newActivityKeptMemo() kept.Memo[activityKept] {
	return kept.Memo[activityKept]{
		Size: func(e activityKept) int64 {
			size := activity.EstimatedArtifactBytes(e.report.Artifacts) + int64(cap(e.read))*16
			for _, id := range e.read {
				size += int64(len(id))
			}
			for _, label := range e.labels {
				size += int64(len(label))
			}
			return size
		},
		MaxBytes: 128 << 20,
	}
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

func (db *DB) keptActivityReport(ctx context.Context, selection, version string, revision int64) (kept.Report, bool, error) {
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
		ids, err := json.Marshal(changed)
		if err != nil {
			return kept.Report{}, false, err
		}
		where, args := activityReportSessionSelectionWhere(
			entry.filter, entry.rangeStart, entry.rangeEnd,
		)
		args = append(args, string(ids))
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

		readIDs, err := json.Marshal(entry.read)
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
			string(ids), entry.usageLowerBound,
			entry.usageUpperBound, string(readIDs), entry.usageLowerBound,
			entry.usageUpperBound).Scan(&peerMatches)
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
	}
	db.activityKept.Put(selection, version, []activityKept{entry})
	return entry.report, true, nil
}
