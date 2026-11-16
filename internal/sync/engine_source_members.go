package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func (e *Engine) providerSourceSessionIDsForForceReplace(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) ([]string, error) {
	members, err := e.providerSourceSessionOwnershipsForForceReplace(
		ctx, provider, source,
	)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.sessionID)
	}
	return ids, nil
}

type preservedSessionIDProvider interface {
	PreservedSessionIDs(parser.SourceRef) []string
}

func providerPreservedSessionIDs(
	provider parser.Provider, source parser.SourceRef,
) []string {
	preserved, ok := provider.(preservedSessionIDProvider)
	if !ok {
		return nil
	}
	return preserved.PreservedSessionIDs(source)
}

// providerSourceSessionOwnershipsForForceReplace lists the stored active
// sessions owned by a force-replaced source, paired with the exact stored
// file path each row is tracked under, so callers can either hard-delete
// them as parser exclusions or tombstone their exact source ownership.
func (e *Engine) providerSourceSessionOwnershipsForForceReplace(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) ([]sourceMissingMember, error) {
	agent := provider.Definition().Type
	root := ""
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if candidate != "" {
			root = candidate
			break
		}
	}
	if root == "" {
		return nil, nil
	}
	scopes := []parser.StoredSourceHintScope{{Path: root}}
	if resolver, ok := provider.(parser.StoredSourceHintScopeProvider); ok {
		if resolved := resolver.StoredSourceHintScopes(parser.ChangedPathRequest{Path: root}); len(resolved) > 0 {
			scopes = resolved
		}
	}
	if e.pathRewriter != nil {
		for i := range scopes {
			scopes[i].Path = e.pathRewriter(scopes[i].Path)
		}
	}
	sourcePaths, err := e.db.ListStoredSourcePathHintsContext(ctx,
		string(agent), storedSourceDBHintScopes(scopes),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"list provider force-replace source hints: %w", err,
		)
	}
	seen := make(map[string]struct{})
	var members []sourceMissingMember
	var sessionIDs []string
	for _, sourcePath := range sourcePaths {
		pathIDs, err := e.db.ListSessionIDsByFilePath(ctx, sourcePath, string(agent))
		if err != nil {
			return nil, fmt.Errorf(
				"list provider force-replace sessions for %s: %w",
				sourcePath, err,
			)
		}
		for _, id := range pathIDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			members = append(members, sourceMissingMember{
				sessionID: id,
				filePath:  sourcePath,
				agent:     agent,
				machine: e.machineForProviderSource(
					agent, source, sourcePath,
				),
			})
			sessionIDs = append(sessionIDs, id)
		}
	}
	storedMachines, err := e.db.ListSessionMachinesByID(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list provider force-replace session machines: %w", err,
		)
	}
	for i := range members {
		if machine, exists := storedMachines[members[i].sessionID]; exists {
			members[i].machine = machine
		}
	}
	return members, nil
}

func (e *Engine) providerSourceMissingSessionOwnershipsForCompleteResultWithPreserved(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	preservedSessionIDs []string,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	emitted := make(map[string]struct{}, len(results))
	for _, result := range results {
		id := applyIDPrefixToID(e.idPrefix, result.Session.ID)
		if id != "" {
			emitted[id] = struct{}{}
		}
	}
	for _, id := range preservedSessionIDs {
		if id := applyIDPrefixToID(e.idPrefix, id); id != "" {
			emitted[id] = struct{}{}
		}
	}
	stored, err := e.providerSourceSessionOwnershipsForForceReplace(
		ctx, provider, source,
	)
	if err != nil {
		return nil, err
	}
	missing := make([]sourceMissingMember, 0, len(stored))
	for _, member := range stored {
		if _, present := emitted[member.sessionID]; present {
			continue
		}
		missing = append(missing, member)
	}
	return missing, nil
}

// completeMultiSessionSourceMissingMembers lists active sessions under an
// exact source path that a complete authoritative parse did not emit. This is
// used by new multi-session providers whose current parser owns the complete
// membership set, unlike Claude's separate legacy-only stale-fork cleanup.
func (e *Engine) completeMultiSessionSourceMissingMembers(
	ctx context.Context,
	agent parser.AgentType,
	sourcePath string,
	excludedSessionIDs []string,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	type membershipReader interface {
		ListSessionWriteIdentitiesByID(
			context.Context, []string,
		) (map[string]db.SessionWriteIdentity, error)
		ListSessionIDsByFilePath(context.Context, string, string) ([]string, error)
		ListSessionMachinesByID(
			context.Context, []string,
		) (map[string]string, error)
	}
	reader := membershipReader(e.db)
	if e.archiveStore != nil {
		archived, ok := e.archiveStore.(membershipReader)
		if !ok {
			return nil, fmt.Errorf(
				"archive %T does not support %s source membership lookup",
				e.archiveStore, agent,
			)
		}
		reader = archived
	}
	present := make(map[string]struct{}, len(results)+len(excludedSessionIDs))
	paths := make(map[string]struct{}, 1)
	emittedIDs := make([]string, 0, len(results))
	for _, result := range results {
		if id := applyIDPrefixToID(e.idPrefix, result.Session.ID); id != "" {
			present[id] = struct{}{}
			emittedIDs = append(emittedIDs, id)
		}
		path := result.Session.File.Path
		if path == "" {
			continue
		}
		if e.pathRewriter != nil {
			path = e.pathRewriter(path)
		}
		paths[path] = struct{}{}
	}
	if len(paths) == 0 && sourcePath != "" {
		paths[e.effectiveSourcePath(sourcePath)] = struct{}{}
	}
	storedIdentities, err := reader.ListSessionWriteIdentitiesByID(ctx, emittedIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list stored %s source identities: %w", agent, err,
		)
	}
	for _, identity := range storedIdentities {
		if identity.Agent == string(agent) && identity.FilePath != "" {
			paths[identity.FilePath] = struct{}{}
		}
	}
	for _, id := range e.applyIDPrefixToSessionIDs(excludedSessionIDs) {
		present[id] = struct{}{}
	}

	var members []sourceMissingMember
	var sessionIDs []string
	for path := range paths {
		storedIDs, err := reader.ListSessionIDsByFilePath(ctx, path, string(agent))
		if err != nil {
			return nil, fmt.Errorf(
				"list %s sessions for complete source %s: %w",
				agent, path, err,
			)
		}
		for _, id := range storedIDs {
			if _, ok := present[id]; ok {
				continue
			}
			members = append(members, sourceMissingMember{
				sessionID: id,
				filePath:  path,
				agent:     agent,
			})
			sessionIDs = append(sessionIDs, id)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	storedMachines, err := reader.ListSessionMachinesByID(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list stored %s session machines: %w", agent, err,
		)
	}
	for i := range members {
		members[i].machine = storedMachines[members[i].sessionID]
	}
	return members, nil
}

// codebuffSourceMissingMembers lists stored sessions a complete Codebuff
// transcript parse no longer emits, such as a subagent whose agent block was
// removed. Codebuff and Freebuff share one provider and a transcript's
// classification can change, so both stored agent labels are checked. A row
// whose other-classification counterpart is emitted is a reclassification,
// not a missing member; applyCodebuffClassificationPolicies replaces it.
func (e *Engine) codebuffSourceMissingMembers(
	ctx context.Context,
	sourcePath string,
	excludedSessionIDs []string,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	emitted := make(map[string]struct{}, len(results))
	for _, result := range results {
		if id := applyIDPrefixToID(e.idPrefix, result.Session.ID); id != "" {
			emitted[id] = struct{}{}
		}
	}
	var missing []sourceMissingMember
	for _, agent := range []parser.AgentType{
		parser.AgentCodebuff, parser.AgentFreebuff,
	} {
		members, err := e.completeMultiSessionSourceMissingMembers(
			ctx, agent, sourcePath, excludedSessionIDs, results,
		)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			counterpart, ok := e.codebuffCounterpartID(member.sessionID)
			if ok {
				if _, reclassified := emitted[counterpart]; reclassified {
					continue
				}
			}
			missing = append(missing, member)
		}
	}
	return missing, nil
}

// codebuffCounterpartID returns the stored ID the same Codebuff transcript
// session carries under the other classification: a codebuff: ID maps to its
// freebuff: twin and back. Subagent IDs embed the parent's full ID, so the
// swap covers them too.
func (e *Engine) codebuffCounterpartID(storedID string) (string, bool) {
	id := strings.TrimPrefix(storedID, e.idPrefix)
	codebuffPrefix := string(parser.AgentCodebuff) + ":"
	freebuffPrefix := string(parser.AgentFreebuff) + ":"
	if rest, ok := strings.CutPrefix(id, codebuffPrefix); ok {
		return applyIDPrefixToID(e.idPrefix, freebuffPrefix+rest), true
	}
	if rest, ok := strings.CutPrefix(id, freebuffPrefix); ok {
		return applyIDPrefixToID(e.idPrefix, codebuffPrefix+rest), true
	}
	return "", false
}

// applyCodebuffClassificationPolicies keeps one live classification per
// Codebuff session. Parent and subagent sessions share one transcript path, so
// decisions are made per session ID. A counterpart under the other
// classification that the user trashed or deleted suppresses the new row;
// a live counterpart is replaced.
func (e *Engine) applyCodebuffClassificationPolicies(
	ctx context.Context, filePath string, res *processResult,
) {
	lookupPath := filePath
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(filePath)
	}
	live := make(map[string]struct{})
	var failed []error
	for _, agent := range []parser.AgentType{
		parser.AgentCodebuff, parser.AgentFreebuff,
	} {
		ids, err := e.db.ListSessionIDsByFilePath(ctx, lookupPath, string(agent))
		if err != nil {
			// One retry absorbs a transient read failure.
			ids, err = e.db.ListSessionIDsByFilePath(ctx, lookupPath, string(agent))
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", agent, err))
			continue
		}
		for _, id := range ids {
			live[id] = struct{}{}
		}
	}
	if len(failed) > 0 {
		// Without both lookups the engine could restore a trashed session
		// under its other classification or leave both classifications live.
		res.err = fmt.Errorf(
			"list session IDs by file path %q: %w",
			lookupPath, errors.Join(failed...),
		)
		res.noCacheSkip = true
		res.results = res.results[:0]
		return
	}

	excluded := make(map[string]struct{}, len(res.excludedSessionIDs))
	for _, id := range e.applyIDPrefixToSessionIDs(res.excludedSessionIDs) {
		excluded[id] = struct{}{}
	}
	addExclusion := func(id string) {
		if _, ok := excluded[id]; ok {
			return
		}
		excluded[id] = struct{}{}
		res.excludedSessionIDs = append(res.excludedSessionIDs, id)
	}
	kept := res.results[:0]
	for _, result := range res.results {
		currentID := applyIDPrefixToID(e.idPrefix, result.Session.ID)
		counterpart, ok := e.codebuffCounterpartID(currentID)
		if !ok {
			kept = append(kept, result)
			continue
		}
		if e.db.IsSessionExcluded(ctx, counterpart) ||
			e.db.IsSessionTrashed(ctx, counterpart) {
			// Keep a trashed current ID trashed rather than converting it to
			// a parser deletion; the upsert's trash guard already hides it.
			if !e.db.IsSessionTrashed(ctx, currentID) {
				addExclusion(currentID)
			}
			continue
		}
		if _, stale := live[counterpart]; stale {
			addExclusion(counterpart)
		}
		kept = append(kept, result)
	}
	res.results = kept
}

// claudeSourceMissingSessionOwnershipsForCompleteResult lists stored active
// Claude fork sessions written by an older parser version under this
// transcript's exact stored path that a complete full parse no longer derives
// from it. The stale data version and fork relationship together prove the row
// is a legacy DAG branch artifact rather than a current parser presence
// regression. Such rows can never be re-stamped by re-parsing the file, so
// they pin the path's minimum data_version at a stale value and defeat the
// unchanged-source skip on every sweep. Tombstoning them as source-missing
// lets the skip converge; a later parse that re-emits an ID revives the row.
// Unlike the provider-scope variant above, the lookup is bound to the results'
// own file paths: a Claude source scope must never widen to sibling
// transcripts, whose sessions are legitimately absent from this parse.
func (e *Engine) claudeSourceMissingSessionOwnershipsForCompleteResult(
	ctx context.Context,
	sourcePath string,
	excludedSessionIDs []string,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	present := make(map[string]struct{}, len(results))
	paths := make(map[string]struct{}, 1)
	for _, result := range results {
		if id := applyIDPrefixToID(e.idPrefix, result.Session.ID); id != "" {
			present[id] = struct{}{}
		}
		path := result.Session.File.Path
		if path == "" {
			continue
		}
		if e.pathRewriter != nil {
			path = e.pathRewriter(path)
		}
		paths[path] = struct{}{}
	}
	if len(paths) == 0 && sourcePath != "" {
		paths[e.effectiveSourcePath(sourcePath)] = struct{}{}
	}
	// Excluded IDs are owned by the parser-exclusion deletion path.
	for _, id := range e.applyIDPrefixToSessionIDs(excludedSessionIDs) {
		present[id] = struct{}{}
	}
	if index := e.archiveRebuildIndex; index != nil {
		return index.missingMembers(paths, present), nil
	}
	var members []sourceMissingMember
	var sessionIDs []string
	for path := range paths {
		storedIDs, err := e.db.ListStaleForkSessionIDsByFilePath(ctx,
			path, string(parser.AgentClaude),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list stale claude fork sessions for %s: %w", path, err,
			)
		}
		for _, id := range storedIDs {
			if _, ok := present[id]; ok {
				continue
			}
			members = append(members, sourceMissingMember{
				sessionID: id,
				filePath:  path,
				agent:     parser.AgentClaude,
			})
			sessionIDs = append(sessionIDs, id)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	storedMachines, err := e.db.ListSessionMachinesByID(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list stored claude session machines: %w", err,
		)
	}
	for i := range members {
		members[i].machine = storedMachines[members[i].sessionID]
	}
	return members, nil
}

// archiveRebuildIndex snapshots stale Claude forks and shared-session source
// ownership from the original archive. The archive takes no writes during a
// rebuild, so the snapshot stays exact.
type archiveRebuildIndex struct {
	byPath map[string][]db.SessionSourceOwnership
	// pathRecords holds the archive's session path records for the agents
	// sourceCollisionID covers, keyed by base id, so a rebuild sees the same
	// ownership an ordinary sync reads from the live archive.
	pathRecords map[string][]db.SessionPathRecord
}

func loadArchiveRebuildIndex(ctx context.Context,
	archive *db.DB, collisionAgents []string,
) (*archiveRebuildIndex, error) {
	ownerships, err := archive.ListStaleForkSessionOwnerships(ctx,
		string(parser.AgentClaude),
	)
	if err != nil {
		return nil, err
	}
	records, err := archive.ListSessionPathRecordsForAgents(ctx, collisionAgents)
	if err != nil {
		return nil, err
	}
	index := &archiveRebuildIndex{
		byPath:      make(map[string][]db.SessionSourceOwnership),
		pathRecords: make(map[string][]db.SessionPathRecord),
	}
	for _, record := range records {
		base := parser.BaseSessionID(record.ID)
		index.pathRecords[base] = append(index.pathRecords[base], record)
	}
	for _, ownership := range ownerships {
		index.byPath[ownership.FilePath] = append(
			index.byPath[ownership.FilePath], ownership,
		)
	}
	return index, nil
}

// missingMembers returns the snapshotted stale forks under paths that a
// complete parse did not re-emit, in a stable path-then-ID order.
func (index *archiveRebuildIndex) missingMembers(
	paths map[string]struct{},
	present map[string]struct{},
) []sourceMissingMember {
	var members []sourceMissingMember
	for path := range paths {
		for _, ownership := range index.byPath[path] {
			if _, ok := present[ownership.ID]; ok {
				continue
			}
			members = append(members, sourceMissingMember{
				sessionID: ownership.ID,
				filePath:  path,
				machine:   ownership.Machine,
				agent:     parser.AgentClaude,
			})
		}
	}
	slices.SortFunc(members, func(a, b sourceMissingMember) int {
		if c := strings.Compare(a.filePath, b.filePath); c != 0 {
			return c
		}
		return strings.Compare(a.sessionID, b.sessionID)
	})
	return members
}

// claudeSourceNeedsStaleForkReconciliation reports whether a freshness skip
// would strand an actionable legacy fork on this exact stored source path.
// Forks rejected by the CWD filter are intentionally preserved and do not
// require another parse. A read error still defeats freshness so cleanup can
// retry instead of silently preserving stale data.
func (e *Engine) claudeSourceNeedsStaleForkReconciliation(
	ctx context.Context, path string,
) bool {
	storedIDs, err := e.db.ListStaleForkSessionIDsByFilePath(ctx,
		path, string(parser.AgentClaude),
	)
	if err != nil {
		return true
	}
	for _, id := range storedIDs {
		allowed, err := e.missingMemberTombstoneAllowed(ctx, id)
		if err != nil || allowed {
			return true
		}
	}
	return false
}

// claudeSourceHasOnlyPreservedStaleForks reports whether every active Claude
// row at path is a stale legacy fork that the current CWD filter intentionally
// preserves. Such rows must not keep an otherwise unchanged source permanently
// stale: the filter decision is re-evaluated on every pass, so a later engine
// with a broader filter will resume reconciliation instead of inheriting a
// durable cache exemption. Any lookup error fails closed and reparses.
func (e *Engine) claudeSourceHasOnlyPreservedStaleForks(
	ctx context.Context, path string,
) bool {
	return e.claudeSourceStaleRowsArePreservedForks(ctx, path, false)
}

// claudeSourceStaleRowsArePreservedForks verifies that every stale row under
// path is a legacy fork rejected by the current CWD filter. When
// allowCurrentRows is true, current primary or fork rows may share the source.
func (e *Engine) claudeSourceStaleRowsArePreservedForks(
	ctx context.Context, path string, allowCurrentRows bool,
) bool {
	staleIDs, err := e.db.ListStaleForkSessionIDsByFilePath(ctx,
		path, string(parser.AgentClaude),
	)
	if err != nil || len(staleIDs) == 0 {
		return false
	}
	activeIDs, err := e.db.ListSessionIDsByFilePath(ctx,
		path, string(parser.AgentClaude),
	)
	if err != nil || len(activeIDs) < len(staleIDs) {
		return false
	}
	stale := make(map[string]struct{}, len(staleIDs))
	for _, id := range staleIDs {
		stale[id] = struct{}{}
	}
	for _, id := range activeIDs {
		if _, ok := stale[id]; !ok {
			if !allowCurrentRows {
				return false
			}
			session, err := e.db.GetSession(ctx, id)
			if err != nil || session == nil ||
				session.DataVersion < db.CurrentDataVersion() {
				return false
			}
			continue
		}
		allowed, err := e.missingMemberTombstoneAllowed(ctx, id)
		if err != nil || allowed {
			return false
		}
	}
	return true
}

// claudeSourceFreshWithoutPrimary verifies an unchanged Claude source through
// exact-path metadata when its canonical primary session is absent and only
// CWD-rejected stale forks remain. The stale forks' old data versions and file
// metadata are ignored only when a current-content-, data-version-, and
// CWD-filter-keyed marker proves the parser completed cleanly without producing
// an admitted primary row.
func (e *Engine) claudeSourceFreshWithoutPrimary(
	ctx context.Context,
	lookupPath string,
	physicalPath string,
	info os.FileInfo,
	sourceFingerprint string,
) (fresh, contentVerified bool) {
	if !e.claudeSourceHasOnlyPreservedStaleForks(ctx, lookupPath) {
		return false, false
	}
	project, ok := e.db.GetProjectByAgentPath(ctx,
		lookupPath, string(parser.AgentClaude),
	)
	if !ok || project == "" || parser.NeedsProjectReparse(project) {
		return false, false
	}
	currentHash := sourceFingerprint
	if currentHash == "" {
		var err error
		currentHash, err = e.contentCheckHash(physicalPath, info.Size())
		if err != nil {
			return false, false
		}
	}
	if !e.claudeRowlessFreshnessMarked(
		lookupPath, physicalPath, currentHash, info.ModTime().UnixNano(),
	) {
		return false, false
	}
	return true, true
}

func (e *Engine) claudeRowlessFreshnessCacheKey(
	path, contentHash string,
) string {
	if path == "" || contentHash == "" {
		return ""
	}
	prefixes := append([]string(nil), e.cwdFilter.prefixes...)
	slices.Sort(prefixes)
	filterHash := sha256.Sum256([]byte(strings.Join(prefixes, "\x00")))
	return providerAgentSkipCacheKey(path, parser.AgentClaude) +
		sourceHashSkipMarker + contentHash +
		"&data_version=" + strconv.Itoa(db.AgentDataVersion(parser.AgentClaude)) +
		"&cwd_filter=" + hex.EncodeToString(filterHash[:])
}

func (e *Engine) cacheClaudeRowlessFreshness(
	ctx context.Context, job syncJob,
) bool {
	if job.claudeRowlessFreshnessKey == "" || job.mtime == 0 ||
		job.noCacheSkip {
		return false
	}
	if !e.claudeSourceHasOnlyPreservedStaleForks(
		ctx, e.effectiveSourcePath(job.path),
	) {
		return false
	}
	e.cacheSkip(
		job.claudeRowlessFreshnessKey, job.mtime, job.sourceFingerprint,
	)
	return true
}

func (e *Engine) claudeRowlessFreshnessMarked(
	lookupPath, physicalPath, contentHash string,
	mtime int64,
) bool {
	paths := []string{lookupPath}
	if physicalPath != lookupPath {
		paths = append(paths, physicalPath)
	}
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	for _, path := range paths {
		key := e.claudeRowlessFreshnessCacheKey(path, contentHash)
		cachedMtime, cached := e.skipCache[key]
		if key != "" && cached && cachedMtime == mtime {
			return true
		}
	}
	return false
}
