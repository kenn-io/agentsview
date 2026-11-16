package sync

import (
	"context"
	"errors"
	"log"
	"maps"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func providerProcessCacheKey(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	providerSemantics parser.ProviderSyncSemantics,
) string {
	key := plannedSkipKey(source, fingerprint)
	if key == "" {
		key = file.Path
	}
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	key = providerAgentSkipCacheKey(key, agent)
	key = providerProcessCacheKeyWithHash(
		key, fingerprint, providerSemantics,
	)
	// A whole-container cache identity includes the parser data version: the
	// container has no stored row of its own, so a restart must not accept
	// an entry recorded by an older parser version.
	if providerPersistentSharedContainerSource(source) {
		separator := "?"
		if strings.Contains(key, "?") {
			separator = "&"
		}
		key += separator + "data_version=" + strconv.Itoa(db.AgentDataVersion(agent))
	}
	return key
}

// providerAgentSkipCacheKey prevents providers with overlapping roots or a
// shared on-disk format from inheriting another agent's cached source state.
// The path stays first so remote-cache path translation remains valid.
func providerAgentSkipCacheKey(key string, agent parser.AgentType) string {
	if key == "" || agent == "" {
		return key
	}
	return key + db.ProviderAgentSkipMarker + string(agent)
}

// SplitProviderSkipCachePath separates the filesystem path from the provider
// qualifier carried by a skip-cache identity. Remote import translates only
// the path and reattaches the suffix after the path mapping succeeds.
func SplitProviderSkipCachePath(key string) (path, suffix string) {
	path, qualified, ok := strings.Cut(key, db.ProviderAgentSkipMarker)
	if !ok {
		return key, ""
	}
	return path, db.ProviderAgentSkipMarker + qualified
}

// legacyProviderSkipCacheKey removes the agent qualifier from a provider cache
// key while retaining any hash or data-version suffix. Successful processing
// clears this predecessor alongside the scoped key so upgrades do not retain
// dead path-only cache entries.
func legacyProviderSkipCacheKey(key string) string {
	base, qualified, ok := strings.Cut(key, db.ProviderAgentSkipMarker)
	if !ok {
		return ""
	}
	_, suffix, hasSuffix := strings.Cut(qualified, "?")
	if !hasSuffix {
		return base
	}
	return base + "?" + suffix
}

func providerProcessCacheKeyWithHash(
	key string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) string {
	if key == "" {
		return ""
	}
	if fingerprint.Hash == "" || !semantics.FingerprintHashInCacheKey {
		return key
	}
	return key + "?source_hash=" + fingerprint.Hash
}

// providerSkipCacheEntryFreshInDB reports whether a skip-cache hit is
// still consistent with the archive. rowHashVerified additionally reports
// that the current fingerprint's content hash was compared against a
// stored session row and matched -- the only cache outcome strong enough
// to stamp a provider_freshness digest, since the other fresh returns
// (hash-free semantics, whole-container identity, no stored row yet)
// never consult a row.
func (e *Engine) providerSkipCacheEntryFreshInDB(ctx context.Context,
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	providerSemantics parser.ProviderSyncSemantics,
) (fresh, rowHashVerified bool) {
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	if fingerprint.Hash == "" ||
		!providerSemantics.FingerprintHashRequiredForFreshness {
		return true, false
	}
	if providerPersistentSharedContainerSource(source) {
		// A whole-container source (Omnigent chat.db, Cursor IDE state.vscdb)
		// has only virtual member rows in the archive, so its entry is fresh
		// without ever finding a row for the physical container path: the
		// cache identity already carries the container hash and parser data
		// version. This is distinct from
		// ProviderSyncSemantics.SkipCacheFreshWithoutStoredRow below, which
		// trusts an entry only while NO row exists yet and resumes stored-row
		// hash validation once the provider persists one.
		return true, false
	}
	lookupPath := providerSkipLookupPath(file, source, fingerprint)
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	if providerSemantics.SkipCacheFreshWithoutStoredRow {
		storedIDs, err := e.db.ListSessionIDsByFilePath(ctx,
			lookupPath, string(agent),
		)
		if err == nil && len(storedIDs) == 0 {
			// A cached parse failure or intentionally ignored source has no
			// persisted row or hash to compare. Retry suppression is therefore
			// mtime/source-signal based until a row exists: a same-mtime rewrite
			// cannot be distinguished in this no-row state. Hash validation applies
			// once a session has actually been stored.
			return true, false
		}
	}
	matched := e.providerFingerprintHashMatchesDB(ctx,
		agent, lookupPath, fingerprint,
		providerSemantics.FingerprintHashRequiredForFreshness,
	)
	return matched, matched
}

func processFileUsesProvider(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentGoose, parser.AgentPiebald,
		parser.AgentWarp, parser.AgentZCode, parser.AgentCrush:
		return true
	default:
		return false
	}
}

func (e *Engine) shouldSkipProviderSource(ctx context.Context,
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	if !providerSourceSupportsPersistedFreshness(agent) {
		return false
	}
	if e.forceParseRequested(file) {
		return false
	}
	lookupPath := providerSkipLookupPath(file, source, fingerprint)
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(ctx,
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	if fingerprint.Size != 0 && storedSize != fingerprint.Size {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	if !e.providerFingerprintHashMatchesDB(ctx,
		agent, lookupPath,
		fingerprint,
		semantics.FingerprintHashRequiredForFreshness,
	) {
		return false
	}
	return e.db.GetDataVersionByAgentPath(ctx,
		lookupPath, string(agent),
	) >= db.CurrentDataVersion()
}

func providerSourceSupportsPersistedFreshness(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentGoose, parser.AgentWarp, parser.AgentZCode,
		parser.AgentCrush:
		return true
	default:
		return false
	}
}

func providerSkipLookupPath(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	for _, path := range []string{
		fingerprint.Key,
		source.FingerprintKey,
		source.DisplayPath,
		source.Key,
		file.Path,
	} {
		if path != "" {
			return path
		}
	}
	return file.Path
}

func (e *Engine) shouldCacheSkip(
	file parser.DiscoveredFile,
) bool {
	if file.Agent == parser.AgentKiro {
		if filepath.Base(file.Path) == kiroSQLiteDBName {
			return false
		}
		if _, _, ok := parseKiroSQLiteVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentZed {
		if filepath.Base(file.Path) == "threads.db" {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, "threads.db"); ok {
			return false
		}
	}
	if file.Agent == parser.AgentZCode {
		if filepath.Base(file.Path) == parser.ZCodeDBName {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, parser.ZCodeDBName); ok {
			return false
		}
	}
	if file.Agent == parser.AgentGoose {
		if filepath.Base(file.Path) == parser.GooseDBName {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(
			file.Path, parser.GooseDBName,
		); ok {
			return false
		}
	}
	if file.Agent == parser.AgentShelley {
		if filepath.Base(file.Path) == shelleyDBFile {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, shelleyDBFile); ok {
			return false
		}
	}
	if file.Agent == parser.AgentTrae {
		if filepath.Base(file.Path) == "state.vscdb" {
			return false
		}
		if _, _, ok := parser.SplitTraeVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentVSCopilot {
		// Visual Studio Copilot conversations are skipped by a composite
		// fingerprint spanning every sibling trace file (see
		// processVisualStudioCopilot). The generic skip cache keys on the
		// representative file's mtime alone, so a cached entry would bypass that
		// composite check and miss a sibling-only change or removal.
		if parser.IsVisualStudioCopilotTraceFile(file.Path) {
			return false
		}
		if _, _, ok := parser.SplitVisualStudioCopilotVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentAider {
		// Aider fans one physical history file out into per-run virtual
		// sessions. A mtime-only skip can hide same-mtime content changes,
		// missing run rows, or stale per-run data versions before the
		// provider fingerprint and dropUnchangedSharedSQLiteResults hash
		// checks run, so all Aider freshness stays on that provider-aware path.
		return false
	}
	if !isOpenCodeFormatStorageAgent(file.Agent) {
		return true
	}
	if filepath.Base(file.Path) == openCodeFormatDBName(file.Agent) {
		return false
	}
	if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) {
		return false
	}
	for _, dir := range e.sources().agentDirs[file.Agent] {
		if dir == "" {
			continue
		}
		src := resolveOpenCodeFormatSource(file.Agent, dir)
		if src.Mode != parser.OpenCodeSourceStorage {
			continue
		}
		if rel, ok := isUnder(dir, file.Path); ok {
			rel = filepath.ToSlash(rel)
			sessionPrefix := "storage/" +
				filepath.Base(src.SessionRoot) + "/"
			return !strings.HasPrefix(rel, sessionPrefix)
		}
	}
	return true
}

const sourceHashSkipMarker = "?source_hash="

// normalizeSourceHashSkipCache performs the one archive-sized pass needed to
// repair legacy duplicate source-hash entries and build the watcher-time
// sibling index. A family with multiple hashed keys is ambiguous: same-mtime
// rewrites are why the hash exists, so no stored key can safely be called
// current. Drop that family so the source reparses once and establishes a
// trustworthy key.
func normalizeSourceHashSkipCache(
	cache map[string]int64, fingerprints map[string]string,
) (map[string]string, map[string]struct{}) {
	index := make(map[string]string)
	counts := make(map[string]int)
	ambiguous := make(map[string]struct{})
	for path := range cache {
		base, _, hashed := strings.Cut(path, sourceHashSkipMarker)
		if !hashed {
			continue
		}
		counts[base]++
		if counts[base] == 1 {
			index[base] = path
		}
	}
	for path := range cache {
		base, _, hashed := strings.Cut(path, sourceHashSkipMarker)
		if hashed && counts[base] > 1 {
			delete(cache, path)
			ambiguous[base] = struct{}{}
			if fingerprints != nil {
				delete(fingerprints, path)
			}
		}
	}
	for base := range index {
		delete(cache, base)
		if fingerprints != nil {
			delete(fingerprints, base)
		}
		if counts[base] > 1 {
			delete(index, base)
		}
	}
	return index, ambiguous
}

// cacheSkip records a file so it won't be retried until its mtime changes.
// The returned work count measures sibling-index probes and is used by the
// cardinality regression to keep watcher-time work independent of cache size.
func (e *Engine) cacheSkip(
	path string, mtime int64, sourceFingerprint ...string,
) int {
	e.skipMu.Lock()
	old, exists := e.skipCache[path]
	previousSize, previouslyDirty := len(e.skipCache), e.skipCacheDirty
	work := e.removeSkipHashSiblingsLocked(path)
	if !exists || old != mtime {
		e.skipCacheDirty = true
	}
	e.skipCache[path] = mtime
	if exists && old == mtime && len(e.skipCache) == previousSize {
		e.skipCacheDirty = previouslyDirty
	}
	if base, _, hashed := strings.Cut(path, sourceHashSkipMarker); hashed {
		e.skipHashKeys[base] = path
	}
	fingerprint := ""
	if len(sourceFingerprint) > 0 {
		fingerprint = sourceFingerprint[0]
	}
	if fingerprint != "" {
		if e.skipFingerprints == nil {
			e.skipFingerprints = make(map[string]string)
		}
		e.skipFingerprints[path] = fingerprint
	} else if e.skipFingerprints != nil {
		delete(e.skipFingerprints, path)
	}
	e.skipMu.Unlock()
	return work
}

// clearSkip removes a skip-cache entry when a file produces a valid session.
// Its work count has the same cardinality-regression role as cacheSkip's.
func (e *Engine) clearSkip(ctx context.Context, path string) int {
	work, err := e.clearSkipPersistent(ctx, path)
	if err != nil {
		log.Printf("clearing persisted skip cache for %s: %v", path, err)
	}
	return work
}

func (e *Engine) clearSkipInMemory(path string) int {
	base, _, _ := strings.Cut(path, sourceHashSkipMarker)
	e.failures.Clear(base)
	if _, suffix := SplitProviderSkipCachePath(base); suffix == "" {
		for agent := range e.sources().providerFactories {
			e.failures.Clear(providerAgentSkipCacheKey(base, agent))
		}
	}
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	failurePath, _ := SplitProviderSkipCachePath(base)
	delete(e.piebaldFailureMemo, failurePath)
	before := len(e.skipCache)
	work := e.removeSkipHashSiblingsLocked(path)
	delete(e.skipCache, path)
	delete(e.skipFingerprints, path)
	legacyPath := legacyProviderSkipCacheKey(path)
	if legacyPath != "" {
		work += e.removeSkipHashSiblingsLocked(legacyPath)
		delete(e.skipCache, legacyPath)
		delete(e.skipFingerprints, legacyPath)
	}
	e.skipCacheDirty = e.skipCacheDirty || len(e.skipCache) != before
	return work
}

func (e *Engine) clearSkipPersistent(ctx context.Context, path string) (int, error) {
	work := e.clearSkipInMemory(path)
	legacyPath := legacyProviderSkipCacheKey(path)
	if e.ephemeral {
		return work, nil
	}
	base, _, _ := strings.Cut(path, sourceHashSkipMarker)
	err := e.db.DeleteSkippedFileAndPrefix(ctx,
		base, base+sourceHashSkipMarker,
	)
	if legacyPath != "" {
		legacyBase, _, _ := strings.Cut(legacyPath, sourceHashSkipMarker)
		err = errors.Join(err, e.db.DeleteSkippedFileAndPrefix(ctx,
			legacyBase, legacyBase+sourceHashSkipMarker,
		))
	}
	return work, err
}

func (e *Engine) removeSkipHashSiblingsLocked(path string) int {
	before := len(e.skipCache)
	defer func() { e.skipCacheDirty = e.skipCacheDirty || len(e.skipCache) != before }()
	if e.skipHashKeys == nil {
		e.skipHashKeys, _ = normalizeSourceHashSkipCache(
			e.skipCache, e.skipFingerprints,
		)
	}
	base, _, hasHash := strings.Cut(path, sourceHashSkipMarker)
	if !hasHash {
		if sibling, ok := e.skipHashKeys[path]; ok {
			delete(e.skipCache, sibling)
			delete(e.skipFingerprints, sibling)
			delete(e.skipHashKeys, path)
		}
		return 1
	}
	delete(e.skipCache, base)
	delete(e.skipFingerprints, base)
	if sibling, ok := e.skipHashKeys[base]; ok {
		delete(e.skipCache, sibling)
		delete(e.skipFingerprints, sibling)
		delete(e.skipHashKeys, base)
	}
	return 2
}

// clearWatcherOverflowCaches invalidates every freshness shortcut whose
// correctness can depend on receiving a concrete changed path. The following
// forced discovery pass rebuilds these caches from parsed source state.
func (e *Engine) clearWatcherOverflowCaches(ctx context.Context) {
	e.skipMu.Lock()
	e.skipCache = make(map[string]int64)
	e.skipCacheDirty = true
	e.skipFingerprints = make(map[string]string)
	e.skipHashKeys = make(map[string]string)
	e.skipMu.Unlock()
	e.ResetFailureCache(ctx)
	if !e.ephemeral {
		if err := e.db.ReplaceSkippedFiles(ctx, map[string]int64{}); err != nil {
			log.Printf("clearing skipped files after watcher overflow: %v", err)
		}
	}
	e.clearTrustedOpenCodeStorageSessions()
	e.clearTrustedSQLiteContainers()
	e.clearVerifiedSources()
	parser.EvictAllCodexSessionIndexes()
}

// InjectSkipCache merges entries into the in-memory skip
// cache. Used by remote sync to pre-populate with
// translated paths.
func (e *Engine) InjectSkipCache(entries map[string]int64) {
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	if e.skipHashKeys == nil {
		e.skipHashKeys, _ = normalizeSourceHashSkipCache(
			e.skipCache, e.skipFingerprints,
		)
	}
	incoming := make(map[string]int64, len(entries))
	maps.Copy(incoming, entries)
	_, ambiguous := normalizeSourceHashSkipCache(incoming, nil)
	for base := range ambiguous {
		e.removeSkipHashSiblingsLocked(base + sourceHashSkipMarker)
	}
	for path, mtime := range incoming {
		e.removeSkipHashSiblingsLocked(path)
		if old, exists := e.skipCache[path]; !exists || old != mtime {
			e.skipCacheDirty = true
		}
		e.skipCache[path] = mtime
		if base, _, hashed := strings.Cut(path, sourceHashSkipMarker); hashed {
			e.skipHashKeys[base] = path
		}
	}
}

// SnapshotSkipCache returns a copy of the in-memory skip
// cache.
func (e *Engine) SnapshotSkipCache() map[string]int64 {
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	out := make(map[string]int64, len(e.skipCache))
	maps.Copy(out, e.skipCache)
	return out
}

// SnapshotRetrySafeSkipCache returns cache state that can survive a failed
// replacement-database rebuild. Entries for sources that changed exclusion or
// source-missing state are omitted because those changes exist only in the
// replacement database that will be discarded.
func (e *Engine) SnapshotRetrySafeSkipCache() map[string]int64 {
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	out := make(map[string]int64, len(e.skipCache))
	for key, mtime := range e.skipCache {
		path, _ := SplitProviderSkipCachePath(key)
		if _, unsafe := e.retryUnsafeSkipPaths[path]; unsafe {
			continue
		}
		out[key] = mtime
	}
	return out
}

func (e *Engine) markRetryUnsafeSkipSource(path string) {
	if path == "" {
		return
	}
	e.skipMu.Lock()
	if e.retryUnsafeSkipPaths == nil {
		e.retryUnsafeSkipPaths = make(map[string]struct{})
	}
	e.retryUnsafeSkipPaths[path] = struct{}{}
	e.skipMu.Unlock()
}

// persistSkipCache writes the in-memory skip cache to the
// database so skipped files survive process restarts.
// Returns the number of entries persisted.
func (e *Engine) persistSkipCache(ctx context.Context) int {
	return e.persistSkipCacheInto(ctx, e.db)
}

// ResetFailureCache forgets every cached source failure and makes that durable.
// A resync retries sources that failed under the previous parser; the daemon
// also calls this before the incremental catch-up after a worker rebuild is
// discarded, because that reset lived only in the worker process.
func (e *Engine) ResetFailureCache(ctx context.Context) {
	e.clearPiebaldFailureMemo()
	e.failures.Reset()
	e.flushFailureCache(ctx)
}

// flushFailureCache makes recorded source failures durable. Passes that end
// incomplete call it directly: a failed pass is exactly when new failures were
// recorded, and it must not promote that pass's ordinary skip entries.
func (e *Engine) flushFailureCache(ctx context.Context) {
	e.flushFailureCacheInto(ctx, e.db)
}

func (e *Engine) flushFailureCacheInto(ctx context.Context, target *db.DB) {
	// An unwritable target is not an error: the cache stays in memory and the
	// next flush to a writable archive carries it.
	if e.ephemeral || target.ReadOnly() || target.WriterClosed() {
		return
	}
	if err := e.failures.Flush(ctx, target); err != nil {
		log.Printf("%v", err)
	}
}

// persistSkipCacheInto writes the current skip cache into target. A resync build
// persists into the replacement database (not the active one) so the post-swap
// engine reloads warm skip state.
func (e *Engine) persistSkipCacheInto(ctx context.Context, target *db.DB) int {
	if e.ephemeral {
		return 0
	}
	e.flushFailureCacheInto(ctx, target)
	e.skipMu.Lock()
	if target == e.db && !e.skipCacheDirty {
		count := len(e.skipCache)
		e.skipMu.Unlock()
		return count
	}
	snapshot := make(map[string]int64, len(e.skipCache))
	maps.Copy(snapshot, e.skipCache)
	if target == e.db {
		e.skipCacheDirty = false
	}
	e.skipMu.Unlock()

	if err := target.ReplaceSkippedFiles(ctx, snapshot); err != nil {
		e.skipMu.Lock()
		e.skipCacheDirty = true
		e.skipMu.Unlock()
		log.Printf("persisting skip cache: %v", err)
	}
	return len(snapshot)
}
