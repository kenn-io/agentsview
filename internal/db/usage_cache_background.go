package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sort"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

// Match the install transaction bound so foreground waiters are released as
// soon as each committed batch is available.
const usageCacheBackfillBatchSize = usageFillInstallBatchSize

const (
	usageCacheVacuumFreelistThreshold = 4096
	usageCacheVacuumPages             = 256
)

// StartUsageCacheBackfill starts one newest-first cache population pass.
// Installed session versions remain the source of truth, so restarting a pass
// naturally skips completed work.
func (db *DB) StartUsageCacheBackfill(ctx context.Context) error {
	if db.readOnly {
		return fmt.Errorf("usage cache background backfill requires a writable archive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	db.usageBackfillMu.Lock()
	db.usageBackfillEnabled = true
	if db.usageBackfillDone != nil {
		select {
		case <-db.usageBackfillDone:
			db.usageBackfillDone = nil
		default:
			db.usageBackfillMu.Unlock()
			return nil
		}
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	db.usageBackfillCancel = cancel
	db.usageBackfillDone = done
	db.usageBackfillErr = nil
	started := db.usageBackfillStarted
	db.usageBackfillMu.Unlock()

	go func() {
		err := db.runUsageCacheBackfill(workerCtx)
		db.usageBackfillMu.Lock()
		db.usageBackfillErr = err
		close(done)
		db.usageBackfillMu.Unlock()
	}()
	if started != nil {
		started()
	}
	return nil
}

// SetUsageCacheBackfillStarted records a callback for daemon lifecycle
// integration. The callback must return promptly; it runs after each new pass
// starts, including passes restarted after an archive reopen.
func (db *DB) SetUsageCacheBackfillStarted(started func()) {
	db.usageBackfillMu.Lock()
	db.usageBackfillStarted = started
	active := false
	if db.usageBackfillDone != nil {
		select {
		case <-db.usageBackfillDone:
		default:
			active = true
		}
	}
	db.usageBackfillMu.Unlock()
	if active && started != nil {
		started()
	}
}

// WaitUsageCacheBackfill waits for the current pass, if any.
func (db *DB) WaitUsageCacheBackfill(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	db.usageBackfillMu.Lock()
	done := db.usageBackfillDone
	db.usageBackfillMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	db.usageBackfillMu.Lock()
	err := db.usageBackfillErr
	db.usageBackfillMu.Unlock()
	return err
}

// restartUsageCacheBackfillIfEnabled resumes the background coverage pass
// after maintenance stopped it, but only when this process explicitly enabled
// it (daemon lifecycle). A CLI resync or compaction reopening the archive
// must not kick off a full background scan on its own.
func (db *DB) restartUsageCacheBackfillIfEnabled() error {
	db.usageBackfillMu.Lock()
	enabled := db.usageBackfillEnabled
	db.usageBackfillMu.Unlock()
	if !enabled {
		return nil
	}
	return db.StartUsageCacheBackfill(context.Background())
}

// StopUsageCacheBackfill cancels and joins the active pass before cache handles
// are closed.
func (db *DB) StopUsageCacheBackfill() {
	db.usageBackfillMu.Lock()
	cancel := db.usageBackfillCancel
	done := db.usageBackfillDone
	db.usageBackfillMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// runUsageCacheBackfill runs one coverage pass. Facts are filled per session
// from their own archive read snapshot and rollups aggregate only committed
// facts, so a pass no longer has to be restarted because the archive was
// written while it ran. Sessions written during the pass are picked up by
// their own mutation notification.
func (db *DB) runUsageCacheBackfill(ctx context.Context) error {
	snapshot, err := db.captureUsageQuery(
		ctx, UsageFilter{}, usageQueryKindActivity)
	if err != nil {
		return err
	}
	return db.runUsageCacheBackfillPass(ctx, time.Now(), snapshot)
}

func (db *DB) runUsageCacheBackfillPass(
	ctx context.Context, started time.Time, snapshot usageQuerySnapshot,
) error {
	cache, release, err := db.usageCache.acquireGeneration(ctx, snapshot.DatabaseID)
	if err != nil {
		return err
	}
	defer release()
	if cache == nil || cache.fill == nil || cache.rollup == nil {
		return fmt.Errorf("usage cache generation is not attached to the archive")
	}
	locations, err := usageBackfillLocations(ctx, cache, snapshot.location)
	if err != nil {
		return err
	}
	if err := cache.sweepDeletionJournal(ctx, db); err != nil {
		return err
	}
	activity, err := db.usageBackfillActivity(ctx)
	if err != nil {
		return err
	}
	orderUsageBackfillSnapshot(&snapshot, activity)
	resolver := export.NewPricingResolver(snapshot.PricingRows)
	covered, deleted := 0, 0
	var dailyRows, exceptionGroups, exceptionRows int64
	allResults := make(map[string]usageFillResult, len(snapshot.Versions))
	for start := 0; start < len(snapshot.Versions); start += usageCacheBackfillBatchSize {
		end := min(start+usageCacheBackfillBatchSize, len(snapshot.Versions))
		cursorTarget := int64(0)
		if start == 0 {
			cursorTarget = snapshot.CursorHighWater
		}
		results, fillErr := cache.fill.FillBackground(
			ctx, snapshot.Versions[start:end], cursorTarget)
		if fillErr != nil {
			return fillErr
		}
		batch := snapshot
		batch.Sessions = snapshot.Sessions[start:end]
		batch.Versions = snapshot.Versions[start:end]
		batch.CursorHighWater = cursorTarget
		for _, location := range locations {
			zoned := batch
			zoned.location = location
			if _, metrics, err := cache.rollup.Ensure(ctx, zoned, results, resolver); err != nil {
				return fmt.Errorf("building usage rollups during backfill: %w", err)
			} else {
				dailyRows += metrics.DailyRows
				exceptionGroups += metrics.ExceptionGroups
				exceptionRows += metrics.ExceptionRows
			}
		}
		if _, err := cache.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
			return fmt.Errorf("optimizing usage cache during backfill: %w", err)
		}
		if end < len(snapshot.Versions) {
			cache.fill.maintainBetweenInstallBatches(ctx)
		}
		for id, result := range results {
			allResults[id] = result
			if result.Deleted {
				deleted++
			} else {
				covered++
			}
		}
	}
	// Filling a later batch can invalidate a rollup install from an
	// earlier batch when the filled session adds a sibling for an
	// identity the earlier batch had finalized. Re-verify the whole
	// snapshot once after every fact is filled, so the pass never
	// completes with rollups it deleted itself.
	if len(snapshot.Versions) > usageCacheBackfillBatchSize {
		for _, location := range locations {
			zoned := snapshot
			zoned.location = location
			if _, metrics, err := cache.rollup.Ensure(
				ctx, zoned, allResults, resolver); err != nil {
				return fmt.Errorf(
					"rebuilding invalidated usage rollups during backfill: %w", err)
			} else {
				dailyRows += metrics.DailyRows
				exceptionGroups += metrics.ExceptionGroups
				exceptionRows += metrics.ExceptionRows
			}
		}
	}
	if len(snapshot.Versions) == 0 {
		results, err := cache.fill.FillBackground(ctx, nil, snapshot.CursorHighWater)
		if err != nil {
			return err
		}
		for _, location := range locations {
			zoned := snapshot
			zoned.location = location
			if _, metrics, err := cache.rollup.Ensure(ctx, zoned, results, resolver); err != nil {
				return fmt.Errorf("building Cursor usage rollups during backfill: %w", err)
			} else {
				dailyRows += metrics.DailyRows
				exceptionGroups += metrics.ExceptionGroups
				exceptionRows += metrics.ExceptionRows
			}
		}
	}
	if err := cache.sweepDeletionJournal(ctx, db); err != nil {
		return err
	}
	if _, err := cache.incrementalVacuum(
		ctx, usageCacheVacuumFreelistThreshold, usageCacheVacuumPages,
	); err != nil {
		return err
	}
	if _, err := cache.db.ExecContext(ctx, `ANALYZE`); err != nil {
		return fmt.Errorf("analyzing completed usage cache: %w", err)
	}
	if _, err := cache.db.ExecContext(ctx, `
		UPDATE usage_cache_metadata SET value = ? WHERE key = ?`,
		time.Now().UTC().Format(time.RFC3339Nano),
		usageCacheMetadataBackfillCompletedAt,
	); err != nil {
		return fmt.Errorf("recording usage cache completion time: %w", err)
	}
	log.Printf("usage cache backfill: covered %d sessions (%d deleted), built %d daily rows and %d exception rows in %d groups in %s",
		covered, deleted, dailyRows, exceptionRows, exceptionGroups,
		time.Since(started).Round(time.Millisecond))
	return nil
}

func usageBackfillLocations(
	ctx context.Context, cache *usageCache, local *time.Location,
) ([]*time.Location, error) {
	localIdentity := usageTimezoneIdentityFor(local, nil)
	rows, err := cache.db.QueryContext(ctx, `SELECT timezone_name
		FROM usage_rollup_timezones
		WHERE timezone_key != ? AND timezone_name NOT IN ('', 'Local')
		ORDER BY last_requested_at DESC LIMIT 8`, localIdentity.Key)
	if err != nil {
		return nil, fmt.Errorf("reading recent usage timezones: %w", err)
	}
	defer rows.Close()
	locations := []*time.Location{local}
	seen := map[string]bool{localIdentity.Key: true}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		location, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		key := usageTimezoneIdentityFor(location, nil).Key
		if !seen[key] {
			seen[key] = true
			locations = append(locations, location)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return locations, nil
}

func (db *DB) usageBackfillActivity(ctx context.Context) (map[string]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, MAX(activity_at) FROM (
			SELECT m.session_id, m.timestamp AS activity_at
			FROM messages m INDEXED BY idx_messages_usage_timestamp
			WHERE `+usageMessageSourceEligibility+`
			UNION ALL
			SELECT m.session_id, m.timestamp AS activity_at
			FROM messages m INDEXED BY idx_messages_activity_timestamp
			WHERE m.role = 'assistant' AND m.model != '<synthetic>'
			UNION ALL
			SELECT ue.session_id, ue.occurred_at AS activity_at
			FROM usage_events ue INDEXED BY idx_usage_events_session
			WHERE ue.model != ''
		) WHERE activity_at IS NOT NULL AND activity_at != ''
		GROUP BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("ordering usage cache backfill: %w", err)
	}
	defer rows.Close()
	activity := make(map[string]string)
	for rows.Next() {
		var sessionID, timestamp string
		if err := rows.Scan(&sessionID, &timestamp); err != nil {
			return nil, err
		}
		activity[sessionID] = timestamp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return activity, nil
}

func orderUsageBackfillSnapshot(
	snapshot *usageQuerySnapshot, activity map[string]string,
) {
	type record struct {
		session usageQuerySession
		version usageSourceVersion
		latest  string
	}
	records := make([]record, len(snapshot.Sessions))
	for index, session := range snapshot.Sessions {
		latest := maxTimestampText(
			activity[session.ID], session.EndedAt, session.StartedAt)
		if latest == "" {
			latest = session.CreatedAt
		}
		records[index] = record{
			session: session, version: snapshot.Versions[index], latest: latest,
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].latest != records[j].latest {
			return records[i].latest > records[j].latest
		}
		return records[i].session.ID < records[j].session.ID
	})
	for index, record := range records {
		snapshot.Sessions[index] = record.session
		snapshot.Versions[index] = record.version
	}
}

func maxTimestampText(values ...string) string {
	var result string
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func (cache *usageCache) sweepDeletionJournal(ctx context.Context, archive *DB) error {
	var raw string
	if err := cache.db.QueryRowContext(ctx, `
		SELECT value FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataDeletionRevision,
	).Scan(&raw); err != nil {
		return err
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		return fmt.Errorf("invalid usage cache deletion revision %q", raw)
	}
	through, err := archive.SessionDeletionPublicationRevision(ctx)
	if err != nil || through == after {
		return err
	}
	tombstones, err := archive.LoadSessionDeletionDelta(
		ctx, after, through, nil, nil)
	if err != nil {
		return err
	}
	conn, err := cache.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// BEGIN IMMEDIATE: this transaction reads identities before its first
	// delete, so a deferred begin could fail with SQLITE_BUSY_SNAPSHOT
	// under a concurrent fill or install commit.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("locking usage cache for deletion sweep: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	tombstoneIDs := make([]string, 0, len(tombstones))
	for _, tombstone := range tombstones {
		tombstoneIDs = append(tombstoneIDs, tombstone.SessionID)
	}
	removed, err := usageFactIdentitiesForSessions(ctx, conn, tombstoneIDs)
	if err != nil {
		return err
	}
	excluded := make(map[string]bool, len(tombstoneIDs))
	for _, sessionID := range tombstoneIDs {
		excluded[sessionID] = true
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM usage_rollup_installs WHERE session_id = ?`,
			sessionID,
		); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM usage_cached_sessions WHERE session_id = ?`,
			sessionID,
		); err != nil {
			return err
		}
	}
	// Rebuild the survivors' rollups so groups the deleted sessions kept
	// irreducible can finalize again.
	if err := invalidateUsageDedupSharers(ctx, conn, removed, excluded); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE usage_cache_metadata
		SET value = CAST(MAX(CAST(value AS INTEGER), ?) AS TEXT)
		WHERE key = ?`, through, usageCacheMetadataDeletionRevision,
	); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing usage cache deletion sweep: %w", err)
	}
	committed = true
	return nil
}

func (cache *usageCache) incrementalVacuum(
	ctx context.Context, threshold, pages int,
) (bool, error) {
	if threshold < 0 || pages <= 0 {
		return false, fmt.Errorf("invalid usage cache vacuum bounds")
	}
	var freelist int
	if err := cache.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freelist); err != nil {
		return false, err
	}
	if freelist <= threshold {
		return false, nil
	}
	if _, err := cache.db.ExecContext(ctx,
		fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)); err != nil {
		return false, err
	}
	return true, nil
}

func (c *usageFillCoordinator) maintainBetweenInstallBatches(ctx context.Context) {
	if err := c.cache.sweepDeletionJournal(ctx, c.archive); err != nil &&
		!errors.Is(err, context.Canceled) {
		log.Printf("usage cache deletion sweep: %v", err)
	}
	if _, err := c.cache.incrementalVacuum(
		ctx, usageCacheVacuumFreelistThreshold, usageCacheVacuumPages,
	); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("usage cache incremental vacuum: %v", err)
	}
	if c.observer.afterMaintenance != nil {
		c.observer.afterMaintenance()
	}
	runtime.Gosched()
}
