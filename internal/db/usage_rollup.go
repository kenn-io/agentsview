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
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
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

func (m usageRollupMetrics) plus(other usageRollupMetrics) usageRollupMetrics {
	return usageRollupMetrics{
		DailyRows:       m.DailyRows + other.DailyRows,
		ExceptionGroups: m.ExceptionGroups + other.ExceptionGroups,
		ExceptionRows:   m.ExceptionRows + other.ExceptionRows,
		BuildDuration:   m.BuildDuration + other.BuildDuration,
		InstallDuration: m.InstallDuration + other.InstallDuration,
	}
}

// usageRollupBuilders bounds the batches one rollup window builds at once.
// Each holds a cache connection, so warm reads keep the rest of the pool.
const usageRollupBuilders = 2

var usageTimezoneIdentityCache sync.Map

func usageTimezoneIdentityFor(
	location *time.Location, _ []usageQueryInterval,
) usageTimezoneIdentity {
	if location == nil {
		location = time.Local //nolint:forbidigo // The report cache identifies the local calendar timezone used for date buckets.
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
	if location != time.Local || (name != "" && name != "Local") { //nolint:forbidigo // The report cache identifies the local calendar timezone used for date buckets.
		return name
	}
	resolved, err := filepath.EvalSymlinks("/etc/localtime")
	if err != nil {
		return name
	}
	normalized := filepath.ToSlash(resolved)
	const marker = "/zoneinfo/"
	_, candidate, ok := strings.CutLast(normalized, marker)
	if !ok {
		return name
	}
	if _, err := time.LoadLocation(candidate); err != nil {
		return name
	}
	return candidate
}

func usageRateHash(
	model, pricedModel, pattern, adjustment string, ok bool, rates export.ModelRates,
) string {
	digest := sha256.New()
	writeUsageHashString(digest, model)
	writeUsageHashString(digest, pricedModel)
	writeUsageHashString(digest, pattern)
	writeUsageHashString(digest, adjustment)
	writeUsageHashBool(digest, ok)
	writeUsageRatesHash(digest, rates)
	return hex.EncodeToString(digest.Sum(nil))
}

func usagePricingIdentity(rows []export.EffectivePricingRow) (string, error) {
	digest, err := export.EffectivePricingDigest(rows)
	if err != nil {
		return "", err
	}
	return usagePricingIdentityWithDigest(digest), nil
}

func usagePricingIdentityWithDigest(digest string) string {
	return digest + "\x00" + pricingpkg.BillingPolicyVersion() + "\x00" + usageSessionPricingIdentityVersion
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
	waiters  int // guarded by usageRollupCoordinator.mu
	installs map[string]usageRollupInstall
	metrics  usageRollupMetrics
	err      error
}

type usageRollupObserver struct {
	beforeEnsure  func()
	beforeAttempt func(map[string]usageFillResult)
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
	zones    map[string]*sync.Mutex
	observer usageRollupObserver
}

func newUsageRollupCoordinator(
	ctx context.Context, cache *usageCache,
) *usageRollupCoordinator {
	return &usageRollupCoordinator{
		cache: cache, ctx: ctx,
		calls: make(map[string]*usageRollupCall),
		zones: make(map[string]*sync.Mutex),
	}
}

func (c *usageRollupCoordinator) Ensure(
	ctx context.Context, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	identity := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals)
	conn, err := c.cache.db.Conn(ctx)
	if err != nil {
		return nil, usageRollupMetrics{}, err
	}
	installs, stale, err := readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, newUsagePricingIdentities(resolver))
	err = errors.Join(err, conn.Close())
	if err != nil {
		return nil, usageRollupMetrics{}, err
	}
	if len(stale) == 0 && installs[usageRollupCursorSessionID].FactRevision >= snapshot.CursorHighWater {
		return installs, usageRollupMetrics{}, nil
	}
	// The catalog digest only keys single-flight; staleness is per session.
	catalogHash := snapshot.PricingIdentity
	batch := snapshot
	batch.Sessions = make([]usageQuerySession, 0, len(stale))
	batch.Versions = make([]usageSourceVersion, 0, len(stale))
	for i, session := range snapshot.Sessions {
		if stale[session.ID] {
			batch.Sessions = append(batch.Sessions, session)
			batch.Versions = append(batch.Versions, snapshot.Versions[i])
		}
	}
	windowFills := make(map[string]usageFillResult, len(batch.Sessions))
	for _, session := range batch.Sessions {
		windowFills[session.ID] = fills[session.ID]
	}
	built, metrics, err := c.ensureWindow(ctx, batch, windowFills, resolver, catalogHash)
	if err != nil {
		return nil, metrics, err
	}
	maps.Copy(installs, built)
	return installs, metrics, nil
}

func (c *usageRollupCoordinator) ensureWindow(
	ctx context.Context, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver,
	catalogHash string,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	key := usageRollupCallKey(snapshot, fills, catalogHash)
	c.mu.Lock()
	call := c.calls[key]
	if call == nil {
		zoneKey := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals).Key
		zone := c.zones[zoneKey]
		if zone == nil {
			zone = &sync.Mutex{}
			c.zones[zoneKey] = zone
		}
		call = &usageRollupCall{done: make(chan struct{})}
		c.calls[key] = call
		if !c.cache.startDetachedWork(func() {
			if c.observer.beforeEnsure != nil {
				c.observer.beforeEnsure()
			}
			// Wait before acquiring a pooled connection. Once a batch starts,
			// it finishes for reuse even if all its callers leave.
			zone.Lock()
			c.mu.Lock()
			if call.waiters == 0 {
				call.err = context.Canceled
				delete(c.calls, key)
				close(call.done)
				c.mu.Unlock()
				zone.Unlock()
				return
			}
			c.mu.Unlock()
			call.installs, call.metrics, call.err = c.ensureBatches(
				call, key, snapshot, fills, resolver, catalogHash)
			zone.Unlock()
			c.mu.Lock()
			if c.calls[key] == call {
				delete(c.calls, key)
			}
			close(call.done)
			c.mu.Unlock()
		}) {
			delete(c.calls, key)
			c.mu.Unlock()
			return nil, usageRollupMetrics{}, fmt.Errorf(
				"%w before starting detached rollup", errUsageCacheSourceChanged,
			)
		}
	}
	call.waiters++
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		call.waiters--
		c.mu.Unlock()
		return nil, usageRollupMetrics{}, ctx.Err()
	case <-call.done:
		return call.installs, call.metrics, call.err
	}
}

// ensureBatches builds a window's rollups in batches of
// usageFillInstallBatchSize sessions. Up to usageRollupBuilders batches build
// at once, each on its own connection and pricing resolver, and they install
// one at a time in batch order, so each install holds the write lock for one
// batch. Once every caller has left, batches that have not started to install
// are dropped; the next request rebuilds them.
//
// Batches also take their connections in batch order. A batch holds its
// connection while it waits for the earlier batch to install, so the earliest
// unfinished batch of every window always holds one and can finish. If a later
// batch could take a connection first, concurrent windows could fill the pool
// with batches that all wait on earlier batches that have none.
func (c *usageRollupCoordinator) ensureBatches(
	call *usageRollupCall, key string, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver,
	catalogHash string,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	var batches []usageQuerySnapshot
	var batchFills []map[string]usageFillResult
	// The first batch runs even when empty so a stale Cursor build lands.
	for first := true; first || len(snapshot.Sessions) > 0; first = false {
		size := min(len(snapshot.Sessions), usageFillInstallBatchSize)
		batch := snapshot
		batch.Sessions = snapshot.Sessions[:size]
		batch.Versions = snapshot.Versions[:size]
		batches = append(batches, batch)
		batchFill := make(map[string]usageFillResult, size)
		for _, session := range batch.Sessions {
			batchFill[session.ID] = fills[session.ID]
		}
		batchFills = append(batchFills, batchFill)
		snapshot.Sessions = snapshot.Sessions[size:]
		snapshot.Versions = snapshot.Versions[size:]
		snapshot.CursorHighWater = 0
	}
	turns := make([]chan struct{}, len(batches)+1)
	acquired := make([]chan struct{}, len(batches)+1)
	for i := range turns {
		turns[i] = make(chan struct{})
		acquired[i] = make(chan struct{})
	}
	close(turns[0])
	close(acquired[0])
	installs := make(map[string]usageRollupInstall)
	var metrics usageRollupMetrics
	var errs error
	var mu sync.Mutex
	var next atomic.Int64
	var wait sync.WaitGroup
	for builder := range min(usageRollupBuilders, len(batches)) {
		builderResolver := resolver
		if builder > 0 {
			builderResolver = resolver.Copy()
		}
		wait.Go(func() {
			// A batch proceeds while callers wait and no earlier batch failed.
			proceed := func() bool {
				mu.Lock()
				failed := errs != nil
				mu.Unlock()
				return !failed && c.keepCall(call, key)
			}
			for i := int(next.Add(1) - 1); i < len(batches); i = int(next.Add(1) - 1) {
				<-acquired[i]
				if !proceed() {
					close(acquired[i+1])
					close(turns[i+1])
					continue
				}
				conn, err := c.cache.db.Conn(c.ctx)
				close(acquired[i+1])
				var built map[string]usageRollupInstall
				var measured usageRollupMetrics
				if err == nil {
					canInstall := func() bool {
						<-turns[i]
						return proceed()
					}
					built, measured, err = c.ensureNow(
						c.ctx, conn, batches[i], batchFills[i], builderResolver, catalogHash, canInstall)
					err = errors.Join(err, conn.Close())
				}
				close(turns[i+1])
				mu.Lock()
				maps.Copy(installs, built)
				metrics = metrics.plus(measured)
				errs = errors.Join(errs, err)
				mu.Unlock()
			}
		})
	}
	wait.Wait()
	return installs, metrics, errs
}

// keepCall reports whether call still has waiters. An abandoned call leaves
// the coordinator, so a later request starts its own call instead of joining
// one that drops batches.
func (c *usageRollupCoordinator) keepCall(call *usageRollupCall, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call.waiters > 0 {
		return true
	}
	if c.calls[key] == call {
		delete(c.calls, key)
	}
	return false
}

// ensureNow aggregates committed per-session facts out of the usage cache. It
// never reads the archive, so an append landing mid-build cannot invalidate
// it: that session's own notification fill installs new facts, which makes its
// rollup install stale and rebuilds it on a later aggregation.
func (c *usageRollupCoordinator) ensureNow(
	ctx context.Context, conn *sql.Conn, snapshot usageQuerySnapshot,
	fills map[string]usageFillResult, resolver *export.PricingResolver, catalogHash string,
	canInstall func() bool,
) (map[string]usageRollupInstall, usageRollupMetrics, error) {
	identity := usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals)
	pricing := newUsagePricingIdentities(resolver)
	var err error
	var metrics usageRollupMetrics
	currentFills := maps.Clone(fills)
	// A concurrent fill can replace a session's facts or add a sibling to a
	// dedup identity this build finalized. The install transaction rejects
	// both races. Refresh committed facts before the next attempt instead of
	// failing the caller.
	for attempt := 1; ; attempt++ {
		installs, done, attemptMetrics, attemptErr := c.ensureAttempt(
			ctx, conn, identity, snapshot, currentFills, pricing, catalogHash, canInstall)
		metrics = metrics.plus(attemptMetrics)
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
			return nil, metrics, errors.New("usage rollup build kept losing dedup classification races")
		}
	}
}

// ensureAttempt performs one read-build-install cycle. It reports done when
// every in-scope install matches the facts and metadata it must reflect.
func (c *usageRollupCoordinator) ensureAttempt(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	snapshot usageQuerySnapshot, fills map[string]usageFillResult,
	pricing *usagePricingIdentities, catalogHash string, canInstall func() bool,
) (map[string]usageRollupInstall, bool, usageRollupMetrics, error) {
	if c.observer.beforeAttempt != nil {
		c.observer.beforeAttempt(fills)
	}
	installs, stale, err := readUsageRollupInstalls(
		ctx, conn, identity, snapshot, fills, pricing)
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
		pricing, cross)
	if err != nil {
		return nil, false, usageRollupMetrics{}, err
	}
	if installs[usageRollupCursorSessionID].FactRevision < snapshot.CursorHighWater {
		cursorBuild, cursorErr := loadCursorUsageRollupBuild(
			ctx, conn, snapshot.CursorHighWater, snapshot.location, pricing)
		if cursorErr != nil {
			return nil, false, usageRollupMetrics{}, cursorErr
		}
		builds = append(builds, cursorBuild)
	}
	for i := range builds {
		builds[i].SourcePricing = catalogHash
		builds[i].ChangeRevision = snapshot.ChangeRevision
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
	if !canInstall() {
		return nil, true, metrics, nil
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
		ctx, conn, identity, snapshot, fills, pricing)
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
	results, err := readUsageCachedSessions(ctx, conn, usageSourceVersionIDs(versions))
	if err != nil {
		return nil, err
	}
	for _, version := range versions {
		if _, ok := results[version.SessionID]; !ok {
			results[version.SessionID] = usageFillResult{Deleted: true}
		}
	}
	return results, nil
}

func readUsageRollupInstalls(
	ctx context.Context, conn *sql.Conn, identity usageTimezoneIdentity,
	snapshot usageQuerySnapshot, fills map[string]usageFillResult,
	pricing *usagePricingIdentities,
) (map[string]usageRollupInstall, map[string]bool, error) {
	query := `SELECT i.id, i.session_id,
		i.fact_install_revision, i.install_revision, i.cached_at,
		i.source_sync_marker, i.source_transcript_rev, i.usage_event_fingerprint,
		i.baked_agent, i.baked_started_at, i.project, i.machine, i.git_branch, i.is_automated, i.user_message_count, i.group_key, i.group_label, i.pricing_inputs, i.pricing_hash
		FROM usage_rollup_timezones tz JOIN usage_rollup_installs i
		  ON i.timezone_id = tz.id WHERE tz.timezone_key = ?`
	args := []any{identity.Key}
	// Scope the read to the sessions this snapshot can consult, so a batched
	// backfill pass stays linear in archive size instead of re-reading every
	// installed row per batch. Sessions and Versions carry the same IDs in the
	// same order from capture through ordering and batch slicing. Archive-scale
	// snapshots exceed the bind limit and read unscoped, which returns a
	// superset of the same rows.
	scope := make([]string, 1, len(snapshot.Versions)+1)
	scope[0] = usageRollupCursorSessionID
	for _, version := range snapshot.Versions {
		scope = append(scope, version.SessionID)
	}
	if len(scope) < maxSQLVars {
		placeholders, scopeArgs := InPlaceholders(scope)
		query += ` AND i.session_id IN ` + placeholders
		args = append(args, scopeArgs...)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	installs := make(map[string]usageRollupInstall)
	sources := make(map[string]usageSourceVersion)
	baked := make(map[string]usageBakedSession)
	pricingInputs := make(map[string]string)
	for rows.Next() {
		var item usageRollupInstall
		var source usageSourceVersion
		var installed usageBakedSession
		var installedInputs string
		if err := rows.Scan(&item.ID, &item.SessionID, &item.FactRevision,
			&item.InstallRevision, &item.CachedAt, &source.SyncMarker,
			&source.TranscriptRevision, &source.UsageEventFingerprint,
			&installed.Agent, &installed.StartedAt, &installed.Project, &installed.Machine, &installed.GitBranch, &installed.IsAutomated, &installed.UserMessageCount, &installed.GroupKey, &installed.GroupLabel, &installedInputs, &item.PricingHash); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		source.SessionID = item.SessionID
		installs[item.SessionID], sources[item.SessionID] = item, source
		baked[item.SessionID] = installed
		pricingInputs[item.SessionID] = installedInputs
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
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
			baked[version.SessionID] != bakedUsageSession(session) {
			stale[version.SessionID] = true
			continue
		}
		// Only this session's own lookups decide pricing staleness.
		expected, err := pricing.forEncoded(pricingInputs[version.SessionID])
		if err != nil {
			return nil, nil, err
		}
		if item.PricingHash != expected {
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
			// Roll back even after cancellation so the pooled connection is clean.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
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
	sessionIDs := make([]string, 0, len(builds))
	for _, build := range builds {
		if build.SessionID != usageRollupCursorSessionID {
			sessionIDs = append(sessionIDs, build.SessionID)
		}
	}
	cachedSessions, err := readUsageCachedSessions(ctx, conn, sessionIDs)
	if err != nil {
		return err
	}
	for _, build := range builds {
		if build.SessionID == usageRollupCursorSessionID {
			continue
		}
		current, ok := cachedSessions[build.SessionID]
		if !ok || current.InstallRevision != build.FactRevision ||
			!current.source.Equal(build.Source) {
			return fmt.Errorf(
				"%w: usage facts changed during rollup build",
				errUsageCacheSourceChanged)
		}
	}
	// Background rebuilds install here too, so only a new zone takes the
	// install time; recordUsageTimezoneRequest owns later recency stamps.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := conn.ExecContext(ctx, `INSERT INTO usage_rollup_timezones(
		timezone_key, timezone_name, interval_fingerprint, last_requested_at
	) VALUES (?, ?, ?, ?) ON CONFLICT(timezone_key) DO UPDATE SET
		timezone_name=excluded.timezone_name,
		interval_fingerprint=excluded.interval_fingerprint`,
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
	stmts := newUsageStatementCache(conn)
	defer func() { _ = stmts.Close() }()
	dates := make(map[string]bool)
	for _, build := range builds {
		var installID int64
		var installedFactRevision int64
		err := stmts.QueryRowContext(ctx, `SELECT id, fact_install_revision
			FROM usage_rollup_installs
			WHERE timezone_id = ? AND session_id = ?`, timezoneID, build.SessionID).
			Scan(&installID, &installedFactRevision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// A delayed build can replace a newer install, or one a dedup
		// invalidation removed. Replay journal changes from its snapshot on
		// the next read, even when the zone already claimed those changes.
		if _, err := stmts.ExecContext(ctx, `UPDATE usage_rollup_timezones
			SET change_revision=MIN(change_revision, ?) WHERE id=?`, build.ChangeRevision, timezoneID); err != nil {
			return err
		}
		if err == nil && build.SessionID == usageRollupCursorSessionID &&
			installedFactRevision > build.FactRevision {
			return fmt.Errorf(
				"%w: Cursor usage advanced during rollup build",
				errUsageCacheSourceChanged)
		}
		if err == nil {
			if _, err := stmts.ExecContext(ctx, `DELETE FROM usage_daily_rollups WHERE rollup_install_id = ?`, installID); err != nil {
				return err
			}
			for _, table := range []string{
				"usage_activity_rollups", "usage_rollup_exceptions",
			} {
				if _, err := stmts.ExecContext(ctx, `DELETE FROM `+table+
					` WHERE rollup_install_id = ?`, installID); err != nil {
					return err
				}
			}
			_, err = stmts.ExecContext(ctx, `UPDATE usage_rollup_installs SET
				source_sync_marker=?, source_transcript_rev=?, usage_event_fingerprint=?,
				fact_install_revision=?, baked_agent=?, baked_started_at=?, project=?, machine=?, git_branch=?, is_automated=?, user_message_count=?, group_key=?, group_label=?, has_authoritative=?,
				pricing_inputs=?, pricing_hash=?, install_revision=?, cached_at=?, source_change_revision=?, source_pricing=?
				WHERE id=?`,
				build.Source.SyncMarker, build.Source.TranscriptRevision,
				build.Source.UsageEventFingerprint, build.FactRevision,
				build.Agent, build.StartedAt, build.Project, build.Machine, build.GitBranch, build.IsAutomated, build.UserMessageCount, build.GroupKey, build.GroupLabel, build.HasAuthoritative, build.PricingInputs, build.PricingHash,
				nextRevision, now, build.ChangeRevision, build.SourcePricing, installID)
		} else {
			result, insertErr := stmts.ExecContext(ctx, `INSERT INTO usage_rollup_installs(
				timezone_id, session_id, source_sync_marker, source_transcript_rev,
				usage_event_fingerprint, fact_install_revision, baked_agent,
				baked_started_at, project, machine, git_branch, is_automated, user_message_count, group_key, group_label, has_authoritative, pricing_inputs, pricing_hash, install_revision,
				cached_at, source_change_revision, source_pricing
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, timezoneID, build.SessionID,
				build.Source.SyncMarker, build.Source.TranscriptRevision,
				build.Source.UsageEventFingerprint, build.FactRevision,
				build.Agent, build.StartedAt, build.Project, build.Machine, build.GitBranch, build.IsAutomated, build.UserMessageCount, build.GroupKey, build.GroupLabel, build.HasAuthoritative, build.PricingInputs, build.PricingHash,
				nextRevision, now, build.ChangeRevision, build.SourcePricing)
			if insertErr != nil {
				return insertErr
			}
			installID, err = result.LastInsertId()
		}
		if err != nil {
			return err
		}
		if err := installUsageRollupRows(ctx, stmts, installID, build, dates); err != nil {
			return err
		}
		if _, err := stmts.ExecContext(ctx, `DELETE FROM usage_rollup_invalidations WHERE timezone_id=? AND session_id=?`, timezoneID, build.SessionID); err != nil {
			return err
		}
		nextRevision++
	}
	if _, err := stmts.ExecContext(ctx, `UPDATE usage_cache_metadata SET value=?
		WHERE key=?`, strconv.FormatInt(nextRevision, 10),
		usageCacheMetadataNextRollupRevision); err != nil {
		return err
	}
	if err := installUsageRollupDays(ctx, stmts, timezoneID, location, dates); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing usage rollup install: %w", err)
	}
	committed = true
	return nil
}

func installUsageRollupRows(
	ctx context.Context, conn *usageStatementCache, installID int64,
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
	ctx context.Context, conn *usageStatementCache, timezoneID int64,
	location *time.Location, dates map[string]bool,
) error {
	if location == nil {
		location = time.Local //nolint:forbidigo // The report cache identifies the local calendar timezone used for date buckets.
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
	catalogHash string,
) string {
	digest := sha256.New()
	writeUsageHashString(digest,
		usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals).Key)
	writeUsageHashString(digest, catalogHash)
	writeUsageHashInt64(digest, snapshot.CursorHighWater)
	writeUsageHashInt64(digest, snapshot.ChangeRevision)
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
		writeUsageHashString(digest, session.Project)
		writeUsageHashString(digest, session.Machine)
		writeUsageHashString(digest, session.GitBranch)
		writeUsageHashString(digest, session.GroupKey)
		writeUsageHashString(digest, parser.HermesCronJobName(session.GroupKey, session.SessionName))
		writeUsageHashBool(digest, session.IsAutomated)
		writeUsageHashInt64(digest, int64(session.UserMessageCount))
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

// usageBakedSession is the archive metadata that determines installed rows and totals.
type usageBakedSession struct {
	Agent, StartedAt, Project, Machine, GitBranch string
	GroupKey, GroupLabel                          string
	IsAutomated                                   bool
	UserMessageCount                              int
}

func bakedUsageSession(session usageQuerySession) usageBakedSession {
	return usageBakedSession{session.Agent, session.StartedAt, session.Project, session.Machine, session.GitBranch, session.GroupKey, parser.HermesCronJobName(session.GroupKey, session.SessionName), session.IsAutomated, session.UserMessageCount}
}
