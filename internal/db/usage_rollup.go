package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const usageRollupCursorSessionID = "\x00cursor"

type usageTimezoneIdentity struct {
	Key, Name, IntervalFingerprint string
}

type usageRollupInstall struct {
	ID, FactRevision, InstallRevision int64
	SessionID, CachedAt, PricingHash  string
}

type usageRollupMetrics struct {
	DailyRows, ExceptionGroups, ExceptionRows int64
	BuildDuration, InstallDuration            time.Duration
}

func usageTimezoneIdentityFor(
	location *time.Location, _ []usageQueryInterval,
) usageTimezoneIdentity {
	if location == nil {
		location = time.Local
	}
	name := usageLocationName(location)
	digest := sha256.New()
	writeUsageHashString(digest, name)
	// Anonymous process-local locations need a stable identity independent of
	// the requested window. Weekly probes cover historical and near-future rule
	// transitions without relying on a platform-specific zoneinfo path.
	for day := time.Date(1970, 1, 1, 12, 0, 0, 0, time.UTC); day.Year() <= 2100; day = day.AddDate(0, 0, 7) {
		zone, offset := day.In(location).Zone()
		writeUsageHashString(digest, zone)
		writeUsageHashInt64(digest, int64(offset))
	}
	fingerprint := hex.EncodeToString(digest.Sum(nil))
	key := name
	if key == "" || key == "Local" {
		key = "local:" + fingerprint
	}
	return usageTimezoneIdentity{Key: key, Name: name, IntervalFingerprint: fingerprint}
}

func usageLocationName(location *time.Location) string {
	name := location.String()
	if location != time.Local || (name != "" && name != "Local") {
		return name
	}
	resolved, err := filepath.EvalSymlinks("/etc/localtime")
	if err != nil {
		return name
	}
	normalized := filepath.ToSlash(resolved)
	const marker = "/zoneinfo/"
	index := strings.LastIndex(normalized, marker)
	if index < 0 {
		return name
	}
	candidate := normalized[index+len(marker):]
	if _, err := time.LoadLocation(candidate); err != nil {
		return name
	}
	return candidate
}

func usageRateHash(
	model, pricedModel, pattern string, ok bool, rates export.ModelRates,
) string {
	digest := sha256.New()
	writeUsageHashString(digest, model)
	writeUsageHashString(digest, pricedModel)
	writeUsageHashString(digest, pattern)
	writeUsageHashBool(digest, ok)
	writeUsageRatesHash(digest, rates)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeUsageRatesHash(digest hash.Hash, rates export.ModelRates) {
	writeUsageHashString(digest, string(rates.Source))
	if rates.UpdatedAt != nil {
		writeUsageHashString(digest, rates.UpdatedAt.UTC().Format(time.RFC3339Nano))
	} else {
		writeUsageHashString(digest, "")
	}
	for _, value := range []int64{
		rates.InputPerMTok.Microdollars, rates.OutputPerMTok.Microdollars,
		rates.CacheWritePerMTok.Microdollars, rates.CacheReadPerMTok.Microdollars,
		int64(len(rates.Bands)),
	} {
		writeUsageHashInt64(digest, value)
	}
	for _, band := range rates.Bands {
		if band.UpdatedAt != nil {
			writeUsageHashString(digest, band.UpdatedAt.UTC().Format(time.RFC3339Nano))
		} else {
			writeUsageHashString(digest, "")
		}
		for _, value := range []int64{
			int64(band.AboveInputTokens), band.InputPerMTok.Microdollars,
			band.OutputPerMTok.Microdollars, band.CacheWritePerMTok.Microdollars,
			band.CacheReadPerMTok.Microdollars,
		} {
			writeUsageHashInt64(digest, value)
		}
	}
}

func usageBandRates(rates export.ModelRates) []export.ModelRates {
	result := make([]export.ModelRates, 0, len(rates.Bands))
	for _, band := range rates.Bands {
		result = append(result, export.ModelRates{
			InputPerMTok: band.InputPerMTok, OutputPerMTok: band.OutputPerMTok,
			CacheWritePerMTok: band.CacheWritePerMTok,
			CacheReadPerMTok:  band.CacheReadPerMTok,
		})
	}
	return result
}

func validateNonnegativeUsageRates(rates export.ModelRates) error {
	for _, item := range append([]export.ModelRates{rates}, usageBandRates(rates)...) {
		if item.InputPerMTok.Microdollars < 0 || item.OutputPerMTok.Microdollars < 0 ||
			item.CacheWritePerMTok.Microdollars < 0 || item.CacheReadPerMTok.Microdollars < 0 {
			return money.ErrNegative
		}
	}
	return nil
}

type usageRollupCall struct {
	done     chan struct{}
	installs map[string]usageRollupInstall
	metrics  usageRollupMetrics
	err      error
}

type usageRollupCoordinator struct {
	archive *DB
	cache   *usageCache
	ctx     context.Context
	mu      sync.Mutex
	calls   map[string]*usageRollupCall
}

func newUsageRollupCoordinator(
	ctx context.Context, archive *DB, cache *usageCache,
) *usageRollupCoordinator {
	return &usageRollupCoordinator{
		archive: archive, cache: cache, ctx: ctx,
		calls: make(map[string]*usageRollupCall),
	}
}

func (c *usageRollupCoordinator) Ensure(
	ctx context.Context, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pricingHash, err := export.EffectivePricingDigest(snapshot.PricingRows)
	if err != nil {
		return nil, usageRollupMetrics{}, fmt.Errorf("hashing usage pricing: %w", err)
	}
	key := usageRollupCallKey(snapshot, fills, pricingHash)
	c.mu.Lock()
	call := c.calls[key]
	if call == nil {
		call = &usageRollupCall{done: make(chan struct{})}
		c.calls[key] = call
		go func() {
			call.installs, call.metrics, call.err = c.ensureNow(
				c.ctx, snapshot, fills, resolver, pricingHash)
			close(call.done)
			c.mu.Lock()
			delete(c.calls, key)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, usageRollupMetrics{}, ctx.Err()
	case <-call.done:
		return call.installs, call.metrics, call.err
	}
}

func (c *usageRollupCoordinator) ensureNow(
	ctx context.Context, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver,
	pricingHash string,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	identity := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals)
	conn, err := c.cache.db.Conn(ctx)
	if err != nil {
		return nil, usageRollupMetrics{}, err
	}
	defer conn.Close()
	installs, stale, err := readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, pricingHash)
	if err != nil || len(stale) == 0 {
		cursorCurrent := installs[usageRollupCursorSessionID].FactRevision >=
			snapshot.CursorHighWater
		if err != nil || cursorCurrent {
			return installs, usageRollupMetrics{}, err
		}
	}
	staleSessions := make(map[string]usageQuerySession, len(stale))
	staleVersions := make(map[string]usageSourceVersion, len(stale))
	staleFills := make(map[string]usageFillResult, len(stale))
	for _, session := range snapshot.Sessions {
		if stale[session.ID] {
			staleSessions[session.ID] = session
		}
	}
	for _, version := range snapshot.Versions {
		if stale[version.SessionID] {
			staleVersions[version.SessionID] = version
			staleFills[version.SessionID] = fills[version.SessionID]
		}
	}
	started := time.Now()
	facts, err := loadUsageRollupFacts(ctx, conn, staleSessions)
	if err != nil {
		return nil, usageRollupMetrics{}, err
	}
	builds, err := buildUsageRollupSessions(
		facts, staleSessions, staleVersions, staleFills, snapshot.location,
		resolver, pricingHash)
	if err != nil {
		return nil, usageRollupMetrics{}, err
	}
	if installs[usageRollupCursorSessionID].FactRevision < snapshot.CursorHighWater {
		cursorBuild, cursorErr := loadCursorUsageRollupBuild(
			ctx, conn, snapshot.CursorHighWater, snapshot.location, pricingHash)
		if cursorErr != nil {
			return nil, usageRollupMetrics{}, cursorErr
		}
		builds = append(builds, cursorBuild)
	}
	metrics := usageRollupMetrics{BuildDuration: time.Since(started)}
	for _, build := range builds {
		metrics.DailyRows += int64(len(build.Daily))
		metrics.ExceptionRows += int64(len(build.Exceptions))
		groups := make(map[string]bool)
		for _, row := range build.Exceptions {
			groups[row.GroupKind+"\x00"+row.GroupKey] = true
		}
		metrics.ExceptionGroups += int64(len(groups))
	}
	if err := c.recheck(ctx, staleVersions, staleSessions); err != nil {
		return nil, metrics, err
	}
	installStarted := time.Now()
	if err := installUsageRollupBuilds(ctx, conn, identity, snapshot.location, builds); err != nil {
		return nil, metrics, err
	}
	metrics.InstallDuration = time.Since(installStarted)
	installs, stale, err = readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, pricingHash)
	if err != nil {
		return nil, metrics, err
	}
	if len(stale) != 0 {
		return nil, metrics, fmt.Errorf("usage rollup install did not verify")
	}
	if installs[usageRollupCursorSessionID].FactRevision < snapshot.CursorHighWater {
		return nil, metrics, fmt.Errorf("Cursor usage rollup install did not verify")
	}
	return installs, metrics, nil
}

func readUsageRollupInstalls(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	snapshot usageQuerySnapshot, fills map[string]usageFillResult,
	pricingHash string,
) (map[string]usageRollupInstall, map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT i.id, i.session_id,
		i.fact_install_revision, i.install_revision, i.cached_at,
		i.source_sync_marker, i.source_transcript_rev, i.usage_event_fingerprint,
		i.baked_agent, i.baked_started_at, i.pricing_hash
		FROM usage_rollup_timezones tz JOIN usage_rollup_installs i
		  ON i.timezone_id = tz.id WHERE tz.timezone_key = ?`, identity.Key)
	if err != nil {
		return nil, nil, err
	}
	installs := make(map[string]usageRollupInstall)
	sources := make(map[string]usageSourceVersion)
	baked := make(map[string][2]string)
	pricing := make(map[string]string)
	for rows.Next() {
		var item usageRollupInstall
		var source usageSourceVersion
		var agent, startedAt, installedPricing string
		if err := rows.Scan(&item.ID, &item.SessionID, &item.FactRevision,
			&item.InstallRevision, &item.CachedAt, &source.SyncMarker,
			&source.TranscriptRevision, &source.UsageEventFingerprint,
			&agent, &startedAt, &installedPricing); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		source.SessionID = item.SessionID
		item.PricingHash = installedPricing
		installs[item.SessionID], sources[item.SessionID] = item, source
		baked[item.SessionID] = [2]string{agent, startedAt}
		pricing[item.SessionID] = installedPricing
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	sessions := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		sessions[session.ID] = session
	}
	stale := make(map[string]bool)
	for _, version := range snapshot.Versions {
		fill := fills[version.SessionID]
		item, ok := installs[version.SessionID]
		session := sessions[version.SessionID]
		if fill.Deleted {
			continue
		}
		if !ok || item.FactRevision != fill.InstallRevision ||
			!sources[version.SessionID].Equal(version) ||
			baked[version.SessionID] != [2]string{session.Agent, session.StartedAt} ||
			pricing[version.SessionID] != pricingHash {
			stale[version.SessionID] = true
		}
	}
	return installs, stale, nil
}

func (c *usageRollupCoordinator) recheck(
	ctx context.Context, versions map[string]usageSourceVersion,
	sessions map[string]usageQuerySession,
) error {
	observed := make([]usageSourceVersion, 0, len(versions))
	for _, version := range versions {
		observed = append(observed, version)
	}
	slices.SortFunc(observed, func(a, b usageSourceVersion) int {
		return compareStrings(a.SessionID, b.SessionID)
	})
	current, err := c.cache.fill.recheckSourceVersions(ctx, observed)
	if err != nil {
		return err
	}
	for _, version := range observed {
		if value, ok := current[version.SessionID]; !ok || !value.Equal(version) {
			return fmt.Errorf("%w during rollup build", errUsageCacheSourceChanged)
		}
	}
	metadata, err := c.currentMetadata(ctx, observed)
	if err != nil {
		return err
	}
	for id, session := range sessions {
		if metadata[id] != [2]string{session.Agent, session.StartedAt} {
			return fmt.Errorf("%w during rollup metadata check", errUsageCacheSourceChanged)
		}
	}
	return nil
}

func (c *usageRollupCoordinator) currentMetadata(
	ctx context.Context, versions []usageSourceVersion,
) (map[string][2]string, error) {
	ids := make([]string, 0, len(versions))
	for _, version := range versions {
		ids = append(ids, version.SessionID)
	}
	result := make(map[string][2]string, len(ids))
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := c.archive.getReader().QueryContext(ctx, `SELECT id, agent,
			COALESCE(started_at, '') FROM sessions
			WHERE deleted_at IS NULL AND id IN `+placeholders, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, agent, startedAt string
			if err := rows.Scan(&id, &agent, &startedAt); err != nil {
				return err
			}
			result[id] = [2]string{agent, startedAt}
		}
		return rows.Err()
	})
	return result, err
}

func installUsageRollupBuilds(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	location *time.Location, builds []usageRollupBuild,
) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_rollup_timezones(
		timezone_key, timezone_name, interval_fingerprint, last_requested_at
	) VALUES (?, ?, ?, ?) ON CONFLICT(timezone_key) DO UPDATE SET
		timezone_name=excluded.timezone_name,
		interval_fingerprint=excluded.interval_fingerprint,
		last_requested_at=excluded.last_requested_at`,
		identity.Key, identity.Name, identity.IntervalFingerprint, now); err != nil {
		return err
	}
	var timezoneID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM usage_rollup_timezones
		WHERE timezone_key = ?`, identity.Key).Scan(&timezoneID); err != nil {
		return err
	}
	var revisionText string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM usage_cache_metadata
		WHERE key = ?`, usageCacheMetadataNextRollupRevision).Scan(&revisionText); err != nil {
		return err
	}
	nextRevision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil {
		return err
	}
	dates := make(map[string]bool)
	for _, build := range builds {
		var installID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM usage_rollup_installs
			WHERE timezone_id = ? AND session_id = ?`, timezoneID, build.SessionID).Scan(&installID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			for _, table := range []string{
				"usage_daily_rollups", "usage_activity_rollups", "usage_rollup_exceptions",
			} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+
					` WHERE rollup_install_id = ?`, installID); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `UPDATE usage_rollup_installs SET
				source_sync_marker=?, source_transcript_rev=?, usage_event_fingerprint=?,
				fact_install_revision=?, baked_agent=?, baked_started_at=?,
				pricing_hash=?, install_revision=?, cached_at=? WHERE id=?`,
				build.Source.SyncMarker, build.Source.TranscriptRevision,
				build.Source.UsageEventFingerprint, build.FactRevision,
				build.Agent, build.StartedAt, build.PricingHash,
				nextRevision, now, installID)
		} else {
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO usage_rollup_installs(
				timezone_id, session_id, source_sync_marker, source_transcript_rev,
				usage_event_fingerprint, fact_install_revision, baked_agent,
				baked_started_at, pricing_hash, install_revision, cached_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, timezoneID, build.SessionID,
				build.Source.SyncMarker, build.Source.TranscriptRevision,
				build.Source.UsageEventFingerprint, build.FactRevision,
				build.Agent, build.StartedAt, build.PricingHash, nextRevision, now)
			if insertErr != nil {
				return insertErr
			}
			installID, err = result.LastInsertId()
		}
		if err != nil {
			return err
		}
		if err := installUsageRollupRows(ctx, tx, installID, build, dates); err != nil {
			return err
		}
		nextRevision++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_cache_metadata SET value=?
		WHERE key=?`, strconv.FormatInt(nextRevision, 10),
		usageCacheMetadataNextRollupRevision); err != nil {
		return err
	}
	if err := installUsageRollupDays(ctx, tx, timezoneID, location, dates); err != nil {
		return err
	}
	return tx.Commit()
}

func installUsageRollupRows(
	ctx context.Context, tx *sql.Tx, installID int64,
	build usageRollupBuild, dates map[string]bool,
) error {
	for _, row := range build.Daily {
		band := -1
		if row.BandThreshold != nil {
			band = *row.BandThreshold
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_daily_rollups VALUES(
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			installID, row.LocalDate, row.ReportedModel, row.PricedModel,
			row.MatchedPattern, boolInt(row.RateOK), row.RateHash, band,
			row.InputTokens, row.OutputTokens, row.ReasoningTokens,
			row.CacheCreationTokens, row.CacheReadTokens, row.WebSearchRequests,
			row.CostMicrodollars, row.SavingsMicrodollars,
			row.AuthoritativeCostMicrodollars, row.ComputedRequestCount,
			row.ComputedAggregateCount, row.ReportedCount, row.BaseRequestCount,
			row.DiscardedSnapshotOutputTokens)
		if err != nil {
			return err
		}
		dates[row.LocalDate] = true
	}
	for _, row := range build.Activity {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_activity_rollups VALUES(?, ?, ?, ?)`,
			installID, row.LocalDate, row.Model, row.UserMessageCount); err != nil {
			return err
		}
		dates[row.LocalDate] = true
	}
	for _, row := range build.Exceptions {
		fact := row.Fact
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_rollup_exceptions VALUES(
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			installID, row.GroupKind, row.GroupKey, fact.CachedSessionID,
			fact.FactIndex, fact.SourceSessionID, fact.LocalDate, fact.Fact.Source,
			fact.Fact.MessageOrdinal, fact.Fact.TimestampMillis, fact.Fact.TimestampNanos,
			fact.Fact.RawTimestamp, boolInt(fact.Fact.UsesSessionStart), fact.Model,
			fact.Fact.InputTokens, fact.Fact.OutputTokens, fact.Fact.ReasoningTokens,
			fact.Fact.CacheCreationTokens, fact.Fact.CacheReadTokens,
			fact.Fact.WebSearchRequests, fact.Fact.ReportedCostMicrodollars,
			fact.Fact.CostSource, boolInt(fact.Fact.RequestScoped),
			fact.Fact.ClaudeMessageID, fact.Fact.ClaudeRequestID,
			fact.Fact.SourceUUID, fact.Fact.UsageDedupKey)
		if err != nil {
			return err
		}
		dates[fact.LocalDate] = true
	}
	return nil
}

func installUsageRollupDays(
	ctx context.Context, tx *sql.Tx, timezoneID int64,
	location *time.Location, dates map[string]bool,
) error {
	if location == nil {
		location = time.Local
	}
	for date := range dates {
		day, err := time.ParseInLocation(time.DateOnly, date, location)
		if err != nil {
			continue
		}
		next := day.AddDate(0, 0, 1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_rollup_days VALUES(?, ?, ?, ?)
			ON CONFLICT(timezone_id, local_date) DO UPDATE SET
			from_ms=excluded.from_ms, to_ms=excluded.to_ms`, timezoneID, date,
			day.UTC().UnixMilli(), next.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

func usageRollupCallKey(
	snapshot usageQuerySnapshot, fills map[string]usageFillResult,
	pricingHash string,
) string {
	digest := sha256.New()
	writeUsageHashString(digest,
		usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals).Key)
	writeUsageHashString(digest, pricingHash)
	writeUsageHashInt64(digest, snapshot.CursorHighWater)
	for _, version := range snapshot.Versions {
		writeUsageHashString(digest, usageFillCallKey(version))
		writeUsageHashInt64(digest, fills[version.SessionID].InstallRevision)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeUsageHashString(digest hash.Hash, value string) {
	writeUsageHashInt64(digest, int64(len(value)))
	_, _ = digest.Write([]byte(value))
}

func writeUsageHashInt64(digest hash.Hash, value int64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(value))
	_, _ = digest.Write(buffer[:])
}

func writeUsageHashBool(digest hash.Hash, value bool) {
	if value {
		writeUsageHashInt64(digest, 1)
	} else {
		writeUsageHashInt64(digest, 0)
	}
}

func compareStrings(left, right string) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
