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
		AND table IN ('sessions', 'messages', 'usage_events', 'model_pricing', 'genai_pricing', 'sync_metadata', 'usage_session_snapshots', 'prepared_usage')`).Scan(&fingerprint)
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
	ready, err := s.preparedUsageReady(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	if ready {
		// Count and sum row hashes so merges and row ordering do not change the
		// fingerprint, while duplicate rows still contribute to it.
		err = s.queryRowContext(ctx, `SELECT hex(SHA256(toString(tuple(count(),sumWithOverflow(row_hash)))))
			FROM (SELECT reinterpretAsUInt128(sipHash128Reference(tuple(*))) AS row_hash
			FROM prepared_usage) SETTINGS final=0`).Scan(&probe.PreparedUsageFingerprint)
		if err != nil {
			return activity.SourceProbe{}, fmt.Errorf("probing prepared usage: %w", err)
		}
	}
	return probe, nil
}

func (s *Store) preparedUsageReady(ctx context.Context) (bool, error) {
	var refreshed, filled uint64
	var fingerprint string
	err := s.queryRowContext(ctx, `SELECT
		(SELECT ifNull(max(toUnixTimestamp(last_success_time)),0) FROM system.view_refreshes
		 WHERE database=currentDatabase() AND view='prepare_usage'),
		(SELECT ifNull(max(toUInt64OrZero(value)),0) FROM sync_metadata WHERE startsWith(key,?)),
		(SELECT hex(SHA256(toString(arraySort(groupArray((table, name, hash_of_all_files))))))
		 FROM system.parts WHERE database = currentDatabase() AND active
		 AND table IN ('sessions', 'usage_session_snapshots'))`,
		usageSnapshotReadyKeyBase+":").Scan(&refreshed, &filled, &fingerprint)
	if err != nil {
		return false, fmt.Errorf("checking prepared usage refresh: %w", err)
	}
	// Existing reports remain available while every archive prepares its first
	// complete snapshot. The refresh must start after the backfill finishes.
	if filled == 0 || refreshed <= filled {
		return false, nil
	}
	// Coverage joins every session with its snapshot; both tables are named
	// in the fingerprint, so the answer only changes when their parts do.
	s.coverageCache.mu.Lock()
	covered, cached := s.coverageCache.covered, s.coverageCache.fingerprint == fingerprint
	s.coverageCache.mu.Unlock()
	if cached {
		return covered, nil
	}
	var missing uint64
	err = s.queryRowContext(ctx, `SELECT count() FROM sessions s
		LEFT JOIN usage_session_snapshots u ON u.id=s.id
		WHERE u.id='' OR u.push_version<s.push_version`).Scan(&missing)
	if err != nil {
		return false, fmt.Errorf("checking complete usage coverage: %w", err)
	}
	s.coverageCache.mu.Lock()
	s.coverageCache.fingerprint, s.coverageCache.covered = fingerprint, missing == 0
	s.coverageCache.mu.Unlock()
	return missing == 0, nil
}

type usageCoverageCache struct {
	mu          sync.Mutex
	fingerprint string
	covered     bool
}
