package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// cwdPrefixFilter gates session ingestion on the session working
// directory. An empty filter allows everything. A non-empty filter
// allows only sessions whose cwd equals a configured prefix or lives
// underneath one; sessions without a recorded cwd are rejected
// because they cannot be attributed to any workspace.
//
// Prefixes and cwds are lexically cleaned before matching and the
// path boundary is the local OS separator. The filter only ever sees
// local paths (remote sync does not apply it), so local filesystem
// semantics are the correct ones: on POSIX a backslash is an ordinary
// filename character, not a boundary, and a cwd like "/a/b/../c" is
// resolved to "/a/c" rather than prefix-matching "/a/b".
type cwdPrefixFilter struct {
	prefixes []string
}

// newCwdPrefixFilter normalizes the configured prefixes: entries are
// trimmed, blank entries are dropped, and each remaining entry is
// cleaned with filepath.Clean so "/a/b/" and "/a/b" behave
// identically and ".." components cannot linger in a prefix.
func newCwdPrefixFilter(prefixes []string) cwdPrefixFilter {
	normalized := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		normalized = append(normalized, filepath.Clean(p))
	}
	return cwdPrefixFilter{prefixes: normalized}
}

func (f cwdPrefixFilter) empty() bool {
	return len(f.prefixes) == 0
}

// allows reports whether a session with the given cwd may be
// ingested. Matching is path-boundary aware: prefix "/a/b" matches
// "/a/b" and "/a/b/c" but not "/a/bc".
func (f cwdPrefixFilter) allows(cwd string) bool {
	if f.empty() {
		return true
	}
	if cwd == "" {
		return false
	}
	cwd = filepath.Clean(cwd)
	sep := string(os.PathSeparator)
	for _, p := range f.prefixes {
		if cwd == p {
			return true
		}
		prefix := p
		if !strings.HasSuffix(prefix, sep) {
			prefix += sep
		}
		if strings.HasPrefix(cwd, prefix) {
			return true
		}
	}
	return false
}

// sourceAllowsParserExclusions reports whether a source's parser
// exclusions (including engine stale-row cleanup) may delete archived
// rows. When the cwd allow-list is active, a source proves it is
// inside the list by producing at least one allowed session or
// incremental update; a source with no allowed output is frozen — its
// exclusions would erase archived sessions whose replacement writes
// the filter vetoes, which the ingestion-only contract forbids.
// Zero-result exclusion carriers (e.g. a file that parses to no live
// session) have no cwd to judge, so they are frozen too.
func (e *Engine) sourceAllowsParserExclusions(res processResult) bool {
	if e.cwdFilter.empty() {
		return true
	}
	if res.incremental != nil && e.cwdFilter.allows(res.incremental.cwd) {
		return true
	}
	for _, pr := range res.results {
		if e.cwdFilter.allows(pr.Session.Cwd) {
			return true
		}
	}
	return false
}

// missingMemberTombstoneAllowed reports whether the cwd allow-list
// admits retiring one vanished shared-container member. The member has
// no parsed result to judge — its source row is gone — so the decision
// uses the archived session's cwd instead of the container's surviving
// results: source-wide gating would let one allowed survivor unfreeze
// members outside the list, and an all-unchanged (dropped) survivor set
// would freeze an allowed member's deletion forever once the container
// fingerprint is cached. A missing archive row skips harmlessly (there
// is nothing to retire); a read error propagates so the container pass
// fails and retries.
func (e *Engine) missingMemberTombstoneAllowed(
	ctx context.Context, sessionID string,
) (bool, error) {
	if e.cwdFilter.empty() {
		return true, nil
	}
	store := db.Store(e.db)
	if e.archiveStore != nil {
		store = e.archiveStore
	}
	sess, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf(
			"read archived cwd for missing member %s: %w", sessionID, err,
		)
	}
	if sess == nil {
		return false, nil
	}
	return e.cwdFilter.allows(sess.Cwd), nil
}

// splitResultsByCwdFilter returns the parsed sessions the cwd
// allow-list admits and the number it vetoes. With no filter
// configured it returns the input untouched.
func (e *Engine) splitResultsByCwdFilter(
	results []parser.ParseResult, sourceCwd sourceCwdDecision,
) ([]parser.ParseResult, int) {
	if e.cwdFilter.empty() || len(results) == 0 {
		return results, 0
	}
	allowed := make([]parser.ParseResult, 0, len(results))
	for _, pr := range results {
		if e.cwdFilter.allows(sourceCwdForFilter(
			pr.Session.Cwd, sourceCwd,
		)) {
			allowed = append(allowed, pr)
		}
	}
	return allowed, len(results) - len(allowed)
}

type sourceCwdPathKey struct {
	path  string
	agent string
}

type sourceCwdSessionKey struct {
	id    string
	path  string
	agent string
}

type sourceCwdReconciliationBatch struct {
	mu       gosync.Mutex
	sessions map[sourceCwdSessionKey]string
	paths    map[sourceCwdPathKey]string
}

func newSourceCwdReconciliationBatch() *sourceCwdReconciliationBatch {
	return &sourceCwdReconciliationBatch{
		sessions: make(map[sourceCwdSessionKey]string),
		paths:    make(map[sourceCwdPathKey]string),
	}
}

type sourceCwdDecision struct {
	resolution parser.SourceCwdResolution
	storedCwd  string
	storedOK   bool
	forceParse bool
}

type sourceCwdReader interface {
	GetCwdByAgentPath(ctx context.Context, path, agent string) (string, bool)
}

// sourceCwdDecision compares provider-owned Cwd authority with the archive row
// before any generic freshness gate can discard the source.
func (e *Engine) sourceCwdDecision(ctx context.Context,
	source parser.SourceRef,
) sourceCwdDecision {
	resolution := source.CwdResolution
	decision := sourceCwdDecision{resolution: resolution}
	if resolution.State == parser.SourceCwdUnspecified || e.db == nil {
		return decision
	}
	path := e.sourceCwdLookupPath(source)
	reader := sourceCwdReader(e.db)
	if e.archiveStore != nil {
		if archived, ok := e.archiveStore.(sourceCwdReader); ok {
			reader = archived
		}
	}
	decision.storedCwd, decision.storedOK = reader.GetCwdByAgentPath(ctx,
		path, string(source.Provider),
	)
	switch resolution.State {
	case parser.SourceCwdResolved:
		decision.forceParse = decision.storedOK &&
			decision.storedCwd != resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		decision.forceParse = decision.storedOK && decision.storedCwd != ""
	case parser.SourceCwdUnspecified, parser.SourceCwdUnavailable:
		// Without source authority, keep the stored working directory.
	}
	return decision
}

func (e *Engine) sourceCwdLookupPath(source parser.SourceRef) string {
	path := providerDiscoveredPath(source)
	if path == "" {
		path = source.FingerprintKey
	}
	if e.pathRewriter != nil {
		path = e.pathRewriter(path)
	}
	return path
}

func sourceCwdParticipates(resolution parser.SourceCwdResolution) bool {
	return resolution.State != parser.SourceCwdUnspecified
}

func sourceCwdForFilter(parsed string, decision sourceCwdDecision) string {
	switch decision.resolution.State {
	case parser.SourceCwdResolved:
		return decision.resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		return ""
	case parser.SourceCwdUnavailable:
		if decision.storedOK && decision.storedCwd != "" {
			return decision.storedCwd
		}
	case parser.SourceCwdUnspecified:
		return parsed
	}
	return parsed
}

func (e *Engine) reconcileFilteredSourceCwd(ctx context.Context,
	results []parser.ParseResult, decision sourceCwdDecision,
) (bool, error) {
	if e.cwdFilter.empty() || !sourceCwdParticipates(decision.resolution) {
		return false, nil
	}
	target := sourceCwdForFilter("", decision)
	changed := false
	for _, result := range results {
		if e.cwdFilter.allows(sourceCwdForFilter(
			result.Session.Cwd, decision,
		)) {
			continue
		}
		id := applyIDPrefixToID(e.idPrefix, result.Session.ID)
		agent := string(result.Session.Agent)
		path := e.effectiveSourcePath(result.Session.File.Path)
		if id == "" || agent == "" || path == "" {
			continue
		}
		key := sourceCwdSessionKey{id: id, path: path, agent: agent}
		if e.deferredSourceCwd != nil {
			e.deferredSourceCwd.mu.Lock()
			e.deferredSourceCwd.sessions[key] = target
			e.deferredSourceCwd.mu.Unlock()
		} else {
			updated, err := e.db.UpdateSessionCwdByIdentity(ctx,
				id, path, agent, target,
			)
			if err != nil {
				return changed, err
			}
			changed = changed || updated
		}
	}
	return changed, nil
}

func (e *Engine) reconcileSourceCwdByPath(ctx context.Context,
	source parser.SourceRef, decision sourceCwdDecision,
) (bool, error) {
	changed, err := e.reconcileSourceCwdAtPath(ctx,
		e.sourceCwdLookupPath(source), source.Provider, decision,
	)
	return changed, err
}

func (e *Engine) reconcileSourceCwdAtPath(ctx context.Context,
	path string, agent parser.AgentType, decision sourceCwdDecision,
) (bool, error) {
	if !sourceCwdParticipates(decision.resolution) ||
		path == "" || e.db == nil {
		return false, nil
	}
	target := sourceCwdForFilter("", decision)
	// The decision already captured the stored Cwd for this source under the
	// same locked pass, so a second per-source point query is redundant.
	if !decision.storedOK || decision.storedCwd == target {
		return false, nil
	}
	if e.deferredSourceCwd != nil {
		e.deferredSourceCwd.mu.Lock()
		e.deferredSourceCwd.paths[sourceCwdPathKey{
			path: path, agent: string(agent),
		}] = target
		e.deferredSourceCwd.mu.Unlock()
	} else {
		updated, err := e.db.UpdateCwdByAgentPathCount(ctx,
			path, string(agent), target,
		)
		if err != nil {
			return false, err
		}
		return updated > 0, nil
	}
	return true, nil
}

func (e *Engine) reconcileSkippedSourceCwd(ctx context.Context, r syncJob) (bool, error) {
	if r.sourceCwdResolution.State == parser.SourceCwdUnspecified {
		return false, nil
	}
	return e.reconcileSourceCwdAtPath(ctx,
		r.sourceCwdPath,
		r.sourceCwdAgent,
		sourceCwdDecision{
			resolution: r.sourceCwdResolution,
			storedCwd:  r.sourceCwdStored,
			storedOK:   r.sourceCwdStoredOK,
		},
	)
}

func (e *Engine) applyDeferredSourceCwd(ctx context.Context,
	target *db.DB, batch *sourceCwdReconciliationBatch,
) (int, error) {
	if batch == nil {
		return 0, nil
	}
	batch.mu.Lock()
	defer batch.mu.Unlock()
	updated := 0
	for key, cwd := range batch.sessions {
		rowUpdated, err := target.UpdateSessionCwdByIdentity(ctx,
			key.id, key.path, key.agent, cwd,
		)
		if err != nil {
			return updated, err
		}
		if rowUpdated {
			updated++
		}
	}
	for key, cwd := range batch.paths {
		count, err := target.UpdateCwdByAgentPathCount(ctx,
			key.path, key.agent, cwd,
		)
		if err != nil {
			return updated, err
		}
		updated += count
	}
	return updated, nil
}

func applySourceCwdResolution(
	s *db.Session, resolution parser.SourceCwdResolution,
	storedCwd string, storedOK bool,
) {
	switch resolution.State {
	case parser.SourceCwdResolved:
		s.Cwd = resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		s.Cwd = ""
	case parser.SourceCwdUnavailable:
		if storedOK && storedCwd != "" {
			s.Cwd = storedCwd
		}
	case parser.SourceCwdUnspecified:
		// This source does not override the parsed working directory.
	}
}
