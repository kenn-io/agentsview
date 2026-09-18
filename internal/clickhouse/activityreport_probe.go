package clickhouse

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
		(SELECT toInt64(count()) FROM sessions),
		ifNull(toString((SELECT max(local_modified_at) FROM sessions)), ''),
		ifNull((SELECT max(data_version) FROM sessions), toInt64(0)),
		ifNull((SELECT max(id) FROM messages), toInt64(0)),
		ifNull((SELECT max(id) FROM usage_events), toInt64(0)),
		greatest(
			ifNull((SELECT max(updated_at) FROM model_pricing), ''),
			ifNull((SELECT max(updated_at) FROM genai_pricing), '')
		),
		ifNull((
			SELECT max(toInt64OrZero(value))
			FROM sync_metadata
			WHERE startsWith(key, ?)
		), toInt64(0))`,
		identityRevisionKeyBase).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxMessageID, &probe.MaxUsageID, &probe.MaxPricingUpdated,
		&probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing clickhouse activity report source: %w", err)
	}
	return probe, nil
}
