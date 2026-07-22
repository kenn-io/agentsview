package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var _ Provider = (*hermesProvider)(nil)

type hermesProviderFactory struct {
	def AgentDef
}

func newHermesProviderFactory(def AgentDef) ProviderFactory {
	return hermesProviderFactory{def: cloneAgentDef(def)}
}

func (f hermesProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f hermesProviderFactory) Capabilities() Capabilities {
	return hermesProviderCapabilities()
}

func (f hermesProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &hermesProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   hermesProviderCapabilities(),
			Config: cfg,
		},
		sources: newHermesSourceSet(cfg.Roots),
	}
}

type hermesProvider struct {
	ProviderBase
	sources hermesSourceSet
}

func (p *hermesProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *hermesProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *hermesProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *hermesProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *hermesProvider) ReconciliationOwnershipScopes(
	root string,
) []StoredSourceHintScope {
	return p.sources.ReconciliationOwnershipScopes(root)
}

func (p *hermesProvider) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	return p.sources.SourceForReconciliation(ctx, path, project)
}

// ReconciliationAggregateMemberPaths maps an aggregate-owned state.db row to
// the sources streamed discovery emits for that member: the virtual
// state.db#<id> path for state-backed sessions and both transcript file
// shapes for transcript-backed ones. Container discovery stamps every
// session with the container path itself (see Parse), which always exists,
// so a removed member would otherwise never trip the missing-path check.
func (p *hermesProvider) ReconciliationAggregateMemberPaths(
	path, fullSessionID string,
) []string {
	path = filepath.Clean(path)
	if filepath.Base(path) != "state.db" {
		return nil
	}
	rawID := strings.TrimPrefix(fullSessionID, "hermes:")
	if rawID == fullSessionID || !IsValidSessionID(rawID) {
		return nil
	}
	sessionsDir := filepath.Join(filepath.Dir(path), "sessions")
	return []string{
		VirtualSourcePath(path, rawID),
		filepath.Join(sessionsDir, rawID+".jsonl"),
		filepath.Join(sessionsDir, "session_"+rawID+".json"),
	}
}

func (p *hermesProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *hermesProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func WriteHermesSessionJSONL(
	w io.Writer, storedPath string, roots []string, rawSessionID string,
) error {
	var storedStateErr error
	if path := ResolveSourceFilePath(storedPath); filepath.Base(path) == "state.db" {
		if _, err := os.Stat(path); err == nil {
			err = writeHermesStateSessionJSONL(w, path, rawSessionID)
			if err == nil {
				return nil
			}
			var lookupErr hermesStateLookupError
			if !errors.Is(err, os.ErrNotExist) &&
				!errors.As(err, &lookupErr) {
				return err
			}
			storedStateErr = err
		}
		if transcript := findHermesSourceFile(
			filepath.Join(filepath.Dir(path), "sessions"),
			rawSessionID,
		); transcript != "" {
			return copyHermesTranscriptFile(w, transcript)
		}
	}
	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: roots})
	if !ok {
		return fmt.Errorf("hermes provider unavailable")
	}
	hp, ok := provider.(*hermesProvider)
	if !ok {
		return fmt.Errorf("hermes provider unavailable")
	}
	source, found, err := hp.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: rawSessionID},
	)
	if err != nil {
		return err
	}
	if !found {
		if storedStateErr != nil && !errors.Is(storedStateErr, os.ErrNotExist) {
			return storedStateErr
		}
		return fmt.Errorf(
			"hermes session %s source not found: %w",
			rawSessionID, os.ErrNotExist,
		)
	}
	src, ok := hp.sources.sourceFromRef(source)
	if !ok {
		return fmt.Errorf("hermes source path unavailable")
	}
	if src.StateDB != "" && src.SessionID != "" {
		return writeHermesStateSessionJSONL(w, src.StateDB, src.SessionID)
	}
	if filepath.Base(src.Path) == "state.db" {
		return writeHermesStateSessionJSONL(w, src.Path, rawSessionID)
	}
	return copyHermesTranscriptFile(w, src.Path)
}

func (p *hermesProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("hermes source path unavailable")
	}
	path := src.Path
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	if src.SessionID != "" {
		return p.parseStateMember(ctx, src, req.Source.ProjectHint, machine, req.Fingerprint)
	}
	if filepath.Base(path) == "state.db" {
		results, err := p.parseArchive(path, req.Source.ProjectHint, machine)
		if err != nil {
			return ParseOutcome{}, err
		}
		// Mirror the legacy engine's stampHermesArchiveResults: every archive
		// session's stored file identity is the state.db path with the
		// aggregate (state.db plus transcripts) size and mtime, so a
		// transcript-only change still refreshes the archive's freshness.
		size, mtime := hermesArchiveEffectiveFileInfo(path)
		out := make([]ParseResultOutcome, 0, len(results))
		for i := range results {
			results[i].Session.File.Path = path
			results[i].Session.File.Size = size
			results[i].Session.File.Mtime = mtime
			out = append(out, ParseResultOutcome{
				Result:      results[i],
				DataVersion: DataVersionCurrent,
			})
		}
		return ParseOutcome{
			Results:           out,
			ResultSetComplete: true,
			ForceReplace:      true,
		}, nil
	}

	sess, msgs, err := p.parseSession(path, req.Source.ProjectHint, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:  *sess,
				Messages: msgs,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

func (p *hermesProvider) parseStateMember(
	ctx context.Context,
	src hermesSource, project, machine string, fingerprint SourceFingerprint,
) (ParseOutcome, error) {
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(src.StateDB)+"?mode=ro")
	if err != nil {
		return ParseOutcome{}, fmt.Errorf("open hermes state db: %w", err)
	}
	defer conn.Close()
	observeSharedContainerScan(ctx)
	ss, found, err := readHermesStateSession(conn, src.SessionID)
	if err != nil {
		return ParseOutcome{}, err
	}
	if !found {
		return ParseOutcome{ResultSetComplete: true, ForceReplace: true, SkipReason: SkipNoSession}, nil
	}
	messages, err := readHermesStateMessagesForSession(conn, src.SessionID)
	if err != nil {
		return ParseOutcome{}, err
	}
	result, ok := buildHermesStateResult(
		ss, messages, filepath.Join(filepath.Dir(src.StateDB), "sessions"),
		src.StateDB, project, machine,
	)
	if !ok {
		return ParseOutcome{ResultSetComplete: true, ForceReplace: true, SkipReason: SkipNoSession}, nil
	}
	result.Session.File.Path = src.Path
	if fingerprint.Hash != "" {
		result.Session.File.Hash = fingerprint.Hash
	}
	// Store the fingerprint's freshness identity (state.db plus selected
	// transcript) so the engine's stored size+mtime skip recognizes an
	// unchanged member while the container is untouched. The stat is shared
	// by every member, so after any sibling change the engine falls back to
	// the per-member content hash stored above to keep unchanged members
	// from reparsing (providerSourceHashFreshDespiteStat); without either
	// identity every pass reparses every member and reconciliation work
	// scales with total member count.
	if fingerprint.Size > 0 {
		result.Session.File.Size = fingerprint.Size
	}
	if fingerprint.MTimeNS > 0 {
		result.Session.File.Mtime = fingerprint.MTimeNS
	}
	return ParseOutcome{
		Results:           []ParseResultOutcome{{Result: result, DataVersion: DataVersionCurrent}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type hermesSource struct {
	Root      string
	Path      string
	StateDB   string
	SessionID string
}

type hermesSourceSet struct {
	roots []string
}

func newHermesSourceSet(roots []string) hermesSourceSet {
	return hermesSourceSet{roots: cleanJSONLRoots(roots)}
}

// The default Hermes roots include the stable profiles container rather than
// its current children. Expanding direct children during discovery lets a
// long-running provider see profiles created after initialization.
func isHermesProfilesContainer(root string) bool {
	root = filepath.Clean(root)
	return filepath.Base(root) == "profiles" &&
		filepath.Base(filepath.Dir(root)) == ".hermes"
}

// hermesProfileArchiveRoots expands a profiles container into its current
// per-profile archive roots. A missing container simply means no profiles
// exist yet; any other ReadDir failure (permissions, transient I/O) is
// returned so callers report the expansion as incomplete discovery instead
// of silently claiming zero profiles — ReconciliationOwnershipScopes still
// claims the whole container, so a swallowed failure would let the engine
// tombstone every stored hermes session under it as source_missing.
func hermesProfileArchiveRoots(profilesRoot string) ([]string, error) {
	entries, err := os.ReadDir(profilesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"expand hermes profiles container %s: %w", profilesRoot, err,
		)
	}
	roots := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		roots = append(roots, filepath.Join(profilesRoot, entry.Name()))
	}
	return roots, nil
}

// expandedRoots returns every discoverable archive root, expanding profiles
// containers into their children. The returned roots always include every
// root that did expand; a non-nil error joins the container expansion
// failures so callers can act on the healthy subset while reporting the
// failure as incomplete discovery rather than an authoritative empty scope.
func (s hermesSourceSet) expandedRoots() ([]string, error) {
	var roots []string
	var expandErr error
	for _, root := range s.roots {
		if isHermesProfilesContainer(root) {
			profileRoots, err := hermesProfileArchiveRoots(root)
			if err != nil {
				expandErr = errors.Join(expandErr, err)
			}
			roots = append(roots, profileRoots...)
			continue
		}
		roots = append(roots, root)
	}
	return roots, expandErr
}

// incompleteProfileExpansionError wraps a profiles-container expansion
// failure in the DiscoveryIncompleteError taxonomy so the sync engine
// retains reconciliation markers and retries instead of tombstoning.
func incompleteProfileExpansionError(expandErr error) error {
	return incompleteDiscoveryError(
		AgentHermes, "expand profiles container", expandErr,
	)
}

func hermesProfileRootForPath(profilesRoot, changedPath string) (string, bool) {
	profilesRoot = filepath.Clean(profilesRoot)
	changedPath = filepath.Clean(changedPath)
	rel, err := filepath.Rel(profilesRoot, changedPath)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	name := rel
	if before, _, ok := strings.Cut(rel, string(filepath.Separator)); ok {
		name = before
	}
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	return filepath.Join(profilesRoot, name), true
}

func (s hermesSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	roots, expandErr := s.expandedRoots()
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, file := range discoverHermesSessions(root) {
			source, ok := s.sourceRef(root, file.Path)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	if expandErr != nil {
		return sources, incompleteProfileExpansionError(expandErr)
	}
	return sources, nil
}

func (s hermesSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	var discoveryErr error
	appendDiscoveryErr := func(err error) {
		err = incompleteDiscoveryError(
			AgentHermes, "stream configured root", err,
		)
		if discoveryErr == nil {
			discoveryErr = err
			return
		}
		discoveryErr = errors.Join(discoveryErr, err)
	}
	discoverTranscripts := func(root, sessionsDir, stateDB string) error {
		return s.discoverTranscriptEach(
			ctx, root, sessionsDir, stateDB, func(source SourceRef) error {
				if err := yield(source); err != nil {
					return discoveryYieldError{cause: err}
				}
				return nil
			},
		)
	}
	roots, expandErr := s.expandedRoots()
	if expandErr != nil {
		appendDiscoveryErr(expandErr)
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stateDB, sessionsDir, ok := hermesStatePaths(root); ok {
			yieldedAny, err := s.discoverStateEach(ctx, root, stateDB, yield)
			if err != nil {
				if cause, ok := discoveryYieldCause(err); ok {
					return cause
				}
				if yieldedAny || ctx.Err() != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return ctxErr
					}
					appendDiscoveryErr(err)
					continue
				}
				log.Printf(
					"hermes: state db discovery failed for %s: %v; "+
						"falling back to transcripts",
					stateDB, err,
				)
				stateErr := err
				fallbackErr := discoverTranscripts(root, sessionsDir, "")
				if cause, ok := discoveryYieldCause(fallbackErr); ok {
					return cause
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				appendDiscoveryErr(stateErr)
				if fallbackErr != nil {
					appendDiscoveryErr(fallbackErr)
				}
				continue
			}
			if err := discoverTranscripts(root, sessionsDir, stateDB); err != nil {
				if cause, ok := discoveryYieldCause(err); ok {
					return cause
				}
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				appendDiscoveryErr(err)
				continue
			}
			continue
		}
		transcriptRoot := hermesTranscriptRoot(root)
		if err := discoverTranscripts(root, transcriptRoot, ""); err != nil {
			if cause, ok := discoveryYieldCause(err); ok {
				return cause
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			appendDiscoveryErr(err)
			continue
		}
	}
	return discoveryErr
}

func (s hermesSourceSet) discoverStateEach(
	ctx context.Context, root, stateDB string, yield func(SourceRef) error,
) (bool, error) {
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("open hermes state db: %w", err)
	}
	defer conn.Close()
	observeSharedContainerScan(ctx)
	rows, err := conn.QueryContext(ctx, "SELECT id FROM sessions ORDER BY id")
	if err != nil {
		return false, fmt.Errorf("query hermes sessions: %w", err)
	}
	defer rows.Close()
	yieldedAny := false
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return yieldedAny, fmt.Errorf("scan hermes session id: %w", err)
		}
		if !IsValidSessionID(id) {
			continue
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		yieldedAny = true
		if err := yield(hermesStateMemberSourceRef(root, stateDB, id)); err != nil {
			return yieldedAny, discoveryYieldError{cause: err}
		}
	}
	return yieldedAny, rows.Err()
}

func (s hermesSourceSet) discoverTranscriptEach(
	ctx context.Context, root, sessionsDir, stateDB string,
	yield func(SourceRef) error,
) error {
	// One read-only connection and prepared statement serve every membership
	// check in the pass; opening state.db per transcript file would scale
	// reconciliation work with archive size instead of the changed batch.
	// Opened lazily so transcript-free directories cost nothing.
	var membership *hermesStateMembership
	defer func() {
		if membership != nil {
			membership.Close()
		}
	}()
	return streamDirectoryEntries(ctx, sessionsDir, func(entry os.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		id := HermesSessionID(name)
		if !IsValidSessionID(id) {
			return nil
		}
		switch {
		case strings.HasSuffix(name, ".jsonl"):
		case strings.HasPrefix(name, "session_") && strings.HasSuffix(name, ".json"):
			if IsRegularFile(filepath.Join(sessionsDir, id+".jsonl")) {
				return nil
			}
		default:
			return nil
		}
		if stateDB != "" {
			if membership == nil {
				opened, err := openHermesStateMembership(ctx, stateDB)
				if err != nil {
					return err
				}
				membership = opened
			}
			found, err := membership.Has(id)
			if err != nil {
				return err
			}
			if found {
				return nil
			}
		}
		ref, ok := hermesTranscriptSourceRef(root, filepath.Join(sessionsDir, name))
		if !ok {
			return nil
		}
		return yield(ref)
	})
}

// hermesStateMembership answers "does state.db hold this session id" through
// one read-only connection and prepared statement for a whole discovery pass.
type hermesStateMembership struct {
	conn *sql.DB
	stmt *sql.Stmt
}

func openHermesStateMembership(
	ctx context.Context, stateDB string,
) (*hermesStateMembership, error) {
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open hermes state db: %w", err)
	}
	observeSharedContainerScan(ctx)
	stmt, err := conn.Prepare("SELECT 1 FROM sessions WHERE id = ? LIMIT 1")
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("prepare hermes session lookup: %w", err)
	}
	return &hermesStateMembership{conn: conn, stmt: stmt}, nil
}

func (m *hermesStateMembership) Has(rawID string) (bool, error) {
	var found int
	err := m.stmt.QueryRow(rawID).Scan(&found)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("query hermes session %s: %w", rawID, err)
}

func (m *hermesStateMembership) Close() {
	m.stmt.Close()
	m.conn.Close()
}

func (s hermesSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		if isHermesProfilesContainer(root) {
			roots = append(roots, WatchRoot{
				Path:         root,
				Recursive:    true,
				IncludeGlobs: []string{"state.db", "state.db-wal", "*.jsonl", "session_*.json"},
				DebounceKey:  string(AgentHermes) + ":profiles:" + root,
			})
			continue
		}
		roots = append(roots, hermesWatchRoots(root)...)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s hermesSourceSet) ReconciliationOwnershipScopes(
	requestedRoot string,
) []StoredSourceHintScope {
	requestedRoot = filepath.Clean(requestedRoot)
	requestedMatchRoot := absoluteHermesPath(requestedRoot)
	var scopes []StoredSourceHintScope
	for _, configuredRoot := range s.roots {
		if !hermesPathWithinOrSame(
			absoluteHermesPath(configuredRoot), requestedMatchRoot,
		) {
			continue
		}
		stateDB, sessionsDir, ok := hermesArchiveRootPaths(configuredRoot)
		if !ok {
			scopes = append(scopes, StoredSourceHintScope{Path: configuredRoot})
			continue
		}
		if IsRegularFile(stateDB) {
			scopes = append(scopes, StoredSourceHintScope{
				Path: stateDB, IncludeVirtualMembers: true,
			})
		}
		scopes = append(scopes, StoredSourceHintScope{Path: sessionsDir})
	}
	return scopes
}

func (s hermesSourceSet) SourceForReconciliation(
	ctx context.Context, path, project string,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	path = filepath.Clean(path)
	roots, expandErr := s.expandedRoots()
	for _, root := range roots {
		var source SourceRef
		var ok bool
		if _, _, virtual := ParseVirtualSourcePathForBase(path, "state.db"); virtual {
			source, ok = s.sourceForChangedPath(root, path, false)
		} else if stateDB, sessionsDir, archive := hermesStatePaths(root); archive {
			switch {
			case samePath(path, stateDB):
				source, ok = hermesArchiveSourceRef(root, stateDB)
			case hermesPathInTranscriptDir(sessionsDir, path) && IsRegularFile(path):
				source, ok = hermesTranscriptSourceRef(root, path)
			}
		} else {
			source, ok = s.sourceRef(root, path)
		}
		if !ok {
			continue
		}
		if project != "" {
			source.ProjectHint = project
		}
		return source, true, nil
	}
	if expandErr != nil {
		// Not-found under a failed container expansion is not authoritative:
		// the engine tombstones sessions whose reconciliation source cannot
		// be resolved, so surface the failure and let it retry instead.
		return SourceRef{}, false, incompleteProfileExpansionError(expandErr)
	}
	return SourceRef{}, false, nil
}

func absoluteHermesPath(path string) string {
	cleaned := filepath.Clean(path)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	return abs
}

func hermesPathWithinOrSame(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s hermesSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allowMissing := jsonlMissingPathFallbackAllowed(req)
	if req.WatchRoot != "" {
		watchRoot := filepath.Clean(req.WatchRoot)
		for _, root := range s.roots {
			if isHermesProfilesContainer(root) && samePath(root, watchRoot) {
				profileRoot, ok := hermesProfileRootForPath(root, req.Path)
				if !ok {
					return nil, nil
				}
				source, ok := s.sourceForChangedPath(
					profileRoot, req.Path, allowMissing,
				)
				if ok {
					return []SourceRef{source}, nil
				}
				return nil, nil
			}
			if !hermesWatchRootMatches(root, watchRoot) {
				continue
			}
			source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
			if ok {
				return []SourceRef{source}, nil
			}
		}
		return nil, nil
	}
	for _, root := range s.roots {
		if isHermesProfilesContainer(root) {
			profileRoot, ok := hermesProfileRootForPath(root, req.Path)
			if !ok {
				continue
			}
			source, ok := s.sourceForChangedPath(
				profileRoot, req.Path, allowMissing,
			)
			if ok {
				return []SourceRef{source}, nil
			}
			continue
		}
		source, ok := s.sourceForChangedPath(root, req.Path, allowMissing)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s hermesSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	roots, expandErr := s.expandedRoots()
	// Not-found is only authoritative when every profiles container
	// expanded; otherwise return the expansion failure so callers retry
	// instead of treating the source as gone.
	notFound := func() (SourceRef, bool, error) {
		if expandErr != nil {
			return SourceRef{}, false, incompleteProfileExpansionError(expandErr)
		}
		return SourceRef{}, false, nil
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range roots {
			if source, ok := s.sourceForPath(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return notFound()
	}
	for _, root := range roots {
		if stateDB, _, ok := hermesStatePaths(root); ok &&
			IsValidSessionID(req.RawSessionID) {
			found, err := hermesStateDBHasSession(stateDB, req.RawSessionID)
			switch {
			case err != nil:
				// Mirror parseArchive: an unreadable or schema-incompatible
				// state.db falls back to transcripts rather than aborting the
				// lookup, so a valid transcript session next to a bad state.db
				// stays resolvable for resync.
				log.Printf(
					"hermes: state db lookup failed for %s: %v; "+
						"falling back to transcripts", stateDB, err,
				)
			case !found:
			default:
				return hermesStateMemberSourceRef(root, stateDB, req.RawSessionID), true, nil
			}
		}
		transcriptRoot := hermesTranscriptRoot(root)
		path := findHermesSourceFile(transcriptRoot, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return notFound()
}

type hermesStateLookupError struct {
	err error
}

func (e hermesStateLookupError) Error() string {
	return e.err.Error()
}

func (e hermesStateLookupError) Unwrap() error {
	return e.err
}

func hermesStateDBHasSession(stateDB string, rawID string) (bool, error) {
	conn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("open hermes state db: %w", err)
	}
	defer conn.Close()

	var found int
	err = conn.QueryRow(
		"SELECT 1 FROM sessions WHERE id = ? LIMIT 1",
		rawID,
	).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("query hermes session %s: %w", rawID, err)
}

func (s hermesSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("hermes source path unavailable")
	}
	path := src.Path
	if src.SessionID != "" {
		return hermesStateMemberFingerprint(ctx, source, src)
	}
	if filepath.Base(path) == "state.db" {
		return hermesArchiveFingerprint(source, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash, err := hashJSONLSourceFile(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s hermesSourceSet) sourceFromRef(source SourceRef) (hermesSource, bool) {
	switch src := source.Opaque.(type) {
	case hermesSource:
		return src, src.Path != ""
	case *hermesSource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	// Expansion failures are not reportable through this boolean seam;
	// callers (Fingerprint, Parse) already turn "not found" into an error
	// the sync engine retries, so search only the roots that expanded.
	roots, _ := s.expandedRoots()
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		for _, root := range roots {
			if ref, ok := s.sourceForPath(root, candidate); ok {
				src := ref.Opaque.(hermesSource)
				return src, true
			}
		}
	}
	return hermesSource{}, false
}

func (s hermesSourceSet) sourceForPath(root, path string) (SourceRef, bool) {
	return s.sourceForChangedPath(root, path, false)
}

func (s hermesSourceSet) sourceForChangedPath(
	root,
	path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if stateDB, sessionID, ok := ParseVirtualSourcePathForBase(path, "state.db"); ok {
		if expected, _, valid := hermesArchivePathsForEvent(root, stateDB); valid &&
			samePath(expected, stateDB) && IsValidSessionID(sessionID) {
			return hermesStateMemberSourceRef(root, stateDB, sessionID), true
		}
		return SourceRef{}, false
	}
	if stateDB, sessionsDir, ok := hermesStatePaths(root); ok {
		if hermesStatePathAffectsArchive(path, stateDB) ||
			hermesPathInTranscriptDir(sessionsDir, path) {
			return hermesArchiveSourceRef(root, stateDB)
		}
		return SourceRef{}, false
	}
	if allowMissing {
		if stateDB, sessionsDir, ok := hermesArchivePathsForEvent(root, path); ok &&
			(hermesStatePathAffectsArchive(path, stateDB) ||
				hermesPathInTranscriptDir(sessionsDir, path)) {
			return hermesArchiveSourceRef(root, stateDB)
		}
		transcriptRoot := hermesTranscriptRoot(root)
		if hermesPathInTranscriptDir(transcriptRoot, path) {
			return hermesTranscriptSourceRef(root, path)
		}
	}
	return s.sourceRef(root, path)
}

func hermesStatePathAffectsArchive(path, stateDB string) bool {
	return samePath(path, stateDB) || samePath(path, stateDB+"-wal")
}

func (s hermesSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if stateDB, _, ok := hermesStatePaths(root); ok && samePath(path, stateDB) {
		return hermesArchiveSourceRef(root, stateDB)
	}
	transcriptRoot := hermesTranscriptRoot(root)
	if !hermesPathInTranscriptDir(transcriptRoot, path) || !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return hermesTranscriptSourceRef(root, path)
}

func hermesArchiveSourceRef(root, stateDB string) (SourceRef, bool) {
	root = filepath.Clean(root)
	stateDB = filepath.Clean(stateDB)
	return SourceRef{
		Provider:       AgentHermes,
		Key:            stateDB,
		DisplayPath:    stateDB,
		FingerprintKey: stateDB,
		Opaque: hermesSource{
			Root: root,
			Path: stateDB,
		},
	}, true
}

func hermesStateMemberSourceRef(root, stateDB, sessionID string) SourceRef {
	path := VirtualSourcePath(stateDB, sessionID)
	return SourceRef{
		Provider: AgentHermes, Key: path, DisplayPath: path, FingerprintKey: path,
		Opaque: hermesSource{Root: filepath.Clean(root), Path: path,
			StateDB: filepath.Clean(stateDB), SessionID: sessionID},
	}
}

func hermesTranscriptSourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return SourceRef{
		Provider:       AgentHermes,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: hermesSource{
			Root: root,
			Path: path,
		},
	}, true
}

func hermesWatchRoots(root string) []WatchRoot {
	root = filepath.Clean(root)
	if stateDB, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		watchRoots := []WatchRoot{{
			Path:         filepath.Dir(stateDB),
			Recursive:    false,
			IncludeGlobs: []string{"state.db", "state.db-wal"},
			DebounceKey:  string(AgentHermes) + ":archive:" + root,
		}}
		watchRoots = append(watchRoots, WatchRoot{
			Path:         sessionsDir,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "session_*.json"},
			DebounceKey:  string(AgentHermes) + ":sessions:" + root,
		})
		return watchRoots
	}
	return []WatchRoot{{
		Path:         root,
		Recursive:    true,
		IncludeGlobs: []string{"state.db", "state.db-wal", "*.jsonl", "session_*.json"},
		DebounceKey:  string(AgentHermes) + ":sessions:" + root,
	}}
}

func ResolveHermesWatchRoots(root string) []string {
	root = filepath.Clean(root)
	if _, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		return []string{sessionsDir}
	}
	return []string{root}
}

func ResolveHermesShallowWatchRoots(root string) []string {
	root = filepath.Clean(root)
	if stateDB, _, ok := hermesArchiveRootPaths(root); ok {
		return []string{filepath.Dir(stateDB)}
	}
	return nil
}

func hermesWatchRootMatches(root, watchRoot string) bool {
	root = filepath.Clean(root)
	watchRoot = filepath.Clean(watchRoot)
	if samePath(root, watchRoot) {
		return true
	}
	if stateDB, sessionsDir, ok := hermesArchiveRootPaths(root); ok {
		return samePath(watchRoot, filepath.Dir(stateDB)) ||
			samePath(watchRoot, sessionsDir)
	}
	switch filepath.Base(root) {
	case "state.db":
		return samePath(watchRoot, filepath.Dir(root)) ||
			samePath(watchRoot, filepath.Join(filepath.Dir(root), "sessions"))
	case "sessions":
		return samePath(watchRoot, filepath.Dir(root))
	default:
		return samePath(watchRoot, filepath.Join(root, "sessions"))
	}
}

func hermesArchivePathsForEvent(root, path string) (stateDB, sessionsDir string, ok bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	switch {
	case filepath.Base(root) == "state.db":
		stateDB = root
		sessionsDir = filepath.Join(filepath.Dir(root), "sessions")
	case filepath.Base(root) == "sessions":
		stateDB = filepath.Join(filepath.Dir(root), "state.db")
		sessionsDir = root
	case samePath(path, filepath.Join(root, "state.db")) ||
		IsRegularFile(filepath.Join(root, "state.db")):
		stateDB = filepath.Join(root, "state.db")
		sessionsDir = filepath.Join(root, "sessions")
	default:
		return "", "", false
	}
	return stateDB, sessionsDir, true
}

func hermesArchiveRootPaths(root string) (stateDB, sessionsDir string, ok bool) {
	root = filepath.Clean(root)
	if stateDB, sessionsDir, ok := hermesStatePaths(root); ok {
		return stateDB, sessionsDir, true
	}
	switch filepath.Base(root) {
	case "state.db":
		return root, filepath.Join(filepath.Dir(root), "sessions"), true
	case "sessions":
		return filepath.Join(filepath.Dir(root), "state.db"), root, true
	default:
		stateDB = filepath.Join(root, "state.db")
		sessionsDir = filepath.Join(root, "sessions")
		if IsRegularFile(stateDB) {
			return stateDB, sessionsDir, true
		}
		if info, err := os.Stat(sessionsDir); err == nil && info.IsDir() {
			return stateDB, sessionsDir, true
		}
		return "", "", false
	}
}

func hermesTranscriptRoot(root string) string {
	root = filepath.Clean(root)
	if _, sessionsDir, ok := hermesStatePaths(root); ok {
		return sessionsDir
	}
	childSessions := filepath.Join(root, "sessions")
	if info, err := os.Stat(childSessions); err == nil && info.IsDir() {
		return childSessions
	}
	return root
}

func hermesPathInTranscriptDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if !samePath(filepath.Dir(path), dir) {
		return false
	}
	name := filepath.Base(path)
	if strings.HasSuffix(name, ".jsonl") {
		return true
	}
	return strings.HasSuffix(name, ".json") && strings.HasPrefix(name, "session_")
}

func hermesArchiveFingerprint(source SourceRef, stateDB string) (SourceFingerprint, error) {
	stateInfo, err := os.Stat(stateDB)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", stateDB, err)
	}
	if stateInfo.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", stateDB)
	}
	fingerprint := SourceFingerprint{
		Key: firstNonEmptyJSONLString(
			source.FingerprintKey,
			source.Key,
			stateDB,
		),
		Size:    stateInfo.Size(),
		MTimeNS: stateInfo.ModTime().UnixNano(),
	}
	h := sha256.New()
	if err := addHermesFingerprintPart(h, "state", stateDB, stateInfo); err != nil {
		return SourceFingerprint{}, err
	}
	walPath := stateDB + "-wal"
	if walInfo, err := os.Stat(walPath); err == nil {
		if walInfo.IsDir() {
			return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", walPath)
		}
		// A zero-length WAL carries no committed frames and is created as a
		// side effect of merely opening the database read-only (the parse
		// itself creates one). Folding its mtime into the fingerprint would
		// make the identity computed before a parse never match the one
		// computed after it, so every subsequent sync re-parses an unchanged
		// archive. Skip the empty WAL; any real commit gives it frames.
		if walInfo.Size() > 0 {
			fingerprint.Size += walInfo.Size()
			if mtime := walInfo.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
				fingerprint.MTimeNS = mtime
			}
			if err := addHermesFingerprintPart(h, "wal", walPath, walInfo); err != nil {
				return SourceFingerprint{}, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", walPath, err)
	}
	_, sessionsDir, _ := hermesStatePaths(stateDB)
	for _, file := range discoverHermesTranscriptFiles(sessionsDir) {
		info, err := os.Stat(file.Path)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", file.Path, err)
		}
		fingerprint.Size += info.Size()
		if mtime := info.ModTime().UnixNano(); mtime > fingerprint.MTimeNS {
			fingerprint.MTimeNS = mtime
		}
		if err := addHermesFingerprintPart(h, "transcript", file.Path, info); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

func hermesStateMemberFingerprint(
	ctx context.Context, source SourceRef, src hermesSource,
) (SourceFingerprint, error) {
	if !IsRegularFile(src.StateDB) {
		return SourceFingerprint{Key: source.FingerprintKey}, nil
	}
	if reconciliationCacheAvailable(ctx) {
		if h, selectedPath, ok := cachedHermesMemberCore(ctx, src); ok {
			return hermesStateMemberFingerprintFinish(source, src, h, selectedPath)
		}
		// One bulk pass over state.db seeds every member's core, so the rest
		// of the reconciliation fingerprints without reopening the database.
		// Seeding failures fall through to the instrumented per-member open,
		// which surfaces the underlying error with its usual handling.
		if seedHermesMemberCores(ctx, src.StateDB) == nil {
			if h, selectedPath, ok := cachedHermesMemberCore(ctx, src); ok {
				return hermesStateMemberFingerprintFinish(source, src, h, selectedPath)
			}
		}
	}
	observeSharedContainerScan(ctx)
	ss, messages, selectedPath, err := readHermesStateSessionSource(
		src.StateDB, src.SessionID,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SourceFingerprint{Key: source.FingerprintKey}, nil
		}
		return SourceFingerprint{}, err
	}
	h := sha256.New()
	if err := addHermesStateSessionFingerprint(h, ss, messages); err != nil {
		return SourceFingerprint{}, err
	}
	return hermesStateMemberFingerprintFinish(source, src, h, selectedPath)
}

func hermesStateMemberFingerprintFinish(
	source SourceRef, src hermesSource, h hash.Hash, selectedPath string,
) (SourceFingerprint, error) {
	info, err := os.Stat(src.StateDB)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint := SourceFingerprint{
		Key:  firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
	}
	if selectedPath != src.StateDB {
		selectedInfo, err := os.Stat(selectedPath)
		if err != nil {
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", selectedPath, err)
		}
		fingerprint.Size += selectedInfo.Size()
		fingerprint.MTimeNS = max(
			fingerprint.MTimeNS, selectedInfo.ModTime().UnixNano(),
		)
		if err := addHermesFingerprintPart(
			h, "selected-transcript", selectedPath, selectedInfo,
		); err != nil {
			return SourceFingerprint{}, err
		}
	}
	fingerprint.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return fingerprint, nil
}

// hermesMemberFingerprintCore is the cached per-member fingerprint seed: the
// sha256 digest state over the member's session row and messages, plus the
// selected source path. seedHermesMemberCores computes it once per pass on a
// shared state.db connection; the fingerprint path resumes the digest instead
// of reopening the database, producing byte-identical hashes either way.
type hermesMemberFingerprintCore struct {
	HashState    []byte `json:"hash_state"`
	SelectedPath string `json:"selected_path"`
}

func hermesMemberCoreCacheKey(fingerprintKey string) string {
	return "hermes:member-core:" + fingerprintKey
}

// seedHermesMemberCores reads every state.db member once and caches each
// member's fingerprint core for the rest of the pass. The per-reconciliation
// once keeps the bulk read to one per state.db per pass even when parallel
// fingerprint workers miss concurrently or a member read error leaves
// individual entries unseeded.
func seedHermesMemberCores(ctx context.Context, stateDB string) error {
	return reconciliationCacheOnce(
		ctx, "hermes:member-cores-seeded:"+stateDB,
		func() error { return seedHermesMemberCoresLocked(ctx, stateDB) },
	)
}

func seedHermesMemberCoresLocked(ctx context.Context, stateDB string) error {
	idsConn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open hermes state db: %w", err)
	}
	defer idsConn.Close()
	memberConn, err := sql.Open("sqlite3", "file:"+sqliteURIPath(stateDB)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open hermes member state db: %w", err)
	}
	defer memberConn.Close()
	observeSharedContainerScan(ctx)
	rows, err := idsConn.QueryContext(ctx, "SELECT id FROM sessions ORDER BY id")
	if err != nil {
		return fmt.Errorf("query hermes sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan hermes session id: %w", err)
		}
		if !IsValidSessionID(id) {
			continue
		}
		retainedIDBytes := int64(len(id))
		observeStreamingRetainedBytes(ctx, retainedIDBytes)
		if err := cacheHermesMemberCore(
			ctx, memberConn, stateDB, id, VirtualSourcePath(stateDB, id),
		); err != nil {
			observeStreamingRetainedBytes(ctx, -retainedIDBytes)
			return err
		}
		observeStreamingRetainedBytes(ctx, -retainedIDBytes)
	}
	return rows.Err()
}

// cacheHermesMemberCore seeds one member's fingerprint core into the
// per-pass reconciliation cache. Member read and digest-marshal problems are
// left for the fingerprint path to surface through its established error
// handling; only cache-write failures propagate.
func cacheHermesMemberCore(
	ctx context.Context, conn *sql.DB, stateDB, id, fingerprintKey string,
) error {
	ss, messages, selectedPath, err := readHermesStateSessionSourceConn(
		conn, stateDB, id,
	)
	if err != nil {
		return nil
	}
	h := sha256.New()
	if err := addHermesStateSessionFingerprint(h, ss, messages); err != nil {
		return nil
	}
	marshaler, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return nil
	}
	payload, err := json.Marshal(hermesMemberFingerprintCore{
		HashState: state, SelectedPath: selectedPath,
	})
	if err != nil {
		return nil
	}
	return reconciliationCachePut(
		ctx, hermesMemberCoreCacheKey(fingerprintKey), string(payload),
	)
}

// cachedHermesMemberCore resumes a member's seeded digest from the per-pass
// reconciliation cache. Any miss, decode failure, or unresumable digest falls
// back to the instrumented per-member open path.
func cachedHermesMemberCore(
	ctx context.Context, src hermesSource,
) (hash.Hash, string, bool) {
	value, found, err := reconciliationCacheGet(
		ctx, hermesMemberCoreCacheKey(src.Path),
	)
	if err != nil || !found {
		return nil, "", false
	}
	var core hermesMemberFingerprintCore
	if err := json.Unmarshal([]byte(value), &core); err != nil {
		return nil, "", false
	}
	h := sha256.New()
	unmarshaler, ok := h.(encoding.BinaryUnmarshaler)
	if !ok || unmarshaler.UnmarshalBinary(core.HashState) != nil {
		return nil, "", false
	}
	return h, core.SelectedPath, true
}

func addHermesStateSessionFingerprint(
	h hash.Hash, ss hermesStateSession, messages []hermesStateMessage,
) error {
	if _, err := fmt.Fprintf(
		h,
		"state-member\x00%q\x00%q\x00%q\x00%q\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%t\x00%g\x00%t\x00%g\x00%q\x00%q\x00%q\x00%d\x00",
		ss.id,
		ss.source,
		ss.model,
		ss.parentSessionID,
		ss.startedAt.UnixNano(),
		ss.endedAt.UnixNano(),
		ss.messageCount,
		ss.inputTokens,
		ss.outputTokens,
		ss.cacheReadTokens,
		ss.cacheWriteTokens,
		ss.reasoningTokens,
		ss.apiCallCount,
		ss.estimatedCost.Valid,
		ss.estimatedCost.Float64,
		ss.actualCost.Valid,
		ss.actualCost.Float64,
		ss.costStatus,
		ss.costSource,
		ss.title,
		len(messages),
	); err != nil {
		return err
	}
	return encodeHermesStateSessionJSONL(h, ss, messages)
}

// hermesArchiveEffectiveFileInfo returns the aggregate size and mtime of a
// Hermes archive: the state.db, its WAL, and every transcript file in its
// sessions directory. WAL-only commits and transcript-only changes both shift
// the stored archive freshness even though state.db itself is unchanged. The
// transcript set matches the legacy
// hermesArchiveTranscriptFiles: every .jsonl and session_*.json file directly
// under the sessions directory, without the .jsonl/.json dedup used elsewhere.
func hermesArchiveEffectiveFileInfo(stateDB string) (int64, int64) {
	info, err := os.Stat(stateDB)
	if err != nil {
		return 0, 0
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	// A zero-length WAL is a read-side artifact of opening the database and
	// carries no committed frames; hermesArchiveFingerprint ignores it for the
	// same reason, and the two aggregations must agree.
	if walInfo, err := os.Stat(stateDB + "-wal"); err == nil &&
		!walInfo.IsDir() && walInfo.Size() > 0 {
		size += walInfo.Size()
		if walMtime := walInfo.ModTime().UnixNano(); walMtime > mtime {
			mtime = walMtime
		}
	}
	_, sessionsDir, ok := hermesStatePaths(stateDB)
	if !ok {
		return size, mtime
	}
	for _, path := range hermesArchiveTranscriptFiles(sessionsDir) {
		fileInfo, err := os.Stat(path)
		if err != nil || fileInfo == nil || fileInfo.IsDir() {
			continue
		}
		size += fileInfo.Size()
		if fileMtime := fileInfo.ModTime().UnixNano(); fileMtime > mtime {
			mtime = fileMtime
		}
	}
	return size, mtime
}

// hermesArchiveTranscriptFiles lists every .jsonl and session_*.json file
// directly under sessionsDir, sorted by path. It mirrors the legacy engine
// helper of the same name so the provider's effective-info aggregation matches
// historical behavior exactly.
func hermesArchiveTranscriptFiles(sessionsDir string) []string {
	if sessionsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".jsonl") ||
			strings.HasPrefix(name, "session_") && strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(sessionsDir, name))
		}
	}
	sort.Strings(paths)
	return paths
}

func addHermesFingerprintPart(
	h hash.Hash,
	label string,
	path string,
	info os.FileInfo,
) error {
	if _, err := fmt.Fprintf(
		h,
		"%s\x00%s\x00%d\x00%d\x00",
		label,
		path,
		info.Size(),
		info.ModTime().UnixNano(),
	); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}
	return nil
}

func hermesProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilitySupported,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilitySupported,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
