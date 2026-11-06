package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/kept"
)

type activityKept struct {
	report       kept.Report
	revision     int64
	read         []string
	lower, upper string
}

func (db *DB) activityKeptVersion(ctx context.Context) (string, int64, error) {
	db.activityKeptOnce.Do(func() {
		db.activityKept.Size = func(e activityKept) int64 {
			return activity.EstimatedArtifactBytes(e.report.Artifacts) + int64(len(e.read))*16
		}
		db.activityKept.MaxBytes = 128 << 20
	})
	var id, identity string
	var revision int64
	err := db.getReader().QueryRowContext(ctx, `SELECT
		COALESCE(MAX(CASE WHEN key = 'database_id' THEN value END), ''),
		COALESCE(MAX(CASE WHEN key = 'usage_change_revision' THEN CAST(value AS INTEGER) END), 0),
		COALESCE(MAX(CASE WHEN key = 'project_identity_publication_revision' THEN value END), '0')
		FROM archive_metadata WHERE key IN ('database_id', 'usage_change_revision', 'project_identity_publication_revision')`).Scan(&id, &revision, &identity)
	if err != nil {
		return "", 0, fmt.Errorf("reading kept activity version: %w", err)
	}
	if id == "" {
		return "", revision, nil
	}
	pricing, err := db.UsagePricingDigest(ctx)
	if err != nil {
		return "", 0, err
	}
	return id + "|" + identity + "|" + pricing, revision, nil
}

func (db *DB) keptActivityReport(ctx context.Context, selection, version string, revision int64) (kept.Report, bool, error) {
	entries, ok := db.activityKept.Get(selection, version)
	if !ok || revision < entries[0].revision {
		return kept.Report{}, false, nil
	}
	entry := entries[0]
	changed, err := db.LoadUsageChanges(ctx, entry.revision, revision)
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
			WHERE (COALESCE(NULLIF(s.started_at, ''), s.created_at) < ?
				AND COALESCE(NULLIF(s.ended_at, ''),
					(SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = s.id AND m.timestamp != ''),
					NULLIF(s.started_at, ''), s.created_at) >= ?)
			OR EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND m.timestamp >= ? AND m.timestamp < ?)
			OR EXISTS (SELECT 1 FROM tool_result_events tre WHERE tre.session_id = s.id
				AND tre.source = 'tool_execution' AND tre.status IN ('completed', 'errored')
				AND agentsview_timestamp_unix_micro(tre.timestamp) IS NOT NULL
				AND tre.timestamp >= ? AND tre.timestamp < ?)
			LIMIT 1)`, string(ids), entry.upper, entry.lower, entry.lower, entry.upper, entry.lower, entry.upper).Scan(&overlaps)
		if err != nil {
			return kept.Report{}, false, fmt.Errorf("checking kept activity changes: %w", err)
		}
		if overlaps {
			return kept.Report{}, false, nil
		}
	}
	entry.revision = revision
	db.activityKept.Put(selection, version, []activityKept{entry})
	return entry.report, true, nil
}
