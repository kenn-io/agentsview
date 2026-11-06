package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
)

type usageZoneCoverage struct {
	id                          int64
	revision                    sql.NullInt64
	installRevision, installSeq int64
	pricing                     string
	foreignPricing              bool
}

func readUsageZoneCoverage(ctx context.Context, q sessionExportQuerier, key string) (usageZoneCoverage, error) {
	var zone usageZoneCoverage
	err := q.QueryRowContext(ctx, `
		SELECT id, change_revision, change_install_revision, change_pricing,
			EXISTS(SELECT 1 FROM usage_rollup_installs
				WHERE timezone_id = z.id
				AND install_revision >= z.change_install_revision
				AND source_pricing <> z.change_pricing)
		FROM usage_rollup_timezones z WHERE timezone_key = ?`, key,
	).Scan(&zone.id, &zone.revision, &zone.installRevision, &zone.pricing, &zone.foreignPricing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zone, err
	}
	err = q.QueryRowContext(ctx, `
		SELECT CAST(value AS INTEGER) FROM usage_cache_metadata WHERE key = ?`,
		usageCacheMetadataNextRollupRevision).Scan(&zone.installSeq)
	return zone, err
}

// A request only advances an existing claim. A completed archive pass can
// establish one. The compare-and-set makes concurrent claim losers harmless.
func (cache *usageCache) claimUsageZone(ctx context.Context, identity usageTimezoneIdentity, zone usageZoneCoverage, revision int64, catalog string, installs map[string]usageRollupInstall) error {
	if zone.revision.Valid && zone.revision.Int64 == revision &&
		zone.installRevision == zone.installSeq && zone.pricing == catalog {
		return nil
	}
	stamps := make([][2]int64, 0, len(installs))
	for _, install := range installs {
		stamps = append(stamps, [2]int64{install.ID, install.InstallRevision})
	}
	encoded, err := json.Marshal(stamps)
	if err != nil {
		return err
	}
	_, err = cache.db.ExecContext(ctx, `
		-- A completed pass can establish this timezone's first claim.
		INSERT INTO usage_rollup_timezones (
			timezone_key, timezone_name, interval_fingerprint, last_requested_at,
			change_revision, change_install_revision, change_pricing)
		-- Cap coverage at the oldest source revision installed since capture.
		SELECT ?, ?, ?, '', MIN(?, COALESCE((
			SELECT MIN(source_change_revision) FROM usage_rollup_installs
			WHERE timezone_id = (
				SELECT id FROM usage_rollup_timezones WHERE timezone_key = ?)
			AND install_revision >= ?), ?)), ?, ?
		-- Refuse a claim when any install since capture used another catalog.
		WHERE NOT EXISTS (
			SELECT 1 FROM usage_rollup_installs WHERE timezone_id = (
				SELECT id FROM usage_rollup_timezones WHERE timezone_key = ?)
			AND install_revision >= ? AND source_pricing <> ?)
		-- Every checked install must survive unchanged, including after rowid reuse.
		AND (SELECT COUNT(*) FROM json_each(?) expected
			JOIN usage_rollup_installs i ON i.id = json_extract(expected.value, '$[0]')
			AND i.install_revision = json_extract(expected.value, '$[1]')) = ?
		-- Advance an existing claim with the captured revision, sequence, and catalog.
		ON CONFLICT(timezone_key) DO UPDATE SET
			change_revision = excluded.change_revision,
			change_install_revision = excluded.change_install_revision,
			change_pricing = excluded.change_pricing
		-- Update only the claim observed before preparation; concurrent winners keep theirs.
		WHERE usage_rollup_timezones.change_revision IS ?
			AND usage_rollup_timezones.change_install_revision = ?`,
		identity.Key, identity.Name, identity.IntervalFingerprint, revision, identity.Key, zone.installSeq, revision, zone.installSeq, catalog,
		identity.Key, zone.installSeq, catalog, string(encoded), len(stamps), zone.revision, zone.installRevision)
	return err
}

func (db *DB) prepareJournaledUsageWindow(ctx context.Context, filter UsageFilter, kind usageQueryKind, cursor bool, report func(string)) (usagePreparedWindow, bool, error) {
	started := time.Now()
	databaseID, err := db.GetDatabaseID(ctx)
	if err != nil {
		return usagePreparedWindow{}, false, err
	}
	cache, release, err := db.usageCache.acquireGeneration(ctx, databaseID)
	if err != nil {
		return usagePreparedWindow{}, false, err
	}
	defer release()
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	zone, lost, window, err := cache.captureUsageZoneWindow(ctx, identity, filter)
	if err != nil {
		return usagePreparedWindow{}, false, err
	}
	fallback := func() error {
		if !db.isUsageBackfillEnabled() {
			return nil
		}
		locations, err := usageBackfillLocations(ctx, cache, time.Local) //nolint:forbidigo // Match the pass's process-local zone and retained explicit zones.
		if err != nil {
			return err
		}
		// An unretained zone cannot gain coverage from this pass. Serving its
		// fallback updates recency, making a later request eligible instead.
		for _, location := range locations {
			if usageTimezoneIdentityFor(location, nil).Key == identity.Key {
				// The pass belongs to the archive; StopUsageCacheBackfill joins it.
				return db.startUsageCacheBackfill(context.Background(), false)
			}
		}
		return nil
	}
	if !zone.revision.Valid || zone.foreignPricing {
		if zone.id == 0 {
			_, err = cache.db.ExecContext(ctx, `
				INSERT INTO usage_rollup_timezones (
					timezone_key, timezone_name, interval_fingerprint, last_requested_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(timezone_key) DO NOTHING`,
				identity.Key, identity.Name, identity.IntervalFingerprint,
				time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return usagePreparedWindow{}, false, err
			}
		}
		return usagePreparedWindow{}, false, fallback()
	}
	snapshot, refresh, refreshIDs, trusted, err := db.captureJournaledUsageWindow(ctx, filter, kind, cursor, databaseID, zone, lost, window)
	if err != nil {
		return usagePreparedWindow{}, false, err
	}
	if !trusted {
		return usagePreparedWindow{snapshot: snapshot}, false, fallback()
	}
	captureDuration := time.Since(started)
	installs, timings, err := cache.refreshJournaledUsageWindow(ctx, &snapshot, refresh, refreshIDs, report)
	if err != nil {
		return usagePreparedWindow{}, false, err
	}
	timings.capture = captureDuration
	timings.candidates = len(snapshot.Sessions)
	err = cache.claimUsageZone(ctx, identity, zone, snapshot.ChangeRevision, snapshot.PricingIdentity, installs)
	return usagePreparedWindow{snapshot: snapshot, installs: installs, timings: timings, journaled: true, installSeq: zone.installSeq}, true, err
}

func (cache *usageCache) captureUsageZoneWindow(ctx context.Context, identity usageTimezoneIdentity, filter UsageFilter) (usageZoneCoverage, []string, []string, error) {
	// Window discovery and missing-install discovery share the same cache view.
	var zone usageZoneCoverage
	tx, err := cache.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zone, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	zone, err = readUsageZoneCoverage(ctx, tx, identity.Key)
	if err != nil {
		return zone, nil, nil, err
	}
	if !zone.revision.Valid || zone.foreignPricing {
		return zone, nil, nil, nil
	}
	lost, err := usageSessionsWithoutRollup(ctx, tx, zone.id, -1)
	if err != nil {
		return zone, nil, nil, err
	}
	window, err := usageRollupWindowSessions(ctx, tx, zone.id, filter)
	if err != nil {
		return zone, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return zone, nil, nil, err
	}
	return zone, lost, window, nil
}

func (db *DB) captureJournaledUsageWindow(ctx context.Context, filter UsageFilter, kind usageQueryKind, cursor bool, databaseID string, zone usageZoneCoverage, lost, window []string) (usageQuerySnapshot, usageQuerySnapshot, []string, bool, error) {
	var refresh usageQuerySnapshot
	refreshIDs := make([]string, 0)
	trusted := false
	snapshot, err := db.captureUsageSnapshot(ctx, filter, func(ctx context.Context, tx *sql.Tx, snapshot *usageQuerySnapshot) error {
		if snapshot.DatabaseID != databaseID {
			return errUsageCacheSourceChanged
		}
		trusted = zone.revision.Int64 <= snapshot.ChangeRevision && snapshot.PricingIdentity == zone.pricing
		if !trusted {
			return captureUsageQueryFrom(ctx, tx, filter, kind, snapshot)
		}
		changes, err := loadSessionJournalChanges(ctx, tx, "usage_session_changes", "usage", zone.revision.Int64, snapshot.ChangeRevision)
		if err != nil {
			return err
		}
		refreshIDs = append(refreshIDs, changes...)
		refreshIDs = append(refreshIDs, lost...)
		slices.Sort(refreshIDs)
		refreshIDs = slices.Compact(refreshIDs)
		refresh = *snapshot
		if err := loadUsageSnapshotSessions(ctx, tx, refreshIDs, filter, false, &refresh); err != nil {
			return err
		}
		fingerprints, err := usageEventFingerprintsWithQuerier(ctx, tx, refreshIDs, nil, false)
		if err != nil {
			return err
		}
		for i := range refresh.Versions {
			refresh.Versions[i].UsageEventFingerprint = fingerprints[refresh.Versions[i].SessionID]
		}
		if err := loadUsageSnapshotSessions(ctx, tx, window, filter, true, snapshot); err != nil {
			return err
		}
		// Replace window metadata for refresh targets with its complete record.
		records := make(map[string]int)
		if len(refresh.Sessions) > 0 {
			for i, session := range snapshot.Sessions {
				records[session.ID] = i
			}
		}
		for i, session := range refresh.Sessions {
			if index, ok := records[session.ID]; ok {
				snapshot.Sessions[index], snapshot.Versions[index] = session, refresh.Versions[i]
			} else {
				snapshot.Sessions = append(snapshot.Sessions, session)
				snapshot.Versions = append(snapshot.Versions, refresh.Versions[i])
			}
		}
		snapshot.Intervals = usageQueryIntervals(filter)
		if cursor {
			return tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM cursor_usage_events`).Scan(&snapshot.CursorHighWater)
		}
		return nil
	})
	return snapshot, refresh, refreshIDs, trusted, err
}

func (cache *usageCache) refreshJournaledUsageWindow(ctx context.Context, snapshot *usageQuerySnapshot, refresh usageQuerySnapshot, refreshIDs []string, report func(string)) (map[string]usageRollupInstall, usageRollupRequestTimings, error) {
	// Deleted IDs must still reach Ensure, which removes their cached facts.
	versions := make(map[string]usageSourceVersion, len(refresh.Versions))
	for _, version := range refresh.Versions {
		versions[version.SessionID] = version
	}
	refresh.Versions = refresh.Versions[:0]
	for _, id := range refreshIDs {
		version, ok := versions[id]
		if !ok {
			version.SessionID = id
		}
		refresh.Versions = append(refresh.Versions, version)
	}
	refresh.CursorHighWater = snapshot.CursorHighWater
	var installs map[string]usageRollupInstall
	var timings usageRollupRequestTimings
	if len(refreshIDs) > 0 || refresh.CursorHighWater > 0 {
		report("Checking the usage cache for this report")
		started := time.Now()
		fills, err := cache.fill.Ensure(ctx, refresh.Versions, refresh.CursorHighWater)
		if err != nil {
			return nil, timings, err
		}
		timings.fill = time.Since(started)
		snapshot.dropDeleted(maps.Clone(fills))
		refresh.dropDeleted(fills)
		built, metrics, err := cache.rollup.Ensure(ctx, refresh, fills, export.NewPricingResolverWithDigest(snapshot.PricingRows, snapshot.PricingDigest))
		if err != nil {
			return nil, timings, err
		}
		timings.rollup = metrics
		installs = built
	}
	return installs, timings, nil
}

func usageSessionsWithoutRollup(ctx context.Context, q sessionExportQuerier, timezoneID int64, limit int) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT session_id FROM usage_rollup_invalidations
		WHERE timezone_id = ? LIMIT ?`, timezoneID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func usageRollupWindowSessions(ctx context.Context, q sessionExportQuerier, timezoneID int64, filter UsageFilter) ([]string, error) {
	from, to := usageRollupDateBounds(usageQuerySnapshot{Intervals: usageQueryIntervals(filter)})
	query := `SELECT session_id FROM usage_rollup_installs WHERE timezone_id=?`
	args := []any{timezoneID}
	if from != "" || to != "" {
		var branches []string
		for _, table := range []string{"usage_daily_rollups", "usage_rollup_exceptions"} {
			branch := `SELECT rollup_install_id FROM ` + table + ` WHERE 1=1`
			if from != "" {
				branch += ` AND local_date>=?`
				args = append(args, from)
			}
			if to != "" {
				branch += ` AND local_date<=?`
				args = append(args, to)
			}
			branches = append(branches, branch)
		}
		// Keep date seeks outermost. The scalar lookup seeks the install's
		// primary key even when ANALYZE makes a tiny install table cheap to scan.
		query = `SELECT DISTINCT (
			SELECT i.session_id FROM usage_rollup_installs i
			WHERE i.id = r.rollup_install_id AND i.timezone_id = ?
		) FROM (` + strings.Join(branches, ` UNION ALL `) + `) r`
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		// A matching date in another timezone has no install in this zone.
		if id.Valid && id.String != usageRollupCursorSessionID {
			ids = append(ids, id.String)
		}
	}
	return ids, rows.Err()
}

func verifyPreparedUsageRollup(ctx context.Context, conn *sql.Conn, key string, prepared usagePreparedWindow) (int64, error) {
	if prepared.journaled {
		return verifyJournaledUsageRollup(ctx, conn, prepared, key)
	}
	return verifyUsageRollupQuery(ctx, conn, key, prepared.snapshot, prepared.installs)
}

func verifyJournaledUsageRollup(ctx context.Context, conn *sql.Conn, prepared usagePreparedWindow, key string) (int64, error) {
	zone, err := readUsageZoneCoverage(ctx, conn, key)
	if err != nil {
		return 0, err
	}
	snapshot := prepared.snapshot
	catalog := snapshot.PricingIdentity
	if !zone.revision.Valid || zone.revision.Int64 < snapshot.ChangeRevision || zone.pricing != catalog {
		return 0, errUsageCacheSourceChanged
	}
	// Only installs written since preparation can mix newer facts with this
	// snapshot's metadata. The indexed range stays bounded by concurrent work.
	// Notifications can pair a conservative source revision with newer session
	// metadata. Check the baked fields even when the revision stamp matches.
	rows, err := conn.QueryContext(ctx, `
		SELECT session_id, source_change_revision, source_pricing,
			baked_agent, baked_started_at
		FROM usage_rollup_installs
		WHERE timezone_id = ? AND install_revision >= ?`, zone.id, prepared.installSeq)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var sessions map[string]*usageQuerySession
	for rows.Next() {
		var id, pricing, agent, startedAt string
		var revision int64
		if err := rows.Scan(&id, &revision, &pricing, &agent, &startedAt); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if sessions == nil {
			sessions = usageRollupSessionMap(snapshot)
		}
		if session, ok := sessions[id]; ok && (revision != snapshot.ChangeRevision || pricing != catalog || agent != session.Agent || startedAt != session.StartedAt) {
			_ = rows.Close()
			return 0, errUsageCacheSourceChanged
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return 0, err
	}
	lost, err := usageSessionsWithoutRollup(ctx, conn, zone.id, 1)
	if err != nil {
		return 0, err
	}
	if len(lost) > 0 {
		return 0, &usageCacheMissingSessionError{SessionID: lost[0]}
	}
	if snapshot.CursorHighWater > 0 {
		var install usageRollupInstall
		err := conn.QueryRowContext(ctx, `
			SELECT id, install_revision, pricing_hash FROM usage_rollup_installs
			WHERE timezone_id = ? AND session_id = ?`, zone.id, usageRollupCursorSessionID,
		).Scan(&install.ID, &install.InstallRevision, &install.PricingHash)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errUsageCacheSourceChanged
		}
		if err != nil {
			return 0, err
		}
		if err := verifyUsageRollupInstall(usageRollupCursorSessionID, prepared.installs, map[int64]usageRollupInstall{install.ID: install}); err != nil {
			return 0, err
		}
	}
	return zone.id, nil
}
