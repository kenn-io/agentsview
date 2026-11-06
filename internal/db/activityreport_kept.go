package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
)

type activityKept struct {
	report                   kept.Report
	revision                 int64
	read, labels             []string
	rangeStart, rangeEnd     string
	paddedLower, paddedUpper string
	startInstant, endInstant string
}

func (db *DB) activityKeptVersion(ctx context.Context) (string, int64, error) {
	db.activityKeptOnce.Do(func() {
		db.activityKept.Size = func(e activityKept) int64 {
			size := activity.EstimatedArtifactBytes(e.report.Artifacts) + int64(len(e.read))*16
			for _, label := range e.labels {
				size += int64(len(label))
			}
			return size
		}
		db.activityKept.MaxBytes = 128 << 20
	})
	var id string
	var revision, pricingRevision int64
	err := db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE(MAX(CASE WHEN key = 'database_id' THEN value END), ''),
		COALESCE(MAX(CASE WHEN key = 'usage_change_revision' THEN CAST(value AS INTEGER) END), 0),
		COALESCE(MAX(CASE WHEN key = ? THEN CAST(value AS INTEGER) END), 0)
		FROM archive_metadata WHERE key IN ('database_id', 'usage_change_revision', ?)`,
		archiveMetadataUsagePricingRevisionKey, archiveMetadataUsagePricingRevisionKey,
	).Scan(&id, &revision, &pricingRevision)
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	if id == "" {
		return "", revision, nil
	}
	return fmt.Sprintf("%s|%d", id, pricingRevision), revision, nil
}

func (db *DB) keptActivityReport(ctx context.Context, selection, version string, revision int64) (kept.Report, bool, error) {
	entries, ok := db.activityKept.Get(selection, version)
	if !ok || revision < entries[0].revision {
		return kept.Report{}, false, nil
	}
	entry := entries[0]
	changed, latestRevision, err := db.loadKeptActivityUsageChanges(ctx, entry.revision)
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
		var overlaps bool
		err = db.getReader().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM json_each(?) changed JOIN sessions s ON s.id = changed.value
			WHERE (`+activityReportSessionWindowPredicate()+`)
			OR EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id
				AND m.timestamp >= ? AND m.timestamp < ?
				AND agentsview_timestamp_unix_micro(m.timestamp) IS NOT NULL
				AND agentsview_timestamp_unix_micro(m.timestamp) >= agentsview_timestamp_unix_micro(?)
				AND agentsview_timestamp_unix_micro(m.timestamp) < agentsview_timestamp_unix_micro(?))
			LIMIT 1)`, string(ids), entry.rangeStart, entry.rangeStart, entry.rangeEnd,
			entry.paddedLower, entry.paddedUpper, entry.startInstant, entry.endInstant).Scan(&overlaps)
		if err != nil {
			return kept.Report{}, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
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
	if latestRevision > revision {
		revision = latestRevision
	}
	entry.revision = revision
	db.activityKept.Put(selection, version, []activityKept{entry})
	return entry.report, true, nil
}

func (db *DB) loadKeptActivityUsageChanges(ctx context.Context, afterRevision int64) ([]string, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, revision
		FROM usage_session_changes
		WHERE revision > ?
		ORDER BY revision`, afterRevision)
	if err != nil {
		return nil, afterRevision, fmt.Errorf("listing kept activity usage changes: %w", err)
	}
	defer rows.Close()

	var ids []string
	latestRevision := afterRevision
	for rows.Next() {
		var id string
		var changedRevision int64
		if err := rows.Scan(&id, &changedRevision); err != nil {
			return nil, afterRevision, fmt.Errorf("scanning kept activity usage change: %w", err)
		}
		ids = append(ids, id)
		if changedRevision > latestRevision {
			latestRevision = changedRevision
		}
	}
	if err := rows.Err(); err != nil {
		return nil, afterRevision, fmt.Errorf("iterating kept activity usage changes: %w", err)
	}
	return ids, latestRevision, nil
}
