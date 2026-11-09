package db

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// UsageSourceVersion identifies the committed facts of a session.
type UsageSourceVersion = usageSourceVersion

// UsageSourceSession is the metadata that determines attribution and filters.
type UsageSourceSession = usageQuerySession

// UsageSourceSnapshot contains values copied from one source transaction.
type UsageSourceSnapshot = usageQuerySnapshot

// UsageSourceRequest selects a window, explicit sessions, or journal changes.
// A non-nil IDs selects exactly those sessions, including an empty set.
type UsageSourceRequest struct {
	Filter        UsageFilter
	IDs           *[]string
	AfterRevision *int64
}

// UsageSourceAccount is one imported account billing record.
type UsageSourceAccount struct {
	ID                                                                                int64
	Agent, Timestamp, Model, DedupKey                                                 string
	InputTokens, OutputTokens, CacheWriteTokens, CacheReadTokens, ChargedMicrodollars int64
	Headless                                                                          bool
}

// UsageCacheSource supplies source snapshots while the shared cache owns facts,
// pricing, deduplication, publication, and generation lifetimes.
type UsageCacheSource interface {
	CaptureUsage(context.Context, UsageSourceRequest) (UsageSourceSnapshot, error)
	StreamUsage(context.Context, []string, func(string, usagefacts.Fact) error) (UsageSourceSnapshot, error)
	ReadUsageAccounts(context.Context) (string, int64, []UsageSourceAccount, error)
	BuildProjectIdentityMap(context.Context, []string) (map[string]export.ProjectMapEntry, error)
	UsageSessionNames(context.Context, []string) (map[string][2]string, error)
}

// UsageCacheReader exposes aggregate reads backed by the shared cache engine.
type UsageCacheReader struct{ engine *DB }

// NewUsageCacheReader keeps each remote target in its own retirement directory.
// The source identity further separates incarnations inside that directory.
func NewUsageCacheReader(directory string, source UsageCacheSource) (*UsageCacheReader, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		log.Printf("creating persistent usage cache directory: %v; using temporary storage", err)
	}
	engine := &DB{usageSource: source}
	engine.usageCache = newUsageCacheManager(filepath.Join(directory, "source"))
	engine.usageCache.attachArchive(engine)
	return &UsageCacheReader{engine: engine}, nil
}

func (reader *UsageCacheReader) Close() error { return reader.engine.usageCache.Close() }

func (reader *UsageCacheReader) GetDailyUsage(ctx context.Context, filter UsageFilter) (DailyUsageResult, error) {
	return reader.engine.GetDailyUsage(ctx, filter)
}

func (reader *UsageCacheReader) GetTopSessionsByCost(ctx context.Context, filter UsageFilter, limit int) ([]TopSessionEntry, error) {
	return reader.engine.GetTopSessionsByCost(ctx, filter, limit)
}

func (reader *UsageCacheReader) GetUsageSessionCounts(ctx context.Context, filter UsageFilter) (UsageSessionCounts, error) {
	return reader.engine.GetUsageSessionCounts(ctx, filter)
}

func (reader *UsageCacheReader) GetUsageMatchingSessionCount(ctx context.Context, filter UsageFilter) (int, error) {
	return reader.engine.GetUsageMatchingSessionCount(ctx, filter)
}

func (db *DB) captureExternalUsage(ctx context.Context, filter UsageFilter, ids *[]string, after *int64) (usageQuerySnapshot, error) {
	snapshot, err := db.usageSource.CaptureUsage(ctx, UsageSourceRequest{Filter: filter, IDs: ids, AfterRevision: after})
	if err != nil {
		return snapshot, err
	}
	snapshot.exactAccountRevision = true
	for i := range snapshot.Versions {
		snapshot.Versions[i].CaptureRevision = snapshot.ChangeRevision
	}
	snapshot.location = filter.Location()
	snapshot.Intervals = usageQueryIntervals(filter)
	snapshot.PricingDigest, err = export.EffectivePricingDigest(snapshot.PricingRows)
	snapshot.PricingIdentity = usagePricingIdentityWithDigest(snapshot.PricingDigest)
	for i := range snapshot.Versions {
		snapshot.Versions[i].CapturePricingDigest = snapshot.PricingDigest
	}
	return snapshot, err
}

func (db *DB) usageProjectIdentityMap(ctx context.Context, labels []string) (map[string]export.ProjectMapEntry, error) {
	if db.usageSource != nil {
		return db.usageSource.BuildProjectIdentityMap(ctx, labels)
	}
	return db.BuildProjectIdentityMap(ctx, labels)
}

func (db *DB) usageTopSessionMetadata(ctx context.Context, ids []string) (map[string]topSessionMetadata, error) {
	if db.usageSource == nil {
		return db.loadTopSessionMetadata(ctx, ids)
	}
	names, err := db.usageSource.UsageSessionNames(ctx, ids)
	result := make(map[string]topSessionMetadata, len(names))
	for id, name := range names {
		result[id] = topSessionMetadata{displayName: name[0], sessionName: name[1]}
	}
	return result, err
}

func usageSnapshotRankingOutput(fact usagefacts.Fact) int64 {
	if fact.RankingOutputTokens != nil {
		return *fact.RankingOutputTokens
	}
	return fact.OutputTokens
}

func (c *usageFillCoordinator) extractExternalSessions(ctx context.Context, versions []usageSourceVersion, expected map[string]usageFillInstallExpectation) (_ *usageFactSpool, _ map[string]usageSourceVersion, err error) {
	spool, err := newUsageFactSpool(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, spool.Close())
		}
	}()
	tx, err := spool.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	writer := usageFactSpoolWriter{ctx: ctx, tx: tx}
	counts := make(map[string][3]int, len(versions))
	snapshot, err := c.archive.usageSource.StreamUsage(ctx, usageSourceVersionIDs(versions), func(id string, fact usagefacts.Fact) error {
		count := counts[id]
		kind, base := 0, 0
		if fact.Source != "message" {
			kind, base = 2, usageEventFactBase
		} else if !fact.TokenEligible {
			kind, base = 1, usageActivityFactBase
		}
		if err := writer.Add(id, base+count[kind], fact); err != nil {
			return err
		}
		count[kind]++
		counts[id] = count
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	capturedPricing, err := export.EffectivePricingDigest(snapshot.PricingRows)
	if err != nil {
		return nil, nil, err
	}
	for _, version := range versions {
		if version.CapturePricingDigest != "" && version.CapturePricingDigest != capturedPricing {
			return nil, nil, errUsageCacheSourceChanged
		}
		if version.CaptureRevision != 0 && version.CaptureRevision != snapshot.ChangeRevision {
			return nil, nil, errUsageCacheSourceChanged
		}
	}
	if snapshot.DatabaseID != c.cache.databaseID {
		return nil, nil, errUsageCacheSourceChanged
	}
	extracted := make(map[string]usageSourceVersion, len(snapshot.Versions))
	for _, version := range snapshot.Versions {
		extracted[version.SessionID] = version
		count := counts[version.SessionID]
		var revision any
		if expectation := expected[version.SessionID]; expectation.Exists {
			revision = expectation.InstallRevision
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id, install_revision, message_count, activity_count, event_count) VALUES (?, ?, ?, ?, ?)`, version.SessionID, revision, count[0], count[1], count[2]); err != nil {
			return nil, nil, err
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	spool.changeRevision = snapshot.ChangeRevision
	return spool, extracted, nil
}

// Account sources can update or delete existing IDs. Replace their facts and
// invalidate both removed and arriving identities in the same transaction.
func (c *usageFillCoordinator) replaceExternalAccounts(ctx context.Context, target int64) error {
	databaseID, revision, accounts, err := c.archive.usageSource.ReadUsageAccounts(ctx)
	if err != nil {
		return err
	}
	if databaseID != c.cache.databaseID || revision != target {
		return errUsageCacheSourceChanged
	}
	tx, err := c.cache.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer tx.Close()
	if _, err := tx.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = tx.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`) }()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM usage_cache_metadata WHERE key=?`, usageCacheMetadataCursorHighWaterMark).Scan(&current); err != nil {
		return err
	}
	currentRevision, err := strconv.ParseInt(current, 10, 64)
	if err != nil {
		return err
	}
	if currentRevision > target {
		return errUsageCacheSourceChanged
	}
	if currentRevision == target {
		return nil
	}
	identities := newUsageDedupIdentitySet()
	rows, err := tx.QueryContext(ctx, `SELECT dedup_key FROM cursor_usage_facts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		identities.add("", "", "", key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollup_installs WHERE session_id=?`, usageRollupCursorSessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cursor_usage_facts`); err != nil {
		return err
	}
	for _, account := range accounts {
		identities.add("", "", "", account.DedupKey)
		millis, _, _ := usagefacts.ParseTimestamp(account.Timestamp)
		if _, err := tx.ExecContext(ctx, `INSERT INTO cursor_usage_facts VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, account.ID, account.Agent, millis, account.Timestamp, account.Model, max(account.InputTokens, 0), max(account.OutputTokens, 0), max(account.CacheWriteTokens, 0), max(account.CacheReadTokens, 0), account.ChargedMicrodollars, boolInt(account.Headless), account.DedupKey); err != nil {
			return err
		}
	}
	if _, err := invalidateUsageDedupSharers(ctx, tx, identities, nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_cache_metadata SET value=? WHERE key=?`, strconv.FormatInt(target, 10), usageCacheMetadataCursorHighWaterMark); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `COMMIT`)
	return err
}

func (db *DB) prepareExternalUsageWindow(ctx context.Context, filter UsageFilter, kind usageQueryKind, cursor, journal bool, report func(string)) (usagePreparedWindow, error) {
	empty := []string{}
	probe, err := db.captureExternalUsage(ctx, filter, &empty, nil)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	cache, release, err := db.usageCache.acquireGeneration(ctx, probe.DatabaseID)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	defer release()
	identity := usageTimezoneIdentityFor(filter.Location(), nil)
	zone, lost, window, err := cache.captureUsageZoneWindow(ctx, identity, filter, kind)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	if journal && kind != usageQueryKindActivity && zone.revision.Valid && !zone.foreignPricing && zone.pricing == probe.PricingIdentity && zone.revision.Int64 <= probe.ChangeRevision {
		requested := slices.Concat(window, lost)
		snapshot, err := db.captureExternalUsage(ctx, filter, &requested, &zone.revision.Int64)
		if err != nil {
			return usagePreparedWindow{}, err
		}
		if snapshot.DatabaseID != probe.DatabaseID || snapshot.PricingIdentity != zone.pricing {
			return usagePreparedWindow{}, errUsageCacheSourceChanged
		}
		if !cursor {
			snapshot.CursorHighWater = 0
		}
		refreshIDs := slices.Concat(snapshot.ChangeIDs, lost)
		slices.Sort(refreshIDs)
		refreshIDs = slices.Compact(refreshIDs)
		refresh := snapshot
		refresh.Sessions, refresh.Versions = nil, nil
		for i, session := range snapshot.Sessions {
			if _, ok := slices.BinarySearch(refreshIDs, session.ID); ok {
				refresh.Sessions = append(refresh.Sessions, session)
				refresh.Versions = append(refresh.Versions, snapshot.Versions[i])
			}
		}
		installs, timings, err := cache.refreshJournaledUsageWindow(ctx, &snapshot, refresh, refreshIDs, report)
		if err != nil {
			return usagePreparedWindow{}, err
		}
		if err := cache.claimUsageZone(ctx, identity, zone, snapshot.ChangeRevision, snapshot.PricingIdentity, installs); err != nil {
			return usagePreparedWindow{}, err
		}
		return usagePreparedWindow{snapshot: snapshot, installs: installs, journaled: true, kind: kind, installSeq: zone.installSeq, timings: timings}, nil
	}
	snapshot, err := db.captureExternalUsage(ctx, filter, nil, nil)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	if snapshot.DatabaseID != probe.DatabaseID {
		return usagePreparedWindow{}, errUsageCacheSourceChanged
	}
	if !cursor {
		snapshot.CursorHighWater = 0
	}
	// A complete source capture permits removal of stale deleted owners before
	// its first global coverage claim. Bounded windows retain their own proof.
	if filter.From == "" && filter.To == "" {
		rows, err := cache.db.QueryContext(ctx, `SELECT session_id FROM usage_cached_sessions`)
		if err != nil {
			return usagePreparedWindow{}, err
		}
		active := make(map[string]bool, len(snapshot.Sessions))
		for _, session := range snapshot.Sessions {
			active[session.ID] = true
		}
		var deleted []string
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return usagePreparedWindow{}, err
			}
			if !active[id] {
				deleted = append(deleted, id)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return usagePreparedWindow{}, err
		}
		if err := cache.fill.deleteNotificationSessions(deleted, snapshot.ChangeRevision); err != nil {
			return usagePreparedWindow{}, err
		}
	}
	installs, timings, err := cache.fillAndRollupUsageWindow(ctx, &snapshot, &snapshot, report)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	prepared := usagePreparedWindow{snapshot: snapshot, installs: installs, timings: timings}
	if filter.From == "" && filter.To == "" && kind != usageQueryKindActivity {
		prepared.claimZone = &zone
	}
	return prepared, nil
}
