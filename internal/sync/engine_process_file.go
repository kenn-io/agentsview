package sync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync/failurecache"
)

// sessionParseError is a per-session parse failure inside a shared
// SQLite store (OpenCode, Zed, Kiro), where one file path fans out to
// many sessions and a single bad payload must not fail the whole db.
type sessionParseError struct {
	sessionID   string // raw parser-side ID, no engine prefix
	virtualPath string // dbPath#rawID source path
	err         error
}

// sourceMissingMember identifies one stored session whose virtual member
// source vanished from a still-existing shared container. The write seam marks
// its exact source ownership missing instead of hard-deleting it as a parser
// exclusion. The archived session remains visible.
type sourceMissingMember struct {
	sessionID string
	filePath  string
	machine   string
	agent     parser.AgentType
}

type processResult struct {
	// sourceBytes is the physical source size used to acquire the retention
	// lease and to account this result against the pending write batch byte
	// cap. Zero on lease-free skips.
	sourceBytes               int64
	sqliteContainerResultPath string
	results                   []parser.ParseResult
	excludedSessionIDs        []string
	// preservedSessionIDs are higher-ranked members omitted by a shared source;
	// they remain present while lower-ranked source ownership is reconciled.
	preservedSessionIDs []string
	// sourceMissingMembers carries stored sessions whose virtual member
	// source no longer exists inside a still-present shared container
	// (e.g. a Windsurf conversation deleted from state.vscdb). They must
	// be marked source-missing, never routed through DeleteParserExcludedSessions.
	sourceMissingMembers []sourceMissingMember
	// sessionErrs carries per-session parse failures from the
	// shared-db fan-out loops. Normal sync logs and skips these;
	// parse-diff (forceParse) surfaces them as DiffParseError report
	// entries so --fail-on-change cannot pass over a session the
	// current binary failed to parse.
	sessionErrs      []sessionParseError
	skip             bool
	mtime            int64
	err              error
	sourceCwdChanged bool
	incremental      *incrementalUpdate
	// providerStatHash stages the per-component freshness digest that
	// applyProviderFilePathPolicies computed but did not yet write. The
	// collector persists it after the matching session row commits
	// successfully, so a downstream write failure (or a CWD-filter veto)
	// never marks an absent or stale session as fresh. nil when the
	// source is not a multi-file hasher agent or its parse produced no
	// kept results.
	providerStatHash *pendingProviderStatHash
	cacheSkip        bool
	cacheFailure     bool
	cachedFailure    bool
	failureCacheKey  string
	failureIdentity  failurecache.Identity
	// claudeRowlessFreshnessKey is staged by a successful complete Claude
	// parse. The collector promotes it only when the parse leaves no admitted
	// session row and a CWD-rejected stale fork still needs filter-scoped
	// freshness, after source-missing reconciliation and CWD filtering succeed.
	claudeRowlessFreshnessKey string
	// cacheAfterWrite records a successful, complete rowless container parse
	// after its member writes commit. Unlike ordinary provider results, these
	// containers have no physical-path session row that can make the next full
	// sync fresh through the archive database.
	cacheAfterWrite bool
	// sourceFingerprint carries S3 object fingerprints into
	// skip-cache writes so same-mtime object rewrites do not stay
	// hidden behind a cached parse failure or non-interactive result.
	sourceFingerprint string
	// noCacheSkip suppresses skip-cache recording even when cacheSkip is set
	// for the agent. Read/scan failures and incomplete append boundaries are
	// transient: a readability fix or completed record may retain the same file
	// mtime, so caching either result would silently skip later work instead of
	// retrying it.
	noCacheSkip bool
	needsRetry  bool
	// forceReplace requests full message replacement on write,
	// even when the existing rows would otherwise be left in
	// place. Set when a fall-through to full parse is recovering
	// from stale stored rows, such as an atomic file replacement
	// or cross-sync streaming split. In those cases the parsed
	// messages can reuse existing ordinals, so the default
	// append-only writeMessages would silently drop the rewrite.
	forceReplace bool
	cacheKey     string
	// retrySessionIDs carries provider per-result data-version state.
	// Legacy parsers use needsRetry as a source-wide fallback.
	retrySessionIDs map[string]bool
	deferredCount   int
	// retryCauses names each result held for retry and why, for logs and
	// the pass summary.
	retryCauses []retryCause
	// suppressPresenceSweep marks a source result that must not authorize
	// presence or tombstone reconciliation, including clean unsupported skips.
	suppressPresenceSweep bool
	// providerFailureCount carries non-fatal partial outcome failures through
	// valid partial writes. Reconciliation may persist the valid results, but
	// must not acknowledge the pass as complete.
	providerFailureCount int
	// providerWideFailureCount is the subset of provider failures that applies
	// to every result in the source. Per-result retry state stays in
	// retrySessionIDs so one member cannot demote otherwise valid siblings.
	providerWideFailureCount int
	// cachedSkip distinguishes a direct mtime skip-cache hit from other skip
	// decisions such as DB freshness or container trust. Remote journal replay
	// uses only this signal for error-suppression telemetry.
	cachedSkip bool
	// storageTrustPath/State/Snap carry an OpenCode-family storage
	// session's pre-parse stat signature and invalidation snapshot to
	// the write path, which promotes it once the session's batch is
	// confirmed fully written (see opencode_storage_gate.go). Empty for
	// everything else.
	storageTrustPath  string
	storageTrustState string
	storageTrustSnap  storageTrustSnapshot
	// retentionLease bounds the memory retained by this result's parsed data.
	// It is acquired at the parse seams (provider parse, incremental parse,
	// legacy/S3 parse) and only ever set on a result carrying parsed data; skip
	// results carry none. The worker loop copies it onto syncJob.retentionLease.
	// Archive-scale collectors release it when transferring the result into
	// their separately bounded pending batch; other collectors hold it through
	// the database write. Every path releases it exactly once.
	retentionLease *parseRetentionLease
	// sourceCwdResolution carries parser-owned source authority to the generic
	// write seam. It is deliberately independent of transcript fingerprints.
	sourceCwdResolution parser.SourceCwdResolution
	sourceCwdStored     string
	sourceCwdStoredOK   bool
	sourceCwdPath       string
	sourceCwdAgent      parser.AgentType
	// staged carries the scratch staging sink for a Codex full parse that
	// took the streaming path. The collector moves it onto the pending
	// write (and into pendingStaged for release after the batch commits);
	// every path that drops the result without writing must releaseStaged.
	staged *codexStagingSink
	// stagedGCRelease restores the process GC percent after this result's
	// staged sink is released. nil when the result carries no sink.
	stagedGCRelease func()
}

// releaseStaged closes the result's staging sink, restores the GC percent
// window it opened, and clears the handles. It must be called exactly once
// on every processResult that carries a sink and is not moving the sink
// onto a pending write.
func (r *processResult) releaseStaged() {
	if r.staged == nil && r.stagedGCRelease == nil {
		return
	}
	if r.staged != nil {
		if err := r.staged.Close(); err != nil {
			log.Printf("closing codex staging sink: %v", err)
		}
		r.staged = nil
	}
	if r.stagedGCRelease != nil {
		r.stagedGCRelease()
		r.stagedGCRelease = nil
	}
}

// savedHistory returns the stored row for id only when it holds messages or
// usage events. Placeholder rows, such as a Recall import or a session header
// written before its content, have nothing for a fallback parse to overwrite.
func savedHistory(ctx context.Context, store db.Store, id string) (*db.Session, error) {
	stored, err := store.GetSessionFull(ctx, id)
	if err != nil || stored == nil {
		return nil, err
	}
	msgs, err := store.GetMessages(ctx, id, 0, 1, true)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		return stored, nil
	}
	reader, ok := store.(interface {
		GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
	})
	if !ok {
		// Without a way to rule out saved usage, keep the row.
		return stored, nil
	}
	events, err := reader.GetUsageEvents(ctx, id)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	return stored, nil
}

func (r *processResult) needsRetryForSession(sessionID string) bool {
	if r.retrySessionIDs != nil {
		return r.retrySessionIDs[sessionID]
	}
	return r.needsRetry
}

func (r *processResult) suppressesPresenceSweepForRetry() bool {
	return r.retrySessionIDs == nil && r.needsRetry
}

func (r *processResult) sourceProofWithheld(hardFailure bool) bool {
	return hardFailure || r.cachedFailure || r.providerFailureCount > 0 || r.deferredCount > 0 ||
		len(r.retrySessionIDs) > 0 || r.needsRetry
}

func (e *Engine) processFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) processResult {
	if e.sourceSyncErr != nil {
		return processResult{err: e.sourceSyncErr}
	}
	if res, ok := e.processProviderFile(ctx, file); ok {
		return res
	}

	// Every registered agent is provider-authoritative, so processProviderFile
	// owns all local-file processing. The only sources that fall through are
	// s3:// objects for providers that declare S3Discovery, which bypass the
	// provider (its source sets read local files) and use the dedicated S3
	// sync path. Anything else is an unrecognized agent type.
	if !strings.HasPrefix(file.Path, "s3://") {
		return processResult{
			err: fmt.Errorf("unknown agent type: %s", file.Agent),
		}
	}

	s3Provider, ok := s3ProviderFor(file.Agent)
	if !ok {
		return processResult{
			err: fmt.Errorf("unsupported s3 agent type: %s", file.Agent),
		}
	}

	if file.SourceMtime == 0 {
		obj, err := statS3SourceObjectWithProvider(file, s3Provider)
		if err != nil {
			return processResult{
				err: fmt.Errorf("stat %s: %w", file.Path, err),
			}
		}
		file.SourceSize = obj.Size
		file.SourceMtime = obj.LastModified.UnixNano()
		file.SourceFingerprint = obj.Fingerprint
	}
	info, err := s3SourceFileInfo(file)
	if err != nil {
		return processResult{
			err: fmt.Errorf("stat %s: %w", file.Path, err),
		}
	}

	// Capture mtime once from the initial stat so all downstream cache
	// operations use a consistent value.
	mtime := info.ModTime().UnixNano()
	cacheSkip := e.shouldCacheSkip(file)
	sourceFingerprint := s3SourceFingerprint(file)

	// Skip files cached from a previous sync whose mtime and source
	// fingerprint are unchanged.
	if cacheSkip && !e.forceParseBypassesCache(file) {
		if e.shouldUseCachedSkip(file, mtime, sourceFingerprint) {
			if e.pathNeedsCachedSkipBypass(ctx, file.Agent, file.Path) {
				e.clearSkip(ctx, file.Path)
			} else {
				return processResult{
					skip:       true,
					mtime:      mtime,
					cacheSkip:  true,
					cachedSkip: true,
				}
			}
		}
	}

	res := e.processS3Session(ctx, file, info, s3Provider)
	res.cacheSkip = cacheSkip
	res.mtime = mtime
	res.sourceFingerprint = sourceFingerprint
	return res
}

func (e *Engine) shouldUseCachedSkip(
	file parser.DiscoveredFile, mtime int64, sourceFingerprint string,
) bool {
	e.skipMu.RLock()
	cachedMtime, cached := e.skipCache[file.Path]
	cachedFingerprint := ""
	if e.skipFingerprints != nil {
		cachedFingerprint = e.skipFingerprints[file.Path]
	}
	e.skipMu.RUnlock()
	if !cached || cachedMtime != mtime {
		return false
	}
	if isS3SourcePath(file.Path) && sourceFingerprint != "" {
		return cachedFingerprint == sourceFingerprint
	}
	return true
}

func (e *Engine) pathNeedsProjectReparse(ctx context.Context,
	agent parser.AgentType,
	path string,
) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	project, ok := e.db.GetProjectByAgentPath(ctx, lookupPath, string(agent))
	return ok && parser.NeedsProjectReparse(project)
}

func (e *Engine) pathNeedsCachedSkipBypass(ctx context.Context,
	agent parser.AgentType,
	path string,
) bool {
	return e.pathNeedsProjectReparse(ctx, agent, path) ||
		e.pathNeedsDataVersionReparse(ctx, agent, path)
}

func (e *Engine) pathNeedsDataVersionReparse(ctx context.Context,
	agent parser.AgentType,
	path string,
) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByAgentPath(ctx,
		lookupPath, string(agent),
	); !ok {
		return false
	}
	return e.db.GetDataVersionByAgentPath(ctx,
		lookupPath, string(agent),
	) < db.CurrentDataVersion()
}

// staleDataVersionIdentitySet loads every source identity that
// pathNeedsDataVersionReparse would report in one scan, so the mtime-cutoff
// filter does not issue two point queries per discovered file. A load failure
// degrades to no cutoff bypass, matching the per-path form's row-missing
// result.
func (e *Engine) staleDataVersionIdentitySet(ctx context.Context) map[sourceCwdPathKey]struct{} {
	if e == nil || e.db == nil {
		return nil
	}
	identities, err := e.db.StaleDataVersionAgentPaths(ctx, db.CurrentDataVersion())
	if err != nil {
		log.Printf("list stale data-version sources: %v", err)
		return nil
	}
	set := make(map[sourceCwdPathKey]struct{}, len(identities))
	for _, identity := range identities {
		set[sourceCwdPathKey{
			path: identity.FilePath, agent: identity.Agent,
		}] = struct{}{}
	}
	return set
}

// staleSourceReparseAdmitted predicts whether the cwd filter would admit a
// reparse of a stale, cwd-participating source. A vetoed reparse cannot
// rewrite the row, so bypassing the cutoff for it would recur every pass
// without ever resolving the staleness; the row stays stale instead, and the
// bypass re-arms as soon as the filter or the source resolution admits the
// source. Filter admission for participating sources depends only on the
// source resolution and stored Cwd, so the prediction is exact; when the
// resolution disagrees with the stored Cwd the forceParse branch has already
// bypassed the cutoff before this gate runs.
func (e *Engine) staleSourceReparseAdmitted(
	participating bool, decision sourceCwdDecision,
) bool {
	if e.cwdFilter.empty() || !participating {
		return true
	}
	return e.cwdFilter.allows(sourceCwdForFilter("", decision))
}

func (e *Engine) pathInStaleIdentitySet(
	set map[sourceCwdPathKey]struct{},
	agent parser.AgentType,
	path string,
) bool {
	if len(set) == 0 {
		return false
	}
	if e.pathRewriter != nil {
		path = e.pathRewriter(path)
	}
	_, stale := set[sourceCwdPathKey{path: path, agent: string(agent)}]
	return stale
}

// sourceFailureCacheIdentity uses the provider's pre-parse fingerprint. Virtual
// SQLite sources also include container state: repairing a schema need not
// change a session's own timestamp.
func (e *Engine) sourceFailureCacheIdentity(
	file parser.DiscoveredFile, fingerprint parser.SourceFingerprint,
) (failurecache.Identity, bool) {
	id := failurecache.Identity{
		MTimeNS: fingerprint.MTimeNS, Size: fingerprint.Size, Fingerprint: fingerprint.Hash,
	}
	if file.Path == "" || file.Agent == "" || strings.HasPrefix(file.Path, "s3://") {
		return id, false
	}
	if physical, _, virtual := parser.ParseVirtualSourcePath(file.Path); virtual {
		state, ok := parser.StatSQLiteContainerState(physical)
		if !ok {
			return id, false
		}
		id.Fingerprint += fmt.Sprint(state)
		return id, true
	}
	if !e.shouldCacheSkip(file) && fingerprint.Hash == "" {
		return id, false
	}
	factory := e.sources().providerFactories[file.Agent]
	if factory == nil {
		return id, false
	}
	caps := factory.Capabilities()
	if (caps.Source.CompositeFingerprint == parser.CapabilitySupported ||
		caps.Source.MultiFileStatHash == parser.CapabilitySupported) && fingerprint.Hash == "" &&
		!isCodexFormatAgent(file.Agent) {
		return id, false
	}
	return id, true
}

func (e *Engine) sourcePathMissing(file parser.DiscoveredFile) bool {
	if file.Path == "" || strings.HasPrefix(file.Path, "s3://") {
		return false
	}
	if _, _, virtual := parser.ParseVirtualSourcePath(file.Path); virtual {
		return false
	}
	_, err := e.lstatSource(file.Path)
	return os.IsNotExist(err)
}

func sourceParseFailureCacheable(err error, paths ...string) bool {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	// Classify structured causes before inspecting wrappers, which can include
	// companion paths as well as the primary source path.
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*json.SemanticError](err); ok {
		return true
	}
	message := err.Error()
	// Parser wrappers include source paths. Directory names must not classify
	// an otherwise transient error as malformed (or vice versa).
	for _, path := range paths {
		if path != "" {
			message = strings.ReplaceAll(message, path, "")
		}
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"permission", "access is denied", "temporary", "temporarily",
		"timed out", "timeout", "database is locked", "database is busy",
		"resource temporarily unavailable", "try again", "read ", "reading ",
		"scan ", "scanning ", "open ", "stat ", "i/o", "input/output",
		"unexpected eof", "truncated", "incomplete",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{
		"malformed", "invalid", "syntax", "unexpected token",
		"no such column", "no such table",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func markSourceFailure(
	file parser.DiscoveredFile, result processResult, id failurecache.Identity, cacheable bool,
) processResult {
	if cacheable && result.err != nil && !errors.Is(result.err, context.Canceled) &&
		!errors.Is(result.err, context.DeadlineExceeded) {
		result.cacheFailure = true
		result.failureCacheKey = providerAgentSkipCacheKey(file.Path, file.Agent)
		result.failureIdentity = id
	}
	return result
}

func (e *Engine) cachedSourceFailure(
	ctx context.Context, file parser.DiscoveredFile, id failurecache.Identity,
) (processResult, bool) {
	key := providerAgentSkipCacheKey(file.Path, file.Agent)
	if !e.failures.Check(key, id) {
		return processResult{}, false
	}
	if e.forceParseBypassesCache(file) || e.pathNeedsCachedSkipBypass(ctx, file.Agent, file.Path) {
		e.failures.Clear(key)
		return processResult{}, false
	}
	return processResult{
		skip: true, cachedFailure: true, suppressPresenceSweep: true,
	}, true
}

func (e *Engine) processProviderFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) (result processResult, used bool) {
	var cwdDecision sourceCwdDecision
	var cwdPath string
	var cwdAgent parser.AgentType
	var sqliteContainerResultPath string
	defer func() {
		result.sqliteContainerResultPath = sqliteContainerResultPath
		if cwdDecision.resolution.State == parser.SourceCwdUnspecified {
			return
		}
		result.sourceCwdResolution = cwdDecision.resolution
		result.sourceCwdStored = cwdDecision.storedCwd
		result.sourceCwdStoredOK = cwdDecision.storedOK
		result.sourceCwdPath = cwdPath
		result.sourceCwdAgent = cwdAgent
	}()
	mode := e.providerMigrationModes[file.Agent]
	usesProvider := processFileUsesProvider(file.Agent)
	if mode != parser.ProviderMigrationProviderAuthoritative && !usesProvider {
		return processResult{}, false
	}
	// S3 sources are not provider-owned: the provider source sets read local
	// files, so s3:// paths use the dedicated S3 sync path (processS3Session),
	// which handles object fetch, fingerprinting, and per-agent skip logic.
	if strings.HasPrefix(file.Path, "s3://") {
		return processResult{}, false
	}
	failureKey := providerAgentSkipCacheKey(file.Path, file.Agent)
	if id, exists := e.failures.Lookup(failureKey); exists {
		if e.forceParseBypassesCache(file) {
			if file.Agent == parser.AgentPiebald && !e.forceParse {
				// Keep retry intent even if source lookup fails before the Piebald
				// memo is consulted; the invalidated durable entry must stay gone.
				e.setPiebaldRetry(file.Path, piebaldFailureIdentity{})
			}
			e.failures.Clear(failureKey)
		} else if id.Missing {
			if e.sourcePathMissing(file) {
				if cached, ok := e.cachedSourceFailure(ctx, file, id); ok {
					return cached, true
				}
			} else {
				e.failures.Clear(failureKey)
			}
		}
	}
	if file.ProviderSource != nil && !file.ProviderProcess && !usesProvider {
		return processResult{}, false
	}
	e.discardStaleSQLiteProviderSource(&file)
	if file.ProviderSource != nil {
		sqliteContainerResultPath = providerDiscoveredPath(*file.ProviderSource)
	}

	// OpenCode-family shared-SQLite gate: when the whole container
	// provably has not changed since the last fully verified pass, none
	// of its sessions can have changed, so skip before paying for the
	// per-session fingerprint (a DB open per source) and parse.
	if e.sqliteContainerSourceFresh(ctx, file) {
		return processResult{skip: true}, true
	}

	// OpenCode-family file-backed storage gate: when the session's
	// per-file stat signature matches the last verified pass, its parse
	// inputs are unchanged, so skip before re-reading the whole message
	// and part tree (see opencode_storage_gate.go). The captured state
	// also feeds the post-parse promotion below.
	storageState, storageSnap, storageStateOK := e.openCodeStorageSessionGateState(file)
	if storageStateOK &&
		e.openCodeStorageSessionFresh(file.Path, storageState) {
		return processResult{skip: true}, true
	}

	factory, ok := e.sources().providerFactories[file.Agent]
	if !ok {
		return processResult{
			err: fmt.Errorf("provider not found for agent type: %s", file.Agent),
		}, true
	}
	machine := e.machineForFile(file)
	providerConfig := parser.ProviderConfig{
		Roots:                 e.sources().agentDirs[file.Agent],
		Machine:               e.machine,
		StableSourceSnapshots: e.stableSourceSnapshots,
		SourceMachines:        e.sources().sourceMachines[file.Agent],
		PathRewriter:          e.pathRewriter,
	}
	provider := factory.NewProvider(providerConfig)

	// Re-apply the pass-failure check at the moment carried metadata is
	// about to be trusted: another worker can fail the container after this
	// file's capture recheck above, and the locked read keeps that mark
	// from racing the fingerprint skip. A write landing after both checks
	// is caught by finalization revalidation, which blocks promotion and
	// clears verification so the next pass reconciles it.
	e.discardFailedSQLiteProviderSource(&file)
	source, found, err := e.providerSourceForDiscoveredFile(ctx, provider, file)
	if err != nil {
		return markSourceFailure(
			file, processResult{err: err}, failurecache.Identity{Missing: true}, e.sourcePathMissing(file),
		), true
	}
	if !found {
		// A forced parse on a deleted shared SQLite database (Zed, ZCode, Shelley)
		// resolves to no source because the physical file is gone. Mirror the
		// legacy deleted-source handling: complete the source as an empty
		// force-replace so the engine retires every session that lived in the
		// removed database instead of failing the sync.
		if (file.ForceParse || file.ForceFullParse) &&
			providerDeletedPhysicalSQLiteSource(file.Agent, file.Path) {
			return processResult{forceReplace: true}, true
		}
		return markSourceFailure(file, processResult{
			err: fmt.Errorf(
				"%s provider source not found for %s",
				file.Agent,
				file.Path,
			),
		}, failurecache.Identity{Missing: true}, e.sourcePathMissing(file)), true
	}
	if source.ConfiguredRoot != "" {
		if sourceMachine, ok := e.configuredMachineForPath(
			file.Agent, source.ConfiguredRoot,
		); ok {
			machine = sourceMachine
		}
	} else if file.Machine == "" {
		machine = e.machineForProviderSource(file.Agent, source, file.Path)
	}
	file.Machine = machine
	providerSemantics := provider.Capabilities().Sync

	// SyncSingleSession resolves a single session by ID and carries the
	// caller-preferred project (typically the DB-preserved value, so a
	// user override is not reverted) on file.Project without an explicit
	// ProviderSource. Provider FindSource re-derives ProjectHint from the
	// path, so honor the caller's project as the hint in that case. Full
	// discovery and changed-path classification always supply
	// file.ProviderSource, whose ProjectHint stays authoritative.
	if file.ProviderSource == nil && file.Project != "" {
		source.ProjectHint = file.Project
	}
	file.ProviderSource = &source
	sqliteContainerResultPath = providerDiscoveredPath(source)
	cwdDecision = e.sourceCwdDecision(ctx, source)
	cwdPath = e.sourceCwdLookupPath(source)
	cwdAgent = source.Provider
	if cwdAgent == "" {
		cwdAgent = file.Agent
	}
	explicitForce := e.forceParseBypassesCache(file) || file.ForceFullParse
	forceSourceCwdParse := cwdDecision.forceParse
	var piebaldFailure piebaldFailureLookup
	if file.Agent == parser.AgentPiebald {
		var found bool
		piebaldFailure, found = e.preparePiebaldFailure(ctx, source, explicitForce || forceSourceCwdParse)
		if found {
			if piebaldFailure.err != nil {
				return processResult{
					skip: true, cachedFailure: true, suppressPresenceSweep: true,
				}, true
			}
			if piebaldFailure.retry {
				file.ForceParse = true
				forceSourceCwdParse = true
			}
		}
	}

	// Codex checkpoint decision: an invalid proof requires an authoritative
	// replacement. A missing checkpoint is merely absent optimization state:
	// unchanged upgraded archives keep the existing stat/content freshness
	// gates and earn a checkpoint lazily on their next real source change.
	var fingerprint parser.SourceFingerprint
	var codexCheckpoint *db.ParserCheckpoint
	var codexSeed []byte
	var codexFullHash string
	var codexHashState []byte
	codexForceFullParse := false
	codexProvenUnchanged := false
	codexAuditDeepVerify := e.checkpointAudit.Load() &&
		isCodexFormatAgent(file.Agent)
	var codexUnchangedMtime int64
	verifiedCapture, verifiedMtime, verifiedFresh, verifiedStateOK := e.verifiedProviderSourceState(provider, source, file)
	if !forceSourceCwdParse && !codexForceFullParse &&
		!codexProvenUnchanged && !codexAuditDeepVerify &&
		verifiedStateOK && verifiedFresh {
		if e.verifiedProviderSourceFreshInDB(ctx,
			verifiedCapture.key.agent, source,
			verifiedCapture.signature.size, verifiedMtime,
		) {
			return processResult{
				skip:  true,
				mtime: verifiedMtime,
			}, true
		}
		e.invalidateVerifiedSource(
			verifiedCapture.key.agent, verifiedCapture.key.path,
		)
		// The stat snapshot was fresh, but the persisted projection was not.
		// Treat the source as unverified for the remaining gates in this call:
		// if the row disappeared, the Codex cold-parse path can derive its
		// fingerprint from the parse instead of paying for a redundant read.
		verifiedFresh = false
	}

	// Capture the per-component stat digest from the same pre-parse
	// snapshot that gates freshness, BEFORE fingerprinting or parsing
	// reads the source. The in-memory verified-source gate runs first so
	// a warm engine does not pay for a second stat snapshot it will never
	// consult. Persisting a digest computed after the parse would race a
	// concurrent companion rewrite: the stored digest would describe the
	// new file state while the session row holds the old parse payload,
	// so every later warm sync would short-circuit on the new digest and
	// the stale row would never be refreshed. This single snapshot feeds
	// the warm-match comparison in providerSourceFreshBeforeFingerprint,
	// the confirmed-unchanged-skip stamp, and the write-path staging, so
	// all three always agree.
	//
	// providerStatDigestEligible withholds the capture for
	// content-authority providers under a pathRewriter: materialized
	// remote stats cannot prove a re-download unchanged, so their remote
	// freshness stays content-hash arbitrated.
	var preParseStatHash *pendingProviderStatHash
	if hasher, ok := e.sources().providerStatHashers[file.Agent]; ok &&
		e.providerStatDigestEligible(file.Agent) {
		if physicalPath := providerDiscoveredPath(source); physicalPath != "" {
			targetKey := physicalPath
			if e.pathRewriter != nil {
				targetKey = e.pathRewriter(physicalPath)
			}
			preParseStatHash = &pendingProviderStatHash{
				agent:        file.Agent,
				physicalPath: physicalPath,
				targetKey:    targetKey,
				digest:       hasher.ComputeMultiFileStatHash(physicalPath),
			}
		}
	}

	// Persisted stat-digest skip. This runs before the single-session
	// content guard below on purpose: a matching digest (size, mtime,
	// ctime, and inode per component) plus a current stored row
	// proves the source unchanged without opening it, so a fresh process
	// (daemon restart or one-shot CLI sync) skips on stats alone instead of
	// re-hashing the full transcript through
	// providerIncrementalContentChanged. A same-size same-mtime in-place
	// rewrite bumps a ctime, and an atomic replacement changes the inode
	// even when a coarse clock gives it the original's timestamps;
	// either breaks the digest and falls through to the content-verified
	// gates.
	if !forceSourceCwdParse && !codexForceFullParse &&
		!codexProvenUnchanged && !codexAuditDeepVerify {
		if freshMTime, fresh := e.providerSourceFreshBeforeFingerprint(
			ctx, source, file, preParseStatHash,
		); fresh {
			if verifiedStateOK {
				e.promoteVerifiedSource(verifiedCapture)
			}
			return processResult{
				skip:  true,
				mtime: freshMTime,
			}, true
		}
	}

	// Consult the heavier checkpoint tables only after the free in-memory and
	// persisted stat-digest gates have declined. This preserves current-main
	// warm-sweep behavior while still proving appends before any full content
	// fingerprint read. Audit mode deliberately bypasses this optimization and
	// takes the authoritative full-parse path below.
	if isCodexFormatAgent(file.Agent) && !codexAuditDeepVerify {
		cpResult, cpErr := e.codexCheckpointFingerprint(ctx, source, file)
		if cpErr != nil {
			log.Printf("codex checkpoint %s: %v", file.Path, cpErr)
		} else {
			switch cpResult.decision {
			case codexCheckpointUnchanged:
				codexProvenUnchanged = true
				codexUnchangedMtime = cpResult.fingerprint.MTimeNS
			case codexCheckpointAppend:
				fingerprint = cpResult.fingerprint
				codexCheckpoint = cpResult.checkpoint
				codexSeed = cpResult.seed
				codexFullHash = cpResult.fingerprint.Hash
				codexHashState = cpResult.hashState
			case codexCheckpointMissing, codexCheckpointFallback:
			case codexCheckpointInvalid:
				codexForceFullParse = true
			}
		}
	}

	// DB-freshness skip for single-session JSONL providers (Claude):
	// when the stored session's size, mtime, and data version already
	// match the source and its project does not need reparse, skip the
	// parse entirely. This reproduces the legacy process arm's
	// shouldSkipFile gate so an unchanged session is not re-parsed on
	// every full sync. A content-verified skip confirmed the current
	// bytes against the stored row hash, so it backfills the stat digest
	// for rows that predate the side-table (or whose digest an index or
	// companion touch invalidated); without the stamp those rows would
	// re-hash on every fresh process forever, since a skip never writes.
	sourceForceReplace := false
	if !forceSourceCwdParse && !codexForceFullParse &&
		!codexProvenUnchanged && !codexAuditDeepVerify {
		if mtime, fresh, forceReplace, contentVerified := e.providerSingleSessionFresh(
			ctx, provider, source, file,
		); fresh {
			if !verifiedStateOK || contentVerified {
				if verifiedStateOK {
					e.promoteVerifiedSource(verifiedCapture)
				}
				if contentVerified {
					e.stampProviderStatHashForConfirmedSource(
						ctx, preParseStatHash,
					)
				}
				return processResult{
					skip:  true,
					mtime: mtime,
				}, true
			}
			// A gate-eligible local source without a comparable stored hash
			// takes the fingerprint path once to earn verified-source trust.
		} else if forceReplace {
			sourceForceReplace = true
		}
	}

	// Watermark-only shared-container sources (changed-path classification)
	// carry just the session-row watermark. When it does not advance past
	// the stored composite watermark, the session and project rows provably
	// did not change, so skip before Fingerprint pays the per-session child
	// lookup; a child-only edit this cannot see is reconciled by the next
	// full-discovery pass, whose digest comparison still catches it.
	if !forceSourceCwdParse && !codexForceFullParse &&
		!codexProvenUnchanged && !codexAuditDeepVerify {
		if freshMtime, fresh := e.watermarkOnlySQLiteSourceFresh(ctx,
			source, file,
		); fresh {
			return processResult{
				skip:  true,
				mtime: freshMtime,
			}, true
		}
	}

	// The checkpoint proved the committed transcript and the current stat
	// snapshot agree. Persist that same snapshot so archives created before
	// provider freshness digests do not re-enter the content-hash path after
	// every restart; this also refreshes a digest after an unrelated
	// session-index touch.
	if codexProvenUnchanged {
		if verifiedStateOK {
			e.promoteVerifiedSource(verifiedCapture)
		}
		e.stampProviderStatHashForConfirmedSource(ctx, preParseStatHash)
		return processResult{
			skip:        true,
			mtime:       codexUnchangedMtime,
			noCacheSkip: true,
		}, true
	}

	// Claude keeps its existing freshness gates and uses only proven appends.
	// As for Codex, bytes before the tail anchor are trusted as append-only.
	if file.Agent == parser.AgentClaude && !sourceForceReplace && !forceSourceCwdParse {
		if cpResult, err := e.codexCheckpointFingerprint(ctx, source, file); err == nil && cpResult.decision == codexCheckpointAppend {
			fingerprint = cpResult.fingerprint
			codexCheckpoint = cpResult.checkpoint
			codexFullHash = cpResult.fingerprint.Hash
			codexHashState = cpResult.hashState
		}
	}
	// codexFingerprintFromParse marks a never-synced Codex-format source
	// whose fingerprint is derived from the parser's single-pass hash
	// state instead of a separate full-file fingerprint read.
	codexFingerprintFromParse := false
	if codexCheckpoint == nil {
		var err error
		if isCodexFormatAgent(file.Agent) &&
			!verifiedFresh &&
			!e.forceParseRequested(file) &&
			!codexAuditDeepVerify {
			// A never-synced Codex-format source has no stored hash to
			// compare: skip the standalone fingerprint read and derive
			// the hash from the parser's single-pass capture after the
			// parse, so the cold full sync reads the source once instead
			// of twice. Only the identity fields are needed up front;
			// every skip gate above this point requires a stored row or
			// cache entry, which a new source does not have. A persisted
			// skip-cache entry for the same base path (e.g. a parse-error
			// entry keyed by the real source hash) keeps the fingerprint
			// read so its cache key still matches.
			lookupPath := file.Path
			if e.pathRewriter != nil {
				lookupPath = e.pathRewriter(file.Path)
			}
			_, hasHash := e.db.GetFileHashByAgentPath(ctx,
				lookupPath, string(file.Agent),
			)
			e.skipMu.RLock()
			skipBase := providerAgentSkipCacheKey(file.Path, file.Agent)
			_, hasSkipEntry := e.skipHashKeys[skipBase]
			e.skipMu.RUnlock()
			// A cold parse failure has no captured content hash. Its entry forces
			// a full fingerprint on the retry before it can become a cache hit.
			_, hasFailure := e.failures.Lookup(failureKey)
			if !hasHash && !hasSkipEntry && !hasFailure {
				if info, statErr := os.Stat(file.Path); statErr == nil {
					inode, device := getFileIdentity(file.Path, info)
					mtime := info.ModTime().UnixNano()
					if file.Agent == parser.AgentCodex {
						mtime = parser.CodexEffectiveMtime(
							file.Path, mtime,
						)
					}
					fingerprint = parser.SourceFingerprint{
						Key: codexCheckpointFingerprintKey(
							source, file.Path,
						),
						Size:    info.Size(),
						MTimeNS: mtime,
						Inode:   uint64(inode),
						Device:  uint64(device),
					}
					codexFingerprintFromParse = true
				}
			}
		}
		if !codexFingerprintFromParse {
			var meter parser.ContentReadMeter
			fingerprint, err = e.providerFingerprint(
				parser.WithContentReadMeter(ctx, &meter), provider, source,
			)
			if meter.Reads() > 0 {
				e.countContentCheck(meter.Bytes())
			}
		}
		if err != nil {
			if (file.ForceParse || file.ForceFullParse) &&
				providerDeletedPhysicalSQLiteSource(file.Agent, file.Path) &&
				errors.Is(err, os.ErrNotExist) {
				excludedSessionIDs, ownershipErr := e.providerSourceSessionIDsForForceReplace(
					ctx, provider, source,
				)
				if ownershipErr != nil {
					return processResult{
						err:         ownershipErr,
						noCacheSkip: true,
					}, true
				}
				return processResult{
					excludedSessionIDs: excludedSessionIDs,
					forceReplace:       true,
				}, true
			}
			return markSourceFailure(file, processResult{err: err},
				failurecache.Identity{Missing: true}, errors.Is(err, os.ErrNotExist) && e.sourcePathMissing(file)), true
		}
	}
	cacheKey := providerProcessCacheKey(
		file, source, fingerprint, providerSemantics,
	)
	cacheSkip := e.shouldCacheSkip(file)
	if cacheSkip && !forceSourceCwdParse &&
		!e.forceParseBypassesCache(file) &&
		!codexForceFullParse && !codexAuditDeepVerify {
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[cacheKey]
		e.skipMu.RUnlock()
		if cached && cachedMtime == fingerprint.MTimeNS {
			// A cached skip must not hide a session whose stored row needs
			// self-healing (e.g. a parser data-version bump or generated
			// roborev CI worktree project): clear the entry and fall through
			// to a full reparse, mirroring the legacy process arm.
			cacheFresh, cacheRowHashVerified := e.providerSkipCacheEntryFreshInDB(ctx,
				file,
				source,
				fingerprint,
				providerSemantics,
			)
			indexChanged, indexVerified := false, true
			if file.Agent == parser.AgentCodex {
				indexChanged, indexVerified = e.codexCachedIndexSessionNameState(ctx, file.Path)
			}
			if !cacheFresh {
				e.clearSkip(ctx, cacheKey)
			} else if e.pathNeedsCachedSkipBypass(ctx, file.Agent, file.Path) {
				e.clearSkip(ctx, cacheKey)
			} else if indexChanged {
				// The transcript fingerprint can remain byte-for-byte identical
				// while session_index.jsonl changes this session's title. Do not
				// let a pre-existing transcript skip entry hide that metadata
				// refresh; non-Codex providers avoid the index lookup entirely.
				e.clearSkip(ctx, cacheKey)
			} else {
				cacheStillFresh := true
				if file.Agent == parser.AgentOmnigent {
					restored, err := parser.RestoreOmnigentCachedSourceState(
						ctx, provider, source,
					)
					if err != nil {
						return processResult{
							err: fmt.Errorf(
								"restore cached %s source state: %w",
								file.Agent, err,
							),
						}, true
					}
					if restored {
						currentFingerprint, err := e.providerFingerprint(
							ctx, provider, source,
						)
						if err != nil {
							return processResult{err: err}, true
						}
						if currentFingerprint != fingerprint {
							e.clearSkip(ctx, cacheKey)
							fingerprint = currentFingerprint
							cacheKey = providerProcessCacheKey(
								file,
								source,
								fingerprint,
								providerSemantics,
							)
							cacheStillFresh = false
						}
					}
				}
				if cacheStillFresh {
					if verifiedStateOK && indexVerified &&
						e.shouldSkipProviderSourceByDB(ctx,
							file, fingerprint, providerSemantics,
						) {
						e.promoteVerifiedSource(verifiedCapture)
					}
					// The fingerprint just content-hashed the current
					// source and matched the stored row, so this skip may
					// backfill or refresh the stat digest for rows that
					// predate the side-table or whose digest a shared
					// index touch invalidated.
					if cacheRowHashVerified && indexVerified {
						e.stampProviderStatHashForConfirmedSource(
							ctx, preParseStatHash,
						)
					}
					return processResult{
						skip:       true,
						mtime:      fingerprint.MTimeNS,
						cacheSkip:  true,
						cachedSkip: true,
						cacheKey:   cacheKey,
					}, true
				}
				// A commit raced cache validation and tracker restoration.
				// Fall through to parse the now-current container.
			}
		}
	}
	if cacheSkip && !forceSourceCwdParse && e.shouldSkipProviderSource(ctx,
		file, source, fingerprint, providerSemantics,
	) {
		return processResult{
			skip:      true,
			mtime:     fingerprint.MTimeNS,
			cacheSkip: true,
			cacheKey:  cacheKey,
		}, true
	}

	// Append-only incremental parse for already-synced JSONL files.
	// When the incremental path declines but signals forceReplace,
	// carry the flag onto the full parse so the write path replaces
	// stored messages instead of appending on top of stale rows.
	var incRes processResult
	var incOK bool
	if codexForceFullParse {
		incRes = processResult{forceReplace: true}
	} else if codexAuditDeepVerify {
		// The audit content-hashes the full source below and repairs only
		// on mismatch; never tail-apply against a prefix it cannot prove.
		incRes = processResult{}
	} else if !forceSourceCwdParse {
		incRes, incOK = e.tryProviderIncrementalAppend(
			ctx, provider, source, file, fingerprint,
			codexSeed, codexFullHash, codexHashState, codexCheckpoint,
		)
	}
	if incOK {
		incRes.mtime = fingerprint.MTimeNS
		incRes.cacheSkip = cacheSkip
		incRes.cacheKey = cacheKey
		if incRes.incremental != nil &&
			incRes.incremental.fileSize == fingerprint.Size {
			incRes.incremental.providerStatHash = preParseStatHash
		}
		return incRes, true
	}
	// A Claude checkpoint proves only the append path. A full parse needs
	// an authoritative fingerprint, including bytes before the tail anchor.
	if file.Agent == parser.AgentClaude && codexCheckpoint != nil {
		var meter parser.ContentReadMeter
		fingerprint, err = e.providerFingerprint(parser.WithContentReadMeter(ctx, &meter), provider, source)
		if err != nil {
			return processResult{err: err}, true
		}
		e.countContentCheck(meter.Bytes())
		cacheKey = providerProcessCacheKey(file, source, fingerprint, providerSemantics)
	}
	// An explicit full pass must replace the stored message stream after
	// bypassing incremental append. Otherwise a complete parse can still be
	// written with append semantics and leave stale earlier rows untouched.
	//
	// file.ForceFullParse means "this file was just fetched and must be
	// looked at" (e.g. a routine remote-mirror import marking a
	// freshly-synced state.db), not "every session this file fans out to
	// must be rewritten." For Hermes/Augure Desktop, one state.db expands
	// into many per-session members (MemberFreshnessOverridesFileForce);
	// folding the file-level flag into every member's
	// incForceReplace would force-rewrite every session in the archive on
	// every routine import, defeating this provider's whole per-session
	// freshness model. Only a member-level force (sourceForceReplace,
	// incRes.forceReplace, or the engine-wide e.forceFullParse) bypasses
	// the member's own unchanged-skip below. A standalone source, such as
	// a Hermes transcript, is the fetched file itself and keeps
	// file.ForceFullParse's existing effect, so an in-place remote edit
	// still replaces its stored messages.
	fileForceFullParse := file.ForceFullParse &&
		(!providerSemantics.MemberFreshnessOverridesFileForce || !providerVirtualSourceContainerExists(file.Path))
	incForceReplace := sourceForceReplace || incRes.forceReplace ||
		e.forceFullParse || fileForceFullParse

	// DB-stored fingerprint skip. The provider has no database handle, so the
	// engine reproduces the legacy DB-aware skip that single-session JSONL
	// providers relied on: an unchanged source whose stored size and effective
	// mtime already match is not reparsed, even when the in-memory skip cache
	// was cleared (e.g. by SyncSingleSession) or never populated (a fresh
	// engine). For Codex this also folds in the session_index.jsonl sidecar:
	// a shared index mtime bump that did not change this session's title must
	// not trigger a reparse.
	// A rejected checkpoint forbids resuming from its cursor, but does not
	// invalidate a matching full-source hash. In particular, device numbers
	// can change across boots without changing the transcript.
	// A fingerprint awaiting its parse-derived hash cannot prove freshness.
	if !forceSourceCwdParse && !e.forceParseRequested(file) &&
		(!incForceReplace || codexForceFullParse && fingerprint.Hash != "") {
		dbFresh, metadataVerified := e.providerSourceFreshnessByDB(ctx,
			file, fingerprint, providerSemantics,
		)
		if dbFresh && verifiedStateOK && metadataVerified {
			e.promoteVerifiedSource(verifiedCapture)
		}
		// The Codex-family fingerprint content-hashed the rollout and
		// shouldSkipCodexFingerprint verified it against the stored row
		// (hash, project, data version, index title), so this skip may
		// backfill or refresh the stat digest: rows that predate the
		// side-table, and rollouts whose digest a shared index touch
		// invalidated without changing their own title, would otherwise
		// re-hash on every fresh process forever.
		if dbFresh {
			if metadataVerified {
				e.stampProviderStatHashForConfirmedSource(
					ctx, preParseStatHash,
				)
			}
			return processResult{
				skip:        true,
				mtime:       fingerprint.MTimeNS,
				cacheSkip:   cacheSkip,
				cacheKey:    cacheKey,
				noCacheSkip: true,
			}, true
		}
	}

	// DB-stored-file-info skip: a session whose persisted file_size/file_mtime
	// already match the source fingerprint (and whose data_version is current)
	// is unchanged and need not be reparsed. This reproduces the legacy
	// shouldSkipByPath behavior the per-agent process methods provided before the
	// migration, so a repeat full/periodic sync of an untouched
	// provider-authoritative session (OpenHands, Cursor, Hermes, Vibe, ...)
	// skips instead of rewriting. It only skips on an exact size+mtime match, so
	// a provider whose fingerprint mtime differs from the stored value simply
	// reparses, matching the prior behavior. Claude and Cowork have their own
	// earlier freshness checks; this is the generic fallback for the rest.
	//
	// This member's own check below (providerSourceUnchangedInDB) is a
	// content-hash freshness decision that already rejects a genuinely
	// changed member; forceParseRequested's file.ForceFullParse term exists
	// for providers where one DiscoveredFile is one physical file, so
	// "reparse this file" and "reparse this member" are the same thing. For
	// Hermes/Augure Desktop's per-session fan-out
	// (MemberFreshnessOverridesFileForce), they are not: a routine
	// remote-mirror import marks the shared state.db ForceFullParse
	// because the mirror layer can't
	// incrementally diff it, and that flag reaches every member it fans out
	// to. Excluding it here (fileForceFullParse), specifically for this
	// skip, keeps that import from rewriting every unchanged session on
	// every routine sync.
	memberForceParseRequested := e.forceParse || e.forceFullParse || file.ForceParse ||
		fileForceFullParse
	if !forceSourceCwdParse && !incForceReplace && !memberForceParseRequested &&
		e.providerSourceUnchangedInDB(
			ctx, source, fingerprint, providerSemantics, preParseStatHash,
		) {
		return processResult{
			skip:      true,
			mtime:     fingerprint.MTimeNS,
			cacheSkip: cacheSkip,
			cacheKey:  cacheKey,
		}, true
	}

	failureIdentity, failureIdentityOK := e.sourceFailureCacheIdentity(file, fingerprint)
	if failureIdentityOK {
		cacheFile := file
		forceFailureRefresh := forceSourceCwdParse
		if piebaldFailure.retry && !explicitForce && !cwdDecision.forceParse {
			// Internal retry intent bypasses archive freshness, but an unchanged
			// durable failure still skips. Explicit refreshes bypass both.
			cacheFile.ForceParse = false
			forceFailureRefresh = false
		}
		if forceFailureRefresh || codexForceFullParse || codexAuditDeepVerify {
			e.failures.Clear(failureKey)
		} else if cached, ok := e.cachedSourceFailure(ctx, cacheFile, failureIdentity); ok {
			return cached, true
		}
	}

	// Unchanged sources cannot replace the preferred copy, so resolve its
	// rank only after the freshness gates have declined.
	if lowerRanked, err := e.providerSourceLowerRanked(
		ctx, provider, file.Agent, source,
	); err != nil {
		return processResult{
			err: fmt.Errorf("rank %s source %s: %w", file.Agent, providerDiscoveredPath(source), err),
		}, true
	} else if lowerRanked {
		return processResult{
			skip: true, suppressPresenceSweep: true,
		}, true
	}

	// Provider parse seam: every gate above returns a lease-free skip. From
	// here the provider parses the source, so acquire the retention lease that
	// bounds the parsed payload and attach it to every result carrying that
	// data. A result still classified as a skip below releases it immediately.
	sourceBytes := e.parseRetentionSourceBytes(file)
	if sizer, ok := provider.(parser.ParseSourceSizer); ok {
		sourceBytes, err = sizer.ParseSourceSize(ctx, source)
		if err != nil {
			return processResult{err: err}, true
		}
	}
	unknownSourceSize := sourceBytes < 0
	sourceBytes = max(sourceBytes, 0) // Unknown sizes reserve the whole retention budget.
	lease, err := e.retentionBudget().acquire(
		ctx, sourceBytes,
	)
	if err != nil {
		return processResult{err: err}, true
	}
	if runtimeMetrics := reconciliationRuntimeMetricsFor(ctx); runtimeMetrics != nil {
		if _, _, ok := sqliteContainerSourceForFile(file); ok {
			runtimeMetrics.openCodeSQLiteParse()
		}
	}
	// Large Codex full parses stream through the scratch staging sink: the
	// in-memory model keeps placeholders instead of tool-result content, so
	// peak memory stays bounded by messages + one scratch batch rather than
	// the transcript size. Forks with archived calls also stage: a shortened
	// source does not bound their retained result payloads. Report-only
	// parse-diff disables staging through e.forceParse; ordinary forced full
	// syncs retain the bounded-memory path.
	var stagedSink *codexStagingSink
	var stagedGCRelease func()
	stageCodex := unknownSourceSize || sourceBytes > e.stagedCodexMin
	if file.Agent == parser.AgentCodex && !stageCodex && !e.forceParse {
		reader := e.db
		if archive, ok := e.archiveStore.(*db.DB); ok {
			reader = archive
		}
		stageCodex, err = reader.HasCodexForkToolHistory(ctx, string(file.Agent), e.effectiveSourcePath(providerDiscoveredPath(source)))
		if err != nil {
			lease.Release()
			return processResult{err: err}, true
		}
	}
	if file.Agent == parser.AgentCodex &&
		stageCodex &&
		!e.forceParse {
		stagedSink, err = newCodexStagingSink(ctx,
			e.stagedCodexDir, e.blockedResultCategories, sourceBytes,
		)
		if err != nil {
			lease.Release()
			return processResult{
				err:         fmt.Errorf("codex staging sink: %w", err),
				mtime:       fingerprint.MTimeNS,
				cacheSkip:   cacheSkip,
				cacheKey:    cacheKey,
				noCacheSkip: true,
			}, true
		}
		stagedSink.toolResultImages = e.toolResultImages
		stagedSink.database = e.db
		stagedSink.idPrefix = e.idPrefix
		stagedSink.disableSignals = e.disableSignalRecompute
		stagedGCRelease = beginStagedColdSync()
	}
	admission := &providerResultAdmission{
		engine: e, ctx: ctx, def: provider.Definition(), file: file,
		source: source, fingerprint: fingerprint,
		policy:          providerSemantics.UnchangedResults,
		codexIdentities: isCodexFormatAgent(file.Agent) && stagedSink == nil,
	}
	var outcome parser.ParseOutcome
	if stagedSink != nil {
		if e.forceParseRequested(file) {
			parser.EvictCodexSessionIndexForSession(
				providerDiscoveredPath(source),
			)
		}
		stagedConfig := providerConfig
		stagedConfig.Machine = machine
		outcome, err = stagedCodexParseOutcome(
			ctx, stagedConfig, source, fingerprint, stagedSink,
		)
		if err == nil {
			outcome, err = admission.admitAll(outcome)
		}
	} else {
		outcome, err = parser.ParseEach(ctx, provider, parser.ParseRequest{
			Source:             source,
			Fingerprint:        fingerprint,
			Machine:            machine,
			ForceParse:         e.forceParseRequested(file),
			StoredPathResolver: e.storedPathResolver,
		}, admission.admit)
	}
	if err != nil && admission.validationErr == nil {
		if stagedSink != nil {
			stagedSink.Close()
		}
		if stagedGCRelease != nil {
			stagedGCRelease()
		}
		if file.Agent == parser.AgentPiebald && !explicitForce {
			e.rememberPiebaldParseFailure(ctx, source, piebaldFailure, err)
		}
		if !e.forceParse {
			cwdChanged, reconcileErr := e.reconcileSourceCwdByPath(ctx,
				source, cwdDecision,
			)
			if reconcileErr != nil {
				err = errors.Join(err, reconcileErr)
			}
			return markSourceFailure(file, processResult{
				err:              err,
				sourceCwdChanged: cwdChanged,
				mtime:            fingerprint.MTimeNS,
				cacheSkip:        cacheSkip,
				cacheKey:         cacheKey,
				noCacheSkip:      true,
				retentionLease:   lease,
			}, failureIdentity, failureIdentityOK && sourceParseFailureCacheable(err, file.Path)), true
		}
		return markSourceFailure(file, processResult{
			err:            err,
			mtime:          fingerprint.MTimeNS,
			cacheSkip:      cacheSkip,
			cacheKey:       cacheKey,
			noCacheSkip:    true,
			retentionLease: lease,
		}, failureIdentity, failureIdentityOK && sourceParseFailureCacheable(err, file.Path)), true
	}
	if file.Agent == parser.AgentPiebald && !e.forceParse {
		e.clearPiebaldFailure(source)
	}
	validationErr := admission.validationErr
	if validationErr == nil {
		validationErr = validateProviderOutcome(provider.Definition(), source, fingerprint, outcome)
	}
	if validationErr != nil {
		if stagedSink != nil {
			stagedSink.Close()
		}
		if stagedGCRelease != nil {
			stagedGCRelease()
		}
		return markSourceFailure(file, processResult{
			err:            validationErr,
			mtime:          fingerprint.MTimeNS,
			cacheSkip:      cacheSkip,
			cacheKey:       cacheKey,
			noCacheSkip:    true,
			retentionLease: lease,
		}, failureIdentity, failureIdentityOK), true
	}
	if codexFingerprintFromParse && fingerprint.Hash == "" {
		// A completed parse captures the full source hash even when its pending
		// calls cannot fit a persisted checkpoint.
		if admission.firstHash != "" {
			fingerprint.Hash = admission.firstHash
		}
		if fingerprint.Hash != "" {
			cacheKey = providerProcessCacheKey(file, source, fingerprint, providerSemantics)
		}
	}
	cleanCache := providerOutcomeAllowsCleanSkipCache(outcome) && len(admission.retries) == 0
	providerWideFailureCount := len(outcome.SourceErrors)
	if !outcome.ResultSetComplete {
		providerWideFailureCount++
	}
	providerFailureCount := providerWideFailureCount
	if outcome.SkipReason != parser.SkipNone {
		if outcome.SkipReason == parser.SkipUnsupportedSource {
			e.anomalies.recordUnsupportedSourceLayout(string(file.Agent), file.Path)
		}
		excludedSessionIDs := append([]string(nil), outcome.ExcludedSessionIDs...)
		preservedSessionIDs := providerPreservedSessionIDs(provider, source)
		var missingMembers []sourceMissingMember
		if file.Agent == parser.AgentKiro &&
			parser.KiroSQLiteSourcePresent(source) &&
			outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
			// Only a present SQLite container owns a complete membership set.
			// Current and legacy JSONL sources can legitimately parse to no
			// accepted records during a partial rewrite and must preserve archive rows.
			missingMembers, err = e.providerSourceMissingSessionOwnershipsForCompleteResultWithPreserved(
				ctx, provider, source, preservedSessionIDs, nil,
			)
			if err != nil {
				return processResult{
					err:            err,
					mtime:          fingerprint.MTimeNS,
					cacheSkip:      cacheSkip,
					cacheKey:       cacheKey,
					noCacheSkip:    true,
					retentionLease: lease,
				}, true
			}
		} else if outcome.ForceReplace && outcome.ResultSetComplete {
			owned, ownershipErr := e.providerSourceSessionOwnershipsForForceReplace(
				ctx, provider, source,
			)
			if ownershipErr != nil {
				return processResult{
					err:            ownershipErr,
					mtime:          fingerprint.MTimeNS,
					cacheSkip:      cacheSkip,
					cacheKey:       cacheKey,
					noCacheSkip:    true,
					retentionLease: lease,
				}, true
			}
			omnigentContainerExists :=
				parser.IsOmnigentContainerSource(source) &&
					parser.IsRegularFile(providerDiscoveredPath(source))
			sharedContainerExists := (file.Agent == parser.AgentTrae ||
				file.Agent == parser.AgentCursorIDE) &&
				parser.IsRegularFile(providerDiscoveredPath(source))
			sourceFileMissing := false
			if statPath := validatedProviderSourceStatPath(file.Path); statPath != "" {
				_, statErr := e.lstatSource(statPath)
				sourceFileMissing = os.IsNotExist(statErr)
			}
			if e.pathRewriter != nil ||
				(providerVirtualSourceContainerExists(file.Path) ||
					omnigentContainerExists || sharedContainerExists ||
					sourceFileMissing) {
				// The provider re-resolved this exact virtual member against a
				// still-present shared container, or authoritatively parsed an
				// empty Omnigent, Trae, or Cursor IDE container, or the backing
				// source itself is gone. Carry the stored ownership to the
				// recoverable source-missing seam instead of treating absence
				// as a parser exclusion.
				missingMembers = owned
			} else {
				for _, member := range owned {
					excludedSessionIDs = append(excludedSessionIDs, member.sessionID)
				}
			}
		}
		skipRes := processResult{
			// A complete Kiro source can report no current rows while still
			// owning stored members that need source-missing reconciliation.
			skip:                 !outcome.ForceReplace && len(missingMembers) == 0,
			excludedSessionIDs:   excludedSessionIDs,
			preservedSessionIDs:  preservedSessionIDs,
			sourceMissingMembers: missingMembers,
			sourceBytes:          sourceBytes,
			mtime:                fingerprint.MTimeNS,
			cacheSkip:            cacheSkip,
			cacheKey:             cacheKey,
			noCacheSkip:          !cleanCache,
			forceReplace:         outcome.ForceReplace,
			suppressPresenceSweep: outcome.SkipReason == parser.SkipUnsupportedSource ||
				!outcome.ResultSetComplete,
			providerFailureCount:     providerFailureCount,
			providerWideFailureCount: providerWideFailureCount,
		}
		if file.Agent == parser.AgentClaude && cleanCache &&
			!e.forceParseRequested(file) {
			skipRes.claudeRowlessFreshnessKey = e.claudeRowlessFreshnessCacheKey(file.Path, fingerprint.Hash)
		}
		// A SkipReason outcome without a force-replace carries no parsed data,
		// so it stays a lease-free skip; a force-replace is parse-bearing.
		if skipRes.skip {
			lease.Release()
		} else {
			skipRes.retentionLease = lease
		}
		return skipRes, true
	}
	// Only Session.ID and Session.File.Path are populated for ownership checks.
	parsedResults := admission.emitted
	parsedCount := len(parsedResults)
	excludedSessionIDs := append([]string(nil), outcome.ExcludedSessionIDs...)
	preservedSessionIDs := providerPreservedSessionIDs(provider, source)
	var missingMembers []sourceMissingMember
	// Parse-diff intentionally reports a removed Trae member through its
	// presence sweep. It never filters unchanged results or writes tombstones,
	// so ownership reconciliation is needed only by real sync engines.
	if (file.Agent == parser.AgentKiro ||
		file.Agent == parser.AgentCline ||
		(file.Agent == parser.AgentOmnigent && outcome.ForceReplace) ||
		(file.Agent == parser.AgentCursorIDE && outcome.ForceReplace) ||
		(file.Agent == parser.AgentTrae && !e.forceParse)) &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
		missingMembers, err = e.providerSourceMissingSessionOwnershipsForCompleteResultWithPreserved(
			ctx, provider, source, preservedSessionIDs, parsedResults,
		)
		if err != nil {
			return processResult{
				err:            err,
				mtime:          fingerprint.MTimeNS,
				cacheSkip:      cacheSkip,
				cacheKey:       cacheKey,
				noCacheSkip:    true,
				retentionLease: lease,
			}, true
		}
	} else if (file.Agent == parser.AgentIcodemate ||
		file.Agent == parser.AgentCodebuff) && outcome.ForceReplace &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
		if file.Agent == parser.AgentCodebuff {
			missingMembers, err = e.codebuffSourceMissingMembers(
				ctx, file.Path, outcome.ExcludedSessionIDs, parsedResults,
			)
		} else {
			missingMembers, err = e.completeMultiSessionSourceMissingMembers(
				ctx, file.Agent, file.Path,
				outcome.ExcludedSessionIDs, parsedResults,
			)
		}
		if err != nil {
			return processResult{
				err:            err,
				mtime:          fingerprint.MTimeNS,
				cacheSkip:      cacheSkip,
				cacheKey:       cacheKey,
				noCacheSkip:    true,
				retentionLease: lease,
			}, true
		}
	} else if file.Agent == parser.AgentClaude &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
		missingMembers, err = e.claudeSourceMissingSessionOwnershipsForCompleteResult(
			ctx, file.Path, outcome.ExcludedSessionIDs, parsedResults,
		)
		if err != nil {
			return processResult{
				err:            err,
				mtime:          fingerprint.MTimeNS,
				cacheSkip:      cacheSkip,
				cacheKey:       cacheKey,
				noCacheSkip:    true,
				retentionLease: lease,
			}, true
		}
	}
	filteredResults, truncationVerifyFailed := admission.kept, admission.truncationVerifyFailed
	if stagedSink != nil && len(filteredResults) == 0 {
		// Every result was dropped as unchanged; nothing will publish the
		// staged rows, so release the scratch sink now.
		stagedSink.Close()
		stagedSink = nil
		stagedGCRelease()
		stagedGCRelease = nil
	}
	res := processResult{
		results:              filteredResults,
		excludedSessionIDs:   excludedSessionIDs,
		preservedSessionIDs:  preservedSessionIDs,
		sourceMissingMembers: missingMembers,
		sourceBytes:          sourceBytes,
		mtime:                fingerprint.MTimeNS,
		cacheSkip:            cacheSkip,
		cacheKey:             cacheKey,
		noCacheSkip:          !cleanCache || truncationVerifyFailed,
		forceReplace:         outcome.ForceReplace || incForceReplace,
		suppressPresenceSweep: outcome.SkipReason == parser.SkipUnsupportedSource ||
			!outcome.ResultSetComplete,
		providerFailureCount:     providerFailureCount,
		providerWideFailureCount: providerWideFailureCount,
		retentionLease:           lease,
		providerStatHash:         preParseStatHash,
		staged:                   stagedSink,
		stagedGCRelease:          stagedGCRelease,
		sourceCwdResolution:      cwdDecision.resolution,
		sourceCwdStored:          cwdDecision.storedCwd,
		sourceCwdStoredOK:        cwdDecision.storedOK,
	}
	if (file.Agent == parser.AgentOmnigent ||
		file.Agent == parser.AgentCursorIDE) && cacheSkip && cleanCache &&
		!e.forceParseRequested(file) &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 &&
		fingerprint.Hash != "" {
		// A whole-container parse may only be skip-cached after its member
		// writes commit (cache-after-write); virtual member parses keep the
		// immediate cache path. A truncation-verification failure must not
		// be promoted either: caching that pass as clean would freeze the
		// refused result behind a cached skip instead of retrying it.
		res.cacheAfterWrite = providerPersistentSharedContainerSource(source) &&
			!truncationVerifyFailed
	}
	// Incremental-append providers (Claude and Codex) need the stored file
	// identity so a later sync can detect an atomic file replacement
	// (new inode/device) and fall back to a full parse instead of
	// appending on top of stale state. Match the legacy process arm,
	// which stamped inode/device from the source file stat.
	e.stampProviderFileIdentity(provider, source, res.results)
	res.retryCauses = admission.retries
	for _, retry := range admission.retries {
		id := retry.sessionID
		if res.retrySessionIDs == nil {
			res.retrySessionIDs = make(map[string]bool)
		}
		res.retrySessionIDs[id] = true
		if isCodexFormatAgent(file.Agent) {
			res.deferredCount++
		} else {
			res.providerFailureCount++
		}
	}
	if e.forceParseRequested(file) {
		for _, sourceErr := range outcome.SourceErrors {
			res.sessionErrs = append(res.sessionErrs, sessionParseError{
				sessionID:   sourceErr.SessionID,
				virtualPath: sourceErr.SourceKey,
				err:         sourceErr.Err,
			})
		}
	}
	e.applyProviderFilePathPolicies(ctx, provider, file.Agent, file.Path, &res, e.idPrefix)
	if file.Agent == parser.AgentClaude && cleanCache &&
		!e.forceParseRequested(file) {
		res.claudeRowlessFreshnessKey = e.claudeRowlessFreshnessCacheKey(file.Path, fingerprint.Hash)
	}
	if storageStateOK {
		e.stageOpenCodeStorageTrust(
			&res, file.Path, storageState, storageSnap,
			parsedCount, outcome.ResultSetComplete,
		)
	}
	return res, true
}

// dropUnchangedSharedSQLiteResults reproduces the legacy per-session skip the
// folded processZed/processShelley loops and the aiderFileUnchanged check
// performed. Zed, Shelley, and Trae keep every session in one shared SQLite
// database, and Aider fans every run out of one shared history file, so the
// provider re-parses every session on any change to that shared source.
// Without a per-session filter the engine would rewrite and recount unchanged
// sessions. This drops results whose stored file_mtime and, when available, the
// fingerprint stored in file_hash already match, using the path rewriter so
// remote stored paths resolve. Force-parse runs (parse-diff, single-session
// resync) keep every result so they always re-emit.
func (e *Engine) dropUnchangedSharedSQLiteResults(ctx context.Context,
	file parser.DiscoveredFile,
	results []parser.ParseResult,
	policy parser.UnchangedResultPolicy,
) []parser.ParseResult {
	if e.forceParseRequested(file) || len(results) == 0 {
		return results
	}
	if policy == parser.UnchangedResultNone {
		return results
	}
	compareHash := policy == parser.UnchangedResultMTimeAndHash

	kept := results[:0]
	for _, r := range results {
		path := r.Session.File.Path
		if path == "" {
			kept = append(kept, r)
			continue
		}
		lookupPath := path
		if e.pathRewriter != nil {
			lookupPath = e.pathRewriter(path)
		}
		_, storedMtime, ok := e.db.GetFileInfoByPath(ctx, lookupPath)
		if !ok || storedMtime != r.Session.File.Mtime {
			kept = append(kept, r)
			continue
		}
		if compareHash {
			storedHash, _ := e.db.GetFileHashByPath(ctx, lookupPath)
			if storedHash != r.Session.File.Hash {
				kept = append(kept, r)
				continue
			}
		}
		if e.db.GetDataVersionByPath(ctx, lookupPath) < db.CurrentDataVersion() {
			kept = append(kept, r)
			continue
		}
		// Unchanged: drop so the write batch neither rewrites nor recounts it.
	}
	return kept
}

// dropShrinkingTruncatedCursorIDEResults keeps a Cursor IDE composer whose
// parse saw headers referencing missing bubble rows (a partial cursorDiskKV
// wipe, flagged IsTruncated by the parser) from force-replacing an archived
// session that has messages the truncated transcript no longer carries.
// Every message's source_uuid is its bubble UUID, so the guard admits a
// truncated result only when it still contains every archived bubble: a
// message count alone would admit a wipe of an earlier bubble masked by
// newly added turns. First-time discovery and a gapped conversation that
// keeps growing still write, so a wiped database continues to surface its
// remaining content. The emitted result still counts as present for
// ownership reconciliation, so a preserved session stays active rather than
// being marked source-missing.
// Forced parses bypass archive verification. Empty truncated results are still
// filtered during sync; read-only parse-diff receives them for comparison.
// The returned verifyFailed reports that an archive read failed during
// verification: the caller must not skip-cache the pass as clean, so the
// dropped result is re-attempted instead of frozen behind a cached skip.
func (e *Engine) dropShrinkingTruncatedCursorIDEResults(
	ctx context.Context,
	file parser.DiscoveredFile,
	results []parser.ParseResult,
) (kept []parser.ParseResult, verifyFailed bool) {
	if file.Agent != parser.AgentCursorIDE || len(results) == 0 || e.forceParse {
		return results, false
	}
	kept = results[:0]
	for _, r := range results {
		if r.Session.IsTruncated && len(r.Messages) == 0 {
			continue
		}
		kept = append(kept, r)
	}
	results = kept
	if len(results) == 0 || e.forceParseRequested(file) {
		return results, false
	}
	type messageSourceUUIDReader interface {
		ListMessageSourceUUIDs(context.Context, string) ([]string, error)
	}
	reader := messageSourceUUIDReader(e.db)
	if e.archiveStore != nil {
		// A rebuild writes into a fresh database while the original archive
		// remains readable as archiveStore. Verifying against the empty
		// rebuild target would admit every truncated transcript, and once
		// written there the orphan copy would never rescue the fuller
		// original. A refused write instead leaves the session out of the
		// rebuild, and the orphan copy carries the archived full transcript
		// across.
		archived, ok := e.archiveStore.(messageSourceUUIDReader)
		if !ok {
			kept = results[:0]
			for _, r := range results {
				if r.Session.IsTruncated {
					verifyFailed = true
					continue
				}
				kept = append(kept, r)
			}
			return kept, verifyFailed
		}
		reader = archived
	}
	kept = results[:0]
	for _, r := range results {
		if !r.Session.IsTruncated {
			kept = append(kept, r)
			continue
		}
		id := applyIDPrefixToID(e.idPrefix, r.Session.ID)
		archived, err := reader.ListMessageSourceUUIDs(ctx, id)
		if err != nil {
			// The archive cannot be verified; refusing the overwrite is the
			// recoverable direction.
			verifyFailed = true
			continue
		}
		incoming := make(map[string]struct{}, len(r.Messages))
		for _, msg := range r.Messages {
			if msg.SourceUUID != "" {
				incoming[msg.SourceUUID] = struct{}{}
			}
		}
		containsArchive := true
		for _, uuid := range archived {
			if _, ok := incoming[uuid]; !ok {
				containsArchive = false
				break
			}
		}
		if !containsArchive {
			continue
		}
		kept = append(kept, r)
	}
	return kept, verifyFailed
}
