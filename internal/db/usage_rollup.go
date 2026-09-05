package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const usageRollupCursorSessionID = "\x00cursor"

// usageRollupMaxBuildAttempts bounds the reclassification retry an install
// transaction can request when a concurrent fill changes dedup membership.
// Each attempt reads freshly committed facts, so it makes real progress.
const usageRollupMaxBuildAttempts = 8

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

var usageTimezoneIdentityCache sync.Map

func usageTimezoneIdentityFor(
	location *time.Location, _ []usageQueryInterval,
) usageTimezoneIdentity {
	if location == nil {
		location = time.Local
	}
	// Production locations come from time.LoadLocation or time.Local, so one
	// zone name maps to one rule set for the life of the process. Keying by
	// name keeps the cache bounded even though LoadLocation returns a fresh
	// *time.Location on every call.
	cacheKey := location.String()
	if cached, ok := usageTimezoneIdentityCache.Load(cacheKey); ok {
		return cached.(usageTimezoneIdentity)
	}
	name := usageLocationName(location)
	fingerprint := usageTimezoneRuleFingerprint(name, location)
	key := name + ":" + fingerprint
	if name == "" || name == "Local" {
		key = "local:" + fingerprint
	}
	identity := usageTimezoneIdentity{
		Key: key, Name: name, IntervalFingerprint: fingerprint,
	}
	actual, _ := usageTimezoneIdentityCache.LoadOrStore(cacheKey, identity)
	return actual.(usageTimezoneIdentity)
}

func usageTimezoneRuleFingerprint(name string, location *time.Location) string {
	digest := sha256.New()
	writeUsageHashString(digest, name)
	// Hash every rule regime and its exact end instant from 1970 through 2100
	// so any zoneinfo update that adds, removes, or moves a transition in that
	// range selects a new rollup generation. Usage timestamps outside the range
	// would be clock skew, so rule changes beyond it are out of scope.
	end := time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC)
	for cursor := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC); cursor.Before(end); {
		localized := cursor.In(location)
		zone, offset := localized.Zone()
		writeUsageHashString(digest, zone)
		writeUsageHashInt64(digest, int64(offset))
		_, regimeEnd := localized.ZoneBounds()
		if regimeEnd.IsZero() || !regimeEnd.After(cursor) {
			break
		}
		writeUsageHashInt64(digest, regimeEnd.Unix())
		cursor = regimeEnd
	}
	return hex.EncodeToString(digest.Sum(nil))
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

func usagePricingIdentity(rows []export.EffectivePricingRow) (string, error) {
	digest, err := export.EffectivePricingDigest(rows)
	if err != nil {
		return "", err
	}
	return digest + "\x00" + pricingpkg.BillingPolicyVersion(), nil
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
		rates.CacheWritePerMTok.Microdollars,
		rates.CacheWrite1hPerMTok.Microdollars,
		rates.CacheReadPerMTok.Microdollars,
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
			band.CacheWrite1hPerMTok.Microdollars,
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
			CacheWritePerMTok:   band.CacheWritePerMTok,
			CacheWrite1hPerMTok: band.CacheWrite1hPerMTok,
			CacheReadPerMTok:    band.CacheReadPerMTok,
		})
	}
	return result
}

func validateNonnegativeUsageRates(rates export.ModelRates) error {
	for _, item := range append([]export.ModelRates{rates}, usageBandRates(rates)...) {
		if item.InputPerMTok.Microdollars < 0 || item.OutputPerMTok.Microdollars < 0 ||
			item.CacheWritePerMTok.Microdollars < 0 ||
			item.CacheWrite1hPerMTok.Microdollars < 0 ||
			item.CacheReadPerMTok.Microdollars < 0 {
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

type usageRollupObserver struct {
	beforeEnsure  func()
	beforeInstall func([]usageRollupBuild)
}

// usageRollupCoordinator aggregates committed per-session facts out of the
// usage cache. It deliberately holds no archive handle: the archive is read
// only when a session's facts are filled.
type usageRollupCoordinator struct {
	cache    *usageCache
	ctx      context.Context
	mu       sync.Mutex
	calls    map[string]*usageRollupCall
	observer usageRollupObserver
}

func newUsageRollupCoordinator(
	ctx context.Context, cache *usageCache,
) *usageRollupCoordinator {
	return &usageRollupCoordinator{
		cache: cache, ctx: ctx,
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
	pricingHash, err := usagePricingIdentity(snapshot.PricingRows)
	if err != nil {
		return nil, usageRollupMetrics{}, fmt.Errorf("hashing usage pricing: %w", err)
	}
	key := usageRollupCallKey(snapshot, fills, pricingHash)
	c.mu.Lock()
	call := c.calls[key]
	if call == nil {
		call = &usageRollupCall{done: make(chan struct{})}
		c.calls[key] = call
		if !c.cache.startDetachedWork(func() {
			if c.observer.beforeEnsure != nil {
				c.observer.beforeEnsure()
			}
			call.installs, call.metrics, call.err = c.ensureNow(
				c.ctx, snapshot, fills, resolver, pricingHash)
			close(call.done)
			c.mu.Lock()
			delete(c.calls, key)
			c.mu.Unlock()
		}) {
			delete(c.calls, key)
			c.mu.Unlock()
			return nil, usageRollupMetrics{}, fmt.Errorf(
				"%w before starting detached rollup", errUsageCacheSourceChanged,
			)
		}
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, usageRollupMetrics{}, ctx.Err()
	case <-call.done:
		return call.installs, call.metrics, call.err
	}
}

// ensureNow aggregates committed per-session facts out of the usage cache. It
// never reads the archive, so an append landing mid-build cannot invalidate
// it: that session's own notification fill installs new facts, which makes its
// rollup install stale and rebuilds it on a later aggregation.
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
	var metrics usageRollupMetrics
	currentFills := maps.Clone(fills)
	// A concurrent fill can replace a session's facts or add a sibling to a
	// dedup identity this build finalized. The install transaction rejects
	// both races. Refresh committed facts before the next attempt instead of
	// failing the caller.
	for attempt := 1; ; attempt++ {
		installs, done, attemptMetrics, attemptErr := c.ensureAttempt(
			ctx, conn, identity, snapshot, currentFills, resolver, pricingHash)
		metrics.DailyRows += attemptMetrics.DailyRows
		metrics.ExceptionRows += attemptMetrics.ExceptionRows
		metrics.ExceptionGroups += attemptMetrics.ExceptionGroups
		metrics.BuildDuration += attemptMetrics.BuildDuration
		metrics.InstallDuration += attemptMetrics.InstallDuration
		if attemptErr != nil {
			return nil, metrics, attemptErr
		}
		if done {
			return installs, metrics, nil
		}
		currentFills, err = readCurrentUsageFillResults(
			ctx, conn, snapshot.Versions)
		if err != nil {
			return nil, metrics, err
		}
		if attempt >= usageRollupMaxBuildAttempts {
			return nil, metrics, fmt.Errorf(
				"usage rollup build kept losing dedup classification races")
		}
	}
}

// ensureAttempt performs one read-build-install cycle. It reports done when
// every in-scope install matches the facts and metadata it must reflect.
func (c *usageRollupCoordinator) ensureAttempt(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	snapshot usageQuerySnapshot, fills map[string]usageFillResult,
	resolver *export.PricingResolver, pricingHash string,
) (map[string]usageRollupInstall, bool, usageRollupMetrics, error) {
	installs, stale, err := readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, pricingHash)
	if err != nil || len(stale) == 0 {
		cursorCurrent := installs[usageRollupCursorSessionID].FactRevision >=
			snapshot.CursorHighWater
		if err != nil || cursorCurrent {
			return installs, err == nil, usageRollupMetrics{}, err
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
		if !stale[version.SessionID] {
			continue
		}
		fill := fills[version.SessionID]
		// Record the source version the cached facts came from, not the
		// one the caller's archive snapshot observed.
		staleVersions[version.SessionID] = fill.source
		staleFills[version.SessionID] = fill
	}
	started := time.Now()
	facts, err := loadUsageRollupFacts(ctx, conn, staleSessions)
	if err != nil {
		return nil, false, usageRollupMetrics{}, err
	}
	cross, err := loadUsageRollupCrossIdentities(ctx, conn)
	if err != nil {
		return nil, false, usageRollupMetrics{}, err
	}
	builds, err := buildUsageRollupSessions(
		facts, staleSessions, staleVersions, staleFills, snapshot.location,
		resolver, pricingHash, cross)
	if err != nil {
		return nil, false, usageRollupMetrics{}, err
	}
	if installs[usageRollupCursorSessionID].FactRevision < snapshot.CursorHighWater {
		cursorBuild, cursorErr := loadCursorUsageRollupBuild(
			ctx, conn, snapshot.CursorHighWater, snapshot.location, pricingHash)
		if cursorErr != nil {
			return nil, false, usageRollupMetrics{}, cursorErr
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
	installStarted := time.Now()
	if c.observer.beforeInstall != nil {
		c.observer.beforeInstall(slices.Clone(builds))
	}
	err = installUsageRollupBuilds(
		ctx, conn, identity, snapshot.location, builds, cross)
	metrics.InstallDuration = time.Since(installStarted)
	if err != nil {
		if errors.Is(err, errUsageCacheSourceChanged) {
			return nil, false, metrics, nil
		}
		return nil, false, metrics, err
	}
	installs, stale, err = readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, pricingHash)
	if err != nil {
		return nil, false, metrics, err
	}
	if len(stale) != 0 ||
		installs[usageRollupCursorSessionID].FactRevision < snapshot.CursorHighWater {
		// Another builder installed a different generation of one of these
		// sessions while this one was building. Its facts are committed, so
		// the next attempt sees them.
		return nil, false, metrics, nil
	}
	return installs, true, metrics, nil
}

func readCurrentUsageFillResults(
	ctx context.Context, conn *sql.Conn, versions []usageSourceVersion,
) (map[string]usageFillResult, error) {
	results := make(map[string]usageFillResult, len(versions))
	for _, version := range versions {
		var result usageFillResult
		err := conn.QueryRowContext(ctx, `SELECT source_sync_marker,
			source_transcript_rev, usage_event_fingerprint, install_revision
			FROM usage_cached_sessions WHERE session_id = ?`, version.SessionID).Scan(
			&result.source.SyncMarker, &result.source.TranscriptRevision,
			&result.source.UsageEventFingerprint, &result.InstallRevision)
		if errors.Is(err, sql.ErrNoRows) {
			results[version.SessionID] = usageFillResult{Deleted: true}
			continue
		}
		if err != nil {
			return nil, err
		}
		result.source.SessionID = version.SessionID
		results[version.SessionID] = result
	}
	return results, nil
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
	// Staleness is decided entirely against the usage cache: an install is
	// stale when the per-session facts it was built from are no longer the
	// facts the cache holds. The archive is not consulted here.
	stale := make(map[string]bool)
	for _, version := range snapshot.Versions {
		fill := fills[version.SessionID]
		item, ok := installs[version.SessionID]
		session := sessions[version.SessionID]
		if fill.Deleted {
			continue
		}
		if !ok || item.FactRevision != fill.InstallRevision ||
			!sources[version.SessionID].Equal(fill.source) ||
			baked[version.SessionID] != [2]string{session.Agent, session.StartedAt} ||
			pricing[version.SessionID] != pricingHash {
			stale[version.SessionID] = true
		}
	}
	return installs, stale, nil
}

func installUsageRollupBuilds(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	location *time.Location, builds []usageRollupBuild,
	buildCross usageDedupIdentitySet,
) error {
	// Take the cache write lock up front. A deferred transaction that
	// reads before its first write fails with SQLITE_BUSY_SNAPSHOT
	// when a concurrent fill or install commits after the read
	// snapshot forms; BEGIN IMMEDIATE serializes writers through the
	// busy timeout instead.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("locking usage cache for rollup install: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	// A fill committing between classification and this install can add a
	// sibling for an identity that was finalized into a daily row. Abort on
	// new crossness; crossness that disappeared keeps the conservative
	// exception rows exact.
	currentCross, err := loadUsageRollupCrossIdentities(ctx, conn)
	if err != nil {
		return err
	}
	if !currentCross.subsetOf(buildCross) {
		return fmt.Errorf(
			"%w: dedup identities gained members during rollup build",
			errUsageCacheSourceChanged)
	}
	for _, build := range builds {
		if build.SessionID == usageRollupCursorSessionID {
			continue
		}
		var current usageFillResult
		err := conn.QueryRowContext(ctx, `SELECT source_sync_marker,
			source_transcript_rev, usage_event_fingerprint, install_revision
			FROM usage_cached_sessions WHERE session_id = ?`, build.SessionID).Scan(
			&current.source.SyncMarker, &current.source.TranscriptRevision,
			&current.source.UsageEventFingerprint, &current.InstallRevision)
		current.source.SessionID = build.SessionID
		if errors.Is(err, sql.ErrNoRows) || err == nil &&
			(current.InstallRevision != build.FactRevision ||
				!current.source.Equal(build.Source)) {
			return fmt.Errorf(
				"%w: usage facts changed during rollup build",
				errUsageCacheSourceChanged)
		}
		if err != nil {
			return err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `INSERT INTO usage_rollup_timezones(
		timezone_key, timezone_name, interval_fingerprint, last_requested_at
	) VALUES (?, ?, ?, ?) ON CONFLICT(timezone_key) DO UPDATE SET
		timezone_name=excluded.timezone_name,
		interval_fingerprint=excluded.interval_fingerprint,
		last_requested_at=excluded.last_requested_at`,
		identity.Key, identity.Name, identity.IntervalFingerprint, now); err != nil {
		return err
	}
	var timezoneID int64
	if err := conn.QueryRowContext(ctx, `SELECT id FROM usage_rollup_timezones
		WHERE timezone_key = ?`, identity.Key).Scan(&timezoneID); err != nil {
		return err
	}
	var revisionText string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM usage_cache_metadata
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
		var installedFactRevision int64
		err := conn.QueryRowContext(ctx, `SELECT id, fact_install_revision
			FROM usage_rollup_installs
			WHERE timezone_id = ? AND session_id = ?`, timezoneID, build.SessionID).
			Scan(&installID, &installedFactRevision)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && build.SessionID == usageRollupCursorSessionID &&
			installedFactRevision > build.FactRevision {
			return fmt.Errorf(
				"%w: Cursor usage advanced during rollup build",
				errUsageCacheSourceChanged)
		}
		if err == nil {
			for _, table := range []string{
				"usage_daily_rollups", "usage_activity_rollups", "usage_rollup_exceptions",
			} {
				if _, err := conn.ExecContext(ctx, `DELETE FROM `+table+
					` WHERE rollup_install_id = ?`, installID); err != nil {
					return err
				}
			}
			_, err = conn.ExecContext(ctx, `UPDATE usage_rollup_installs SET
				source_sync_marker=?, source_transcript_rev=?, usage_event_fingerprint=?,
				fact_install_revision=?, baked_agent=?, baked_started_at=?,
				pricing_hash=?, install_revision=?, cached_at=? WHERE id=?`,
				build.Source.SyncMarker, build.Source.TranscriptRevision,
				build.Source.UsageEventFingerprint, build.FactRevision,
				build.Agent, build.StartedAt, build.PricingHash,
				nextRevision, now, installID)
		} else {
			result, insertErr := conn.ExecContext(ctx, `INSERT INTO usage_rollup_installs(
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
		if err := installUsageRollupRows(ctx, conn, installID, build, dates); err != nil {
			return err
		}
		nextRevision++
	}
	if _, err := conn.ExecContext(ctx, `UPDATE usage_cache_metadata SET value=?
		WHERE key=?`, strconv.FormatInt(nextRevision, 10),
		usageCacheMetadataNextRollupRevision); err != nil {
		return err
	}
	if err := installUsageRollupDays(ctx, conn, timezoneID, location, dates); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing usage rollup install: %w", err)
	}
	committed = true
	return nil
}

func installUsageRollupRows(
	ctx context.Context, conn *sql.Conn, installID int64,
	build usageRollupBuild, dates map[string]bool,
) error {
	for _, row := range build.Daily {
		band := -1
		if row.BandThreshold != nil {
			band = *row.BandThreshold
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO usage_daily_rollups VALUES(
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			installID, row.LocalDate, row.ReportedModel, row.ProviderID,
			row.PricedModel, row.MatchedPattern, boolInt(row.RateOK), row.RateHash,
			row.PricingTimestamp, band,
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
		if _, err := conn.ExecContext(ctx, `INSERT INTO usage_activity_rollups VALUES(?, ?, ?, ?)`,
			installID, row.LocalDate, row.Model, row.UserMessageCount); err != nil {
			return err
		}
		dates[row.LocalDate] = true
	}
	for _, row := range build.Exceptions {
		fact := row.Fact
		_, err := conn.ExecContext(ctx, `INSERT INTO usage_rollup_exceptions VALUES(
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			installID, row.GroupKind, row.GroupKey, fact.CachedSessionID,
			fact.FactIndex, fact.SourceSessionID, fact.LocalDate, fact.Fact.Source,
			fact.Fact.MessageOrdinal, fact.Fact.TimestampMillis, fact.Fact.TimestampNanos,
			fact.Fact.RawTimestamp, boolInt(fact.Fact.UsesSessionStart), fact.Model,
			fact.Fact.ProviderID,
			fact.Fact.InputTokens, fact.Fact.OutputTokens, fact.Fact.ReasoningTokens,
			fact.Fact.CacheCreationTokens, fact.Fact.CacheCreation1hTokens,
			fact.Fact.CacheReadTokens,
			fact.Fact.WebSearchRequests, fact.Fact.ReportedCostMicrodollars,
			fact.Fact.CostSource, boolInt(fact.Fact.RequestScoped), boolInt(fact.IsHeadless),
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
	ctx context.Context, conn *sql.Conn, timezoneID int64,
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
		if _, err := conn.ExecContext(ctx, `INSERT INTO usage_rollup_days VALUES(?, ?, ?, ?)
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
	sessions := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		sessions[session.ID] = session
	}
	for _, version := range snapshot.Versions {
		fill := fills[version.SessionID]
		// Key on the source the cached facts came from so two requests
		// whose archive snapshots differ still share one build when the
		// facts they aggregate are identical.
		writeUsageHashString(digest, version.SessionID)
		writeUsageHashString(digest, usageFillCallKey(fill.source))
		writeUsageHashInt64(digest, fill.InstallRevision)
		writeUsageHashBool(digest, fill.Deleted)
		session := sessions[version.SessionID]
		writeUsageHashString(digest, session.Agent)
		writeUsageHashString(digest, session.StartedAt)
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
