package db

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
)

func (db *DB) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	var probe activity.SourceProbe
	err := db.getReader().QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions),
		COALESCE((SELECT MAX(local_modified_at) FROM sessions), ''),
		COALESCE((SELECT MAX(data_version) FROM sessions), 0),
		COALESCE((SELECT MAX(id) FROM messages), 0),
		COALESCE((SELECT MAX(id) FROM usage_events), 0),
		MAX(
			COALESCE((SELECT MAX(updated_at) FROM model_pricing), ''),
			COALESCE((SELECT MAX(updated_at) FROM genai_pricing), '')
		),
		COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata
			WHERE key = ?), 0)`, archiveMetadataProjectIdentityRevisionKey).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxMessageID, &probe.MaxUsageID, &probe.MaxPricingUpdated,
		&probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing activity report source: %w", err)
	}
	return probe, nil
}
