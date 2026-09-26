package clickhouse

import (
	"context"
	"database/sql"
	"errors"
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

// preparedUsageState reports whether prepared_usage can answer reads in
// place of the raw messages and usage events, and which pricing digest its
// stored price records serve. It is ready when an archive has published
// complete snapshots, the table was prepared by this binary's query, every
// session has a current snapshot, and the rows were derived from exactly
// the snapshot set that exists now. Between a push and the refresh that
// follows it, reads fall back to the raw rows rather than serve the
// previous push's usage. The stored price records serve reads only while
// the mirror still holds exactly the records they were copied from.
type preparedUsageState struct {
	ready         bool
	pricingDigest string
}

func (s *Store) preparedUsageState(ctx context.Context) (preparedUsageState, error) {
	var filled uint64
	var fingerprint, comment string
	err := s.queryRowContext(ctx, `SELECT
		(SELECT ifNull(max(toUInt64OrZero(value)),0) FROM sync_metadata WHERE startsWith(key,?)),
		(SELECT hex(SHA256(toString(arraySort(groupArray((table, name, hash_of_all_files))))))
		 FROM system.parts WHERE database = currentDatabase() AND active
		 AND table IN ('sessions', 'usage_session_snapshots', 'usage_event_prices')),
		ifNull((SELECT comment FROM system.tables WHERE database = currentDatabase() AND name = 'prepared_usage'), '')`,
		usageSnapshotReadyKeyBase+":").Scan(&filled, &fingerprint, &comment)
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("checking prepared usage readiness: %w", err)
	}
	// Existing reports remain available while every archive prepares its first
	// complete snapshot. A table prepared by another query may lack the
	// stamp columns, so it is checked before they are read.
	if filled == 0 || comment != chPreparedUsageComment() {
		return preparedUsageState{}, nil
	}
	var stored usageStamp
	err = s.queryRowContext(ctx, `SELECT snapshot_count, snapshot_hash, pricing_digest, price_count, price_hash
		FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(
		&stored.snapshotCount, &stored.snapshotHash, &stored.pricingDigest, &stored.priceCount, &stored.priceHash)
	if errors.Is(err, sql.ErrNoRows) {
		return preparedUsageState{}, nil
	}
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("reading prepared usage stamp: %w", err)
	}
	// Coverage and the live stamp read the tables named in the fingerprint,
	// so they only change when their parts do.
	s.coverageCache.mu.Lock()
	live, cached := s.coverageCache.live, s.coverageCache.fingerprint == fingerprint &&
		s.coverageCache.live.pricingDigest == stored.pricingDigest
	s.coverageCache.mu.Unlock()
	if !cached {
		live = usageStamp{pricingDigest: stored.pricingDigest}
		err = s.queryRowContext(ctx, `SELECT
			(SELECT count() FROM sessions s
			 LEFT JOIN usage_session_snapshots u ON u.id=s.id
			 WHERE u.id='' OR u.push_version<s.push_version),
			(SELECT count() FROM usage_session_snapshots),
			(SELECT sum(sipHash64(id, revision)) FROM usage_session_snapshots),
			(SELECT count() FROM usage_event_prices WHERE pricing_digest = ?),
			(SELECT sum(sipHash64(price_key)) FROM usage_event_prices WHERE pricing_digest = ?)`,
			stored.pricingDigest, stored.pricingDigest).
			Scan(&live.missing, &live.snapshotCount, &live.snapshotHash, &live.priceCount, &live.priceHash)
		if err != nil {
			return preparedUsageState{}, fmt.Errorf("checking complete usage coverage: %w", err)
		}
		s.coverageCache.mu.Lock()
		s.coverageCache.fingerprint, s.coverageCache.live = fingerprint, live
		s.coverageCache.mu.Unlock()
	}
	state := preparedUsageState{
		ready: live.missing == 0 && live.snapshotCount == stored.snapshotCount &&
			live.snapshotHash == stored.snapshotHash,
	}
	if state.ready && stored.pricingDigest != "" &&
		live.priceCount == stored.priceCount && live.priceHash == stored.priceHash {
		state.pricingDigest = stored.pricingDigest
	}
	return state, nil
}

func (s *Store) preparedUsageReady(ctx context.Context) (bool, error) {
	state, err := s.preparedUsageState(ctx)
	return state.ready, err
}

// usageCoverageCache memoizes the live stamp and coverage per parts of
// the sessions, snapshot, and price record tables.
type usageCoverageCache struct {
	mu          sync.Mutex
	fingerprint string
	live        usageStamp
}

// usageStamp identifies a snapshot set and the price records under one
// digest, as stored on prepared rows or as read from the live tables.
type usageStamp struct {
	missing, snapshotCount, snapshotHash uint64
	pricingDigest                        string
	priceCount, priceHash                uint64
}
