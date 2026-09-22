package clickhouse

import (
	"context"
	"fmt"
	"sync"

	"go.kenn.io/agentsview/internal/activity"
)

type activityProbeCache struct {
	mu          sync.Mutex
	fingerprint string
	probe       activity.SourceProbe
}

func (s *Store) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	var fingerprint string
	err := s.queryRowContext(ctx, `SELECT
		hex(SHA256(toString(arraySort(groupArray((table, name, hash_of_all_files))))))
		FROM system.parts
		WHERE database = currentDatabase() AND active
		AND table IN ('sessions', 'messages', 'usage_events', 'model_pricing', 'genai_pricing', 'sync_metadata')`).Scan(&fingerprint)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("reading clickhouse activity source parts: %w", err)
	}
	s.probeCache.mu.Lock()
	probe, cached := s.probeCache.probe, s.probeCache.fingerprint == fingerprint
	s.probeCache.mu.Unlock()
	if cached {
		return probe, nil
	}

	// Parts also change during merges. Recompute the original probe so a merge
	// alone cannot invalidate report tokens or reset session pagination.
	probe, err = s.readActivitySourceProbe(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	s.probeCache.mu.Lock()
	s.probeCache.fingerprint, s.probeCache.probe = fingerprint, probe
	s.probeCache.mu.Unlock()
	return probe, nil
}

func (s *Store) readActivitySourceProbe(ctx context.Context) (activity.SourceProbe, error) {
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
