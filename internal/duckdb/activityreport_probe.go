package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
)

func (s *Store) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	var probe activity.SourceProbe
	err := s.queryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions),
		COALESCE(CAST((SELECT MAX(local_modified_at) FROM sessions) AS VARCHAR), ''),
		COALESCE((SELECT MAX(data_version) FROM sessions), 0),
		COALESCE((SELECT MAX(id) FROM messages), 0),
		COALESCE((SELECT MAX(id) FROM usage_events), 0),
		GREATEST(
			COALESCE(CAST((SELECT MAX(updated_at) FROM model_pricing) AS VARCHAR), ''),
			COALESCE(CAST((SELECT MAX(updated_at) FROM genai_pricing) AS VARCHAR), '')
		),
		COALESCE(CAST((SELECT value FROM sync_metadata WHERE key = ?) AS BIGINT), 0)`,
		identityRevisionMetadataKey).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxMessageID, &probe.MaxUsageID, &probe.MaxPricingUpdated,
		&probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing duckdb activity report source: %w", err)
	}
	return probe, nil
}
