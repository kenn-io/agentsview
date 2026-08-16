package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/activity"
)

const activityReportProjectIdentityGenerationKey = "activity_report_project_identity_generation"

func (s *Store) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	var probe activity.SourceProbe
	err := s.pg.QueryRowContext(ctx, `SELECT
		session_probe.session_count,
		session_probe.max_updated_at,
		session_probe.max_data_version,
		session_probe.message_count,
		COALESCE((SELECT MAX(id) FROM usage_events), 0),
		COALESCE((SELECT MAX(updated_at)::text FROM model_pricing), ''),
		COALESCE((SELECT value::bigint FROM sync_metadata WHERE key = $1), 0)
	FROM (
		SELECT COUNT(*) AS session_count,
			COALESCE(MAX(updated_at)::text, '') AS max_updated_at,
			COALESCE(MAX(data_version), 0) AS max_data_version,
			COALESCE(SUM(message_count), 0) AS message_count
		FROM sessions
	) AS session_probe`, activityReportProjectIdentityGenerationKey).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxMessageID, &probe.MaxUsageID, &probe.MaxPricingUpdated,
		&probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing pg activity report source: %w", err)
	}
	return probe, nil
}
