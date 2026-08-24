package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var _ Provider = (*kiroProvider)(nil)

type kiroProviderFactory struct {
	def AgentDef
}

func newKiroProviderFactory(def AgentDef) ProviderFactory {
	return kiroProviderFactory{def: cloneAgentDef(def)}
}

func (f kiroProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f kiroProviderFactory) Capabilities() Capabilities {
	return kiroProviderCapabilities()
}

func (f kiroProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &kiroProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    kiroProviderCapabilities(),
		Config:  cfg,
		sources: newKiroSourceSet(cfg.Roots),
	}
}

type kiroProvider struct {
	ProviderBase
	sources kiroSourceSet
}

func (p *kiroProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *kiroProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *kiroProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *kiroProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *kiroProvider) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	return p.sources.StoredSourceHintScopes(req)
}

func (p *kiroProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *kiroProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *kiroProvider) ReconciliationSourceRank(
	source SourceRef,
) ReconciliationSourceRank {
	return p.sources.sourceRank(source)
}

func (p *kiroProvider) PreservedSessionIDs(source SourceRef) []string {
	src, ok := p.sources.sourceFromRef(source)
	if !ok || len(src.PreservedIDs) == 0 {
		return nil
	}
	return append([]string(nil), src.PreservedIDs...)
}

func (p *kiroProvider) PersistentArchiveSource(
	path string, fullSessionID string,
) (string, bool) {
	rawSessionID := ProviderRawSessionIDFromFull(p.Def, fullSessionID)
	for _, root := range p.sources.roots {
		source, ok := p.sources.sourceRef(root, path, true)
		if !ok {
			continue
		}
		src, ok := p.sources.sourceFromRef(source)
		if ok && src.Kind == kiroSourceSQLiteSession &&
			rawSessionID != "" && src.SessionID == rawSessionID {
			return src.DBPath, true
		}
	}
	return "", false
}

func (p *kiroProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("kiro source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	switch src.Kind {
	case kiroSourceSQLiteDB:
		return p.parseSQLiteDB(ctx, src, machine)
	case kiroSourceSQLiteSession:
		return p.parseSQLiteSession(src, machine, req.Fingerprint)
	case kiroSourceCurrentJSONL:
		return p.parseCurrentJSONL(src, machine, req.Fingerprint)
	default:
		return p.parseLegacyJSONL(src, machine, req.Fingerprint)
	}
}

func (p *kiroProvider) parseCurrentJSONL(
	src kiroSource, machine string, fingerprint SourceFingerprint,
) (ParseOutcome, error) {
	sess, msgs, err := p.parseCurrentSession(src.Path, src.SessionID, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{ResultSetComplete: true, SkipReason: SkipNoSession}, nil
	}
	if fingerprint.Size > 0 {
		sess.File.Size = fingerprint.Size
	}
	if fingerprint.MTimeNS > 0 {
		sess.File.Mtime = fingerprint.MTimeNS
	}
	if fingerprint.Hash != "" {
		sess.File.Hash = fingerprint.Hash
	}
	return ParseOutcome{Results: []ParseResultOutcome{{
		Result:      ParseResult{Session: *sess, Messages: msgs},
		DataVersion: DataVersionCurrent,
	}}, ResultSetComplete: true}, nil
}

func (p *kiroProvider) parseSQLiteDB(
	ctx context.Context,
	src kiroSource,
	machine string,
) (ParseOutcome, error) {
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			// The entire backing DB file is gone. Preserve the container's
			// stored sessions (the SQLite store is a persistent archive) by
			// skipping without ForceReplace, which would otherwise delete every
			// stored session discovered from this DB.
			return ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	store, err := OpenKiroSQLiteStore(src.DBPath)
	if err != nil {
		return ParseOutcome{}, err
	}
	defer store.Close()
	metas, err := store.ListSessionMeta()
	if err != nil {
		return ParseOutcome{}, err
	}
	results := make([]ParseResultOutcome, 0, len(metas))
	var sourceErrs []SourceError
	for _, meta := range metas {
		if src.SessionIDsSet {
			if _, ok := src.SessionIDs[meta.SessionID]; !ok {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return ParseOutcome{}, err
		}
		sess, msgs, err := store.ParseSession(meta.SessionID, machine)
		if err != nil {
			sourceErrs = append(sourceErrs, SourceError{
				SourceKey:   meta.VirtualPath,
				DisplayPath: meta.VirtualPath,
				SessionID:   "kiro:" + meta.SessionID,
				Err:         err,
				Retryable:   true,
			})
			continue
		}
		if sess == nil {
			continue
		}
		results = append(results, ParseResultOutcome{
			Result: ParseResult{
				Session:  *sess,
				Messages: msgs,
			},
			DataVersion: DataVersionCurrent,
		})
	}
	if len(results) == 0 && len(sourceErrs) == 0 {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      !src.SessionIDsSet,
			SkipReason:        SkipNoSession,
		}, nil
	}
	return ParseOutcome{
		Results:           results,
		SourceErrors:      sourceErrs,
		ResultSetComplete: true,
		ForceReplace:      !src.SessionIDsSet,
	}, nil
}

func (p *kiroProvider) parseSQLiteSession(
	src kiroSource,
	machine string,
	fingerprint SourceFingerprint,
) (ParseOutcome, error) {
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			// The entire backing DB file is gone. Preserve the stored sessions
			// (the SQLite store is a persistent archive) by skipping without
			// ForceReplace, which would otherwise delete every stored session
			// for this source. The sql.ErrNoRows case below keeps ForceReplace
			// because the DB is present and the row was genuinely removed.
			return ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	sess, msgs, err := parseKiroSQLiteSession(src.DBPath, src.SessionID, machine)
	if errors.Is(err, sql.ErrNoRows) {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if fingerprint.Hash != "" {
		sess.File.Hash = fingerprint.Hash
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
		ForceReplace:      true,
	}, nil
}

func (p *kiroProvider) parseLegacyJSONL(
	src kiroSource,
	machine string,
	fingerprint SourceFingerprint,
) (ParseOutcome, error) {
	sess, msgs, err := p.parseLegacySession(src.Path, machine)
	if err != nil {
		return ParseOutcome{}, err
	}
	if sess == nil {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	if fingerprint.Hash != "" {
		sess.File.Hash = fingerprint.Hash
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

type kiroSourceKind uint8

const (
	kiroSourceLegacyJSONL kiroSourceKind = iota
	kiroSourceSQLiteDB
	kiroSourceSQLiteSession
	kiroSourceCurrentJSONL
)

type kiroSource struct {
	Root            string
	Path            string
	DBPath          string
	SessionID       string
	Kind            kiroSourceKind
	SessionIDs      map[string]struct{}
	SessionIDsSet   bool
	SessionIDsTotal int
	PreservedIDs    []string
}

type kiroSourceSet struct {
	roots     []string
	planCache *kiroChangedPlanCache
	readDir   func(string) ([]os.DirEntry, error)
}

type kiroChangedPlanCache struct {
	mu        sync.Mutex
	key       string
	winners   map[string]SourceRef
	databases []SourceRef
}

func newKiroSourceSet(roots []string) kiroSourceSet {
	return kiroSourceSet{
		roots:     cleanJSONLRoots(roots),
		planCache: &kiroChangedPlanCache{},
		readDir:   os.ReadDir,
	}
}

func (s kiroSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	winners, databases, err := s.sourcePlan(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]SourceRef, 0, len(winners)+len(databases))
	seen := make(map[string]struct{})
	for _, source := range databases {
		addJSONLSource(source, &sources, seen)
	}
	for _, source := range winners {
		if src, ok := s.sourceFromRef(source); ok && src.Kind == kiroSourceSQLiteSession {
			continue
		}
		addJSONLSource(source, &sources, seen)
	}
	sortJSONLSources(sources)
	return sources, nil
}

// sourcePlan is the single Kiro authority used before normal workers are
// started. Physical SQLite sources carry the member allowlist selected here.
func (s kiroSourceSet) sourcePlan(ctx context.Context) (map[string]SourceRef, []SourceRef, error) {
	candidates := make(map[string][]SourceRef)
	databases := make([]SourceRef, 0)
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		dbPath, dbPathErr := kiroSQLiteDBPathChecked(root)
		if dbPathErr != nil {
			return nil, nil, dbPathErr
		}
		if dbPath != "" {
			dbSource := s.newSourceRef(root, dbPath, dbPath, "", kiroSourceSQLiteDB)
			metas, err := ListKiroSQLiteSessionMeta(dbPath)
			if err != nil {
				return nil, nil, fmt.Errorf("list Kiro SQLite sessions in %s: %w", dbPath, err)
			}
			dbSourceSrc, ok := s.sourceFromRef(dbSource)
			if !ok {
				return nil, nil, fmt.Errorf("kiro SQLite source unavailable: %s", dbPath)
			}
			dbSourceSrc.SessionIDsTotal = len(metas)
			dbSource.Opaque = dbSourceSrc
			for _, meta := range metas {
				member := s.newSourceRef(root, meta.VirtualPath, dbPath, meta.SessionID, kiroSourceSQLiteSession)
				member.DiscoveryMTimeNS = meta.FileMtime
				candidates[meta.SessionID] = append(candidates[meta.SessionID], member)
			}
			databases = append(databases, dbSource)
		}
		currentFiles, err := s.discoverCurrentJSONL(root)
		if err != nil {
			return nil, nil, err
		}
		for _, file := range currentFiles {
			if source, ok := s.sourceRef(root, file.Path, false); ok {
				if _, id, ok := kiroCurrentPathUnderRoot(root, file.Path); ok {
					s.setSourceMTime(&source)
					candidates[id] = append(candidates[id], source)
				}
			}
		}
		legacyFiles, err := s.discoverLegacyJSONL(root)
		if err != nil {
			return nil, nil, err
		}
		for _, file := range legacyFiles {
			id := KiroSessionIDFromPath(file.Path)
			if id == "" {
				continue
			}
			if source, ok := s.sourceRef(root, file.Path, false); ok {
				s.setSourceMTime(&source)
				candidates[id] = append(candidates[id], source)
			}
		}
	}
	winners := make(map[string]SourceRef, len(candidates))
	for id, list := range candidates {
		if winner, ok := s.bestSource(list); ok {
			winners[id] = winner
		}
	}
	for i := range databases {
		src, _ := s.sourceFromRef(databases[i])
		src.SessionIDs = make(map[string]struct{})
		src.PreservedIDs = nil
		for id, winner := range winners {
			winnerSrc, ok := s.sourceFromRef(winner)
			if !ok {
				continue
			}
			if winnerSrc.Kind == kiroSourceSQLiteSession && samePath(winnerSrc.DBPath, src.DBPath) {
				src.SessionIDs[id] = struct{}{}
				continue
			}
			src.PreservedIDs = append(src.PreservedIDs, "kiro:"+id)
		}
		sort.Strings(src.PreservedIDs)
		if src.SessionIDsTotal > 0 && len(src.SessionIDs) < src.SessionIDsTotal {
			src.SessionIDsSet = true
		} else {
			src.SessionIDs = nil
			src.SessionIDsSet = false
		}
		databases[i].Opaque = src
	}
	return winners, databases, nil
}

func (s kiroSourceSet) setSourceMTime(source *SourceRef) {
	if source.DiscoveryMTimeNS != 0 {
		return
	}
	if src, ok := s.sourceFromRef(*source); ok {
		if info, err := os.Stat(src.Path); err == nil {
			source.DiscoveryMTimeNS = info.ModTime().UnixNano()
		}
	}
}

func (s kiroSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	var firstErr error
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		dbPath, dbPathErr := kiroSQLiteDBPathChecked(root)
		if dbPathErr != nil {
			return dbPathErr
		}
		if dbPath != "" {
			store, err := OpenKiroSQLiteStore(dbPath)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			err = store.ForEachSessionMeta(ctx, func(meta KiroSQLiteSessionMeta) error {
				source := s.newSourceRef(
					root, meta.VirtualPath, dbPath, meta.SessionID,
					kiroSourceSQLiteSession,
				)
				source.DiscoveryMTimeNS = meta.FileMtime
				return yield(source)
			})
			closeErr := store.Close()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if closeErr != nil {
				if firstErr == nil {
					firstErr = closeErr
				}
				continue
			}
		}
		if err := s.discoverLegacyJSONLEach(ctx, root, yield); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.discoverCurrentJSONLEach(ctx, root, yield); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

func (s kiroSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "*.json", "messages.jsonl", "session.json", kiroSQLiteDBName, kiroSQLiteDBName + "-*"},
			DebounceKey:  string(AgentKiro) + ":root:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s kiroSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		source, ok := s.sourceRefForChangedPath(root, req.Path)
		if !ok {
			continue
		}
		winners, databases, err := s.sourcePlanForChangedPath(ctx, req, source)
		if err != nil {
			return nil, err
		}
		current, _ := s.sourceFromRef(source)
		sources := make([]SourceRef, 0, 2)
		if current.Kind == kiroSourceSQLiteDB {
			for _, database := range databases {
				db, ok := s.sourceFromRef(database)
				if ok && samePath(db.DBPath, current.DBPath) {
					sources = append(sources, database)
					break
				}
			}
		} else {
			id := current.SessionID
			if id == "" {
				id = KiroSessionIDFromPath(current.Path)
			}
			if winner, ok := winners[id]; ok {
				sources = append(sources, winner)
			}
		}
		tombstones, err := s.changedPathTombstones(root, source, req.StoredSourcePaths, winners)
		if err != nil {
			return nil, err
		}
		sources = append(sources, tombstones...)
		return sources, nil
	}
	return nil, nil
}

func (s kiroSourceSet) sourcePlanForChangedPath(
	ctx context.Context, req ChangedPathRequest, changed SourceRef,
) (map[string]SourceRef, []SourceRef, error) {
	changedSrc, ok := s.sourceFromRef(changed)
	if !ok {
		return nil, nil, nil
	}
	if changedSrc.Kind != kiroSourceSQLiteDB {
		id := changedSrc.SessionID
		if id == "" {
			id = KiroSessionIDFromPath(changedSrc.Path)
		}
		winner, found, err := s.sourceForSession(ctx, id, changed)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return map[string]SourceRef{}, nil, nil
		}
		return map[string]SourceRef{id: winner}, nil, nil
	}
	if s.planCache == nil {
		return s.sourcePlan(ctx)
	}
	key := kiroChangedPlanCacheKey(req)
	s.planCache.mu.Lock()
	if s.planCache.key == key {
		winners, databases := s.planCache.winners, s.planCache.databases
		s.planCache.mu.Unlock()
		return winners, databases, nil
	}
	s.planCache.mu.Unlock()

	winners, databases, err := s.sourcePlan(ctx)
	if err != nil {
		return nil, nil, err
	}
	s.planCache.mu.Lock()
	s.planCache.key = key
	s.planCache.winners = winners
	s.planCache.databases = databases
	s.planCache.mu.Unlock()
	return winners, databases, nil
}

func (s kiroSourceSet) sourceForSession(
	ctx context.Context, sessionID string, changed SourceRef,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	if sessionID == "" {
		return SourceRef{}, false, nil
	}
	var candidates []SourceRef
	if changedSrc, ok := s.sourceFromRef(changed); ok &&
		changedSrc.Kind != kiroSourceSQLiteDB && kiroSourceExists(changed) {
		s.setSourceMTime(&changed)
		candidates = append(candidates, changed)
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return SourceRef{}, false, err
		}
		dbPath, err := kiroSQLiteDBPathChecked(root)
		if err != nil {
			return SourceRef{}, false, err
		}
		if dbPath != "" {
			meta, found, err := KiroSQLiteSessionMetaForID(dbPath, sessionID)
			if err != nil {
				return SourceRef{}, false, fmt.Errorf(
					"find Kiro SQLite session %s: %w", sessionID, err,
				)
			}
			if found {
				source := s.newSourceRef(
					root, meta.VirtualPath, dbPath, sessionID,
					kiroSourceSQLiteSession,
				)
				source.DiscoveryMTimeNS = meta.FileMtime
				candidates = append(candidates, source)
			}
		}
		currentFiles, err := s.discoverCurrentJSONLForSession(root, sessionID)
		if err != nil {
			return SourceRef{}, false, err
		}
		for _, file := range currentFiles {
			if source, ok := s.sourceRef(root, file.Path, false); ok {
				s.setSourceMTime(&source)
				candidates = append(candidates, source)
			}
		}
		legacySources, err := s.legacySourcesForSession(root, sessionID, changed)
		if err != nil {
			return SourceRef{}, false, err
		}
		for _, source := range legacySources {
			s.setSourceMTime(&source)
			candidates = append(candidates, source)
		}
	}
	winner, found := s.bestSource(candidates)
	return winner, found, nil
}

// legacySourcesForSession resolves the legacy transcripts that can carry
// sessionID with bounded exact-path probes: legacy identity is derived from
// the file name, so only <root>/<id>.jsonl and <root>/cli/<id>.jsonl qualify.
func (s kiroSourceSet) legacySourcesForSession(
	root, sessionID string, changed SourceRef,
) ([]SourceRef, error) {
	root = filepath.Clean(root)
	paths := make(map[string]struct{})
	if changedSrc, ok := s.sourceFromRef(changed); ok &&
		changedSrc.Kind == kiroSourceLegacyJSONL &&
		samePath(changedSrc.Root, root) &&
		kiroSourceExists(changed) &&
		KiroSessionIDFromPath(changedSrc.Path) == sessionID {
		paths[filepath.Clean(changedSrc.Path)] = struct{}{}
	}
	if IsValidSessionID(sessionID) {
		for _, dir := range []string{root, filepath.Join(root, "cli")} {
			candidate := filepath.Join(dir, sessionID+".jsonl")
			regular, err := kiroRegularFileUnderRootChecked(root, candidate)
			if err != nil {
				return nil, fmt.Errorf(
					"probe Kiro legacy transcript %s: %w", candidate, err,
				)
			}
			if regular {
				paths[candidate] = struct{}{}
			}
		}
	}
	sources := make([]SourceRef, 0, len(paths))
	for path := range paths {
		if source, ok := s.sourceRef(root, path, false); ok {
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func kiroChangedPlanCacheKey(req ChangedPathRequest) string {
	info, err := os.Stat(req.Path)
	if err != nil {
		return fmt.Sprintf("%s\x00%s\x00%s\x00missing", req.WatchRoot, req.Path, req.EventKind)
	}
	return fmt.Sprintf(
		"%s\x00%s\x00%s\x00%d\x00%d",
		req.WatchRoot, req.Path, req.EventKind, info.Size(), info.ModTime().UnixNano(),
	)
}

func (s kiroSourceSet) StoredSourceHintScopes(
	req ChangedPathRequest,
) []StoredSourceHintScope {
	for _, root := range s.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		source, ok := s.sourceRefForChangedPath(root, req.Path)
		if !ok {
			continue
		}
		src, ok := s.sourceFromRef(source)
		if !ok {
			return nil
		}
		switch src.Kind {
		case kiroSourceSQLiteDB:
			return []StoredSourceHintScope{{
				Path: src.DBPath, IncludeVirtualMembers: true,
			}}
		case kiroSourceSQLiteSession, kiroSourceLegacyJSONL:
			return []StoredSourceHintScope{{Path: src.Path}}
		case kiroSourceCurrentJSONL:
			return []StoredSourceHintScope{{Path: src.Path}}
		}
		return nil
	}
	return nil
}

// changedPathTombstones emits a per-session source for every stored Kiro SQLite
// member whose row is gone from a still-present database. The whole-DB source
// re-writes the surviving rows; these let a row deleted from a present database
// be force-replaced out of the archive, matching the db-backed providers.
// A vanished database file yields no tombstones, preserving the stored sessions
// (per the persistent-archive rule).
func (s kiroSourceSet) changedPathTombstones(
	root string,
	changed SourceRef,
	storedPaths []string,
	winners map[string]SourceRef,
) ([]SourceRef, error) {
	src, ok := s.sourceFromRef(changed)
	if !ok || src.Kind != kiroSourceSQLiteDB || !IsRegularFile(src.DBPath) {
		return nil, nil
	}
	var tombstones []SourceRef
	seen := make(map[string]struct{})
	for _, stored := range storedPaths {
		ref, ok := s.sourceRef(root, stored, true)
		if !ok {
			continue
		}
		member, ok := ref.Opaque.(kiroSource)
		if !ok || member.Kind != kiroSourceSQLiteSession {
			continue
		}
		if !samePath(member.DBPath, src.DBPath) {
			continue
		}
		exists, err := KiroSQLiteSessionExistsWithError(member.DBPath, member.SessionID)
		if err != nil {
			return nil, fmt.Errorf("check Kiro SQLite session %s: %w", member.SessionID, err)
		}
		if exists {
			continue
		}
		if winner, ok := winners[member.SessionID]; ok {
			if winnerSrc, winnerOK := s.sourceFromRef(winner); winnerOK &&
				(winnerSrc.Kind != kiroSourceSQLiteSession || !samePath(winnerSrc.DBPath, member.DBPath)) {
				tombstones = append(tombstones, winner)
			}
			continue
		}
		if _, dup := seen[member.Path]; dup {
			continue
		}
		seen[member.Path] = struct{}{}
		tombstones = append(tombstones, ref)
	}
	return tombstones, nil
}

func (s kiroSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	var hinted []SourceRef
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path, true); ok {
				if req.RequireFreshSource && !kiroSourceExists(source) {
					continue
				}
				if req.RawSessionID == "" {
					return source, true, nil
				}
				if sourceMatchesKiroSession(source, req.RawSessionID) {
					if req.PreferStoredSource {
						return source, true, nil
					}
					hinted = append(hinted, source)
				}
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	var candidates []SourceRef
	candidates = append(candidates, hinted...)
	for _, root := range s.roots {
		dbPath, dbPathErr := kiroSQLiteDBPathChecked(root)
		if dbPathErr != nil {
			return SourceRef{}, false, dbPathErr
		}
		if dbPath != "" {
			exists, err := KiroSQLiteSessionExistsWithError(dbPath, req.RawSessionID)
			if err != nil {
				return SourceRef{}, false, fmt.Errorf(
					"find Kiro SQLite session %s: %w", req.RawSessionID, err,
				)
			}
			if exists {
				candidates = append(candidates, s.newSourceRef(
					root, KiroSQLiteVirtualPath(dbPath, req.RawSessionID), dbPath,
					req.RawSessionID, kiroSourceSQLiteSession,
				))
			}
		}
		currentFiles, err := s.discoverCurrentJSONL(root)
		if err != nil {
			return SourceRef{}, false, err
		}
		for _, file := range currentFiles {
			if _, id, ok := kiroCurrentPathUnderRoot(root, file.Path); ok &&
				id == req.RawSessionID {
				if source, ok := s.sourceRef(root, file.Path, false); ok {
					candidates = append(candidates, source)
				}
			}
		}
		if path := s.legacySourceFile(root, req.RawSessionID); path != "" {
			if source, ok := s.sourceRef(root, path, false); ok {
				candidates = append(candidates, source)
			}
		}
		legacyFiles, err := s.discoverLegacyJSONL(root)
		if err != nil {
			return SourceRef{}, false, err
		}
		for _, file := range legacyFiles {
			if KiroSessionIDFromPath(file.Path) != req.RawSessionID {
				continue
			}
			if source, ok := s.sourceRef(root, file.Path, false); ok {
				candidates = append(candidates, source)
			}
		}
	}
	if source, ok := s.bestSource(candidates); ok {
		return source, true, nil
	}
	return SourceRef{}, false, nil
}

func kiroSourceExists(source SourceRef) bool {
	src, ok := source.Opaque.(kiroSource)
	if !ok {
		ptr, ok := source.Opaque.(*kiroSource)
		if !ok || ptr == nil {
			return false
		}
		src = *ptr
	}
	switch src.Kind {
	case kiroSourceSQLiteSession:
		return KiroSQLiteSessionExists(src.DBPath, src.SessionID)
	case kiroSourceSQLiteDB:
		return IsRegularFile(src.DBPath)
	default:
		return IsRegularFile(src.Path)
	}
}

func (s kiroSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("kiro source path unavailable")
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path)
	if src.Kind == kiroSourceSQLiteSession {
		if _, err := os.Stat(src.DBPath); err != nil {
			if os.IsNotExist(err) {
				return SourceFingerprint{Key: key}, nil
			}
			return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
		}
		row, err := loadKiroSQLiteRow(src.DBPath, src.SessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return SourceFingerprint{Key: key}, nil
			}
			return SourceFingerprint{}, err
		}
		return SourceFingerprint{
			Key:     key,
			Size:    int64(len(row.value)),
			MTimeNS: row.updatedAt * 1_000_000,
		}, nil
	}
	info, err := os.Stat(src.Path)
	if err != nil {
		if os.IsNotExist(err) && src.Kind == kiroSourceSQLiteDB {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", src.Path)
	}
	fingerprint := SourceFingerprint{
		Key:     key,
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
	}
	if src.Kind == kiroSourceSQLiteDB {
		if compositeMtime, err := sqliteDBCompositeMtime(
			src.DBPath, sqliteDBJournalSuffixes,
		); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		return fingerprint, nil
	}
	sidecar := ""
	switch src.Kind {
	case kiroSourceCurrentJSONL:
		if sidecar, ok := kiroCurrentSidecarPath(s.roots, src.Path); ok {
			if sideInfo, err := os.Stat(sidecar); err == nil {
				fingerprint.Size += sideInfo.Size()
				if sideInfo.ModTime().UnixNano() > fingerprint.MTimeNS {
					fingerprint.MTimeNS = sideInfo.ModTime().UnixNano()
				}
			}
		}
	case kiroSourceLegacyJSONL:
		candidate, found, sidecarErr := kiroLegacySidecarPath(s.roots, src.Path)
		if sidecarErr != nil {
			return SourceFingerprint{}, sidecarErr
		}
		if found {
			sidecar = candidate
			sideInfo, statErr := os.Stat(sidecar)
			if statErr != nil {
				return SourceFingerprint{}, fmt.Errorf("stat %s: %w", sidecar, statErr)
			}
			fingerprint.Size += sideInfo.Size()
			if sideInfo.ModTime().UnixNano() > fingerprint.MTimeNS {
				fingerprint.MTimeNS = sideInfo.ModTime().UnixNano()
			}
		}
	}
	hash := ""
	switch src.Kind {
	case kiroSourceCurrentJSONL:
		sidecar = ""
		if candidate, ok := kiroCurrentSidecarPath(s.roots, src.Path); ok {
			sidecar = candidate
		}
		hash, err = hashKiroJSONLSource(src.Path, sidecar)
	case kiroSourceLegacyJSONL:
		hash, err = hashKiroJSONLSource(src.Path, sidecar)
	default:
		hash, err = hashJSONLSourceFile(src.Path)
	}
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint.Hash = hash
	return fingerprint, nil
}

func hashKiroJSONLSource(transcript, sidecar string) (string, error) {
	transcriptHash, err := hashJSONLSourceFile(transcript)
	if err != nil {
		return "", err
	}
	sidecarHash := "missing"
	if sidecar != "" {
		if _, err := os.Stat(sidecar); err == nil {
			sidecarHash, err = hashJSONLSourceFile(sidecar)
			if err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", sidecar, err)
		}
	}
	digest := sha256.Sum256([]byte("kiro-current\x00" + transcriptHash + "\x00" + sidecarHash))
	return fmt.Sprintf("%x", digest[:]), nil
}

func (s kiroSourceSet) sourceFromRef(source SourceRef) (kiroSource, bool) {
	switch src := source.Opaque.(type) {
	case kiroSource:
		return src, src.Path != ""
	case *kiroSource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, true); ok {
				src := ref.Opaque.(kiroSource)
				return src, true
			}
		}
	}
	return kiroSource{}, false
}

// IsKiroSQLiteSource reports whether a Kiro source is backed by the current
// SQLite store rather than one of the JSONL layouts.
func IsKiroSQLiteSource(source SourceRef) bool {
	src, ok := (kiroSourceSet{}).sourceFromRef(source)
	return ok && (src.Kind == kiroSourceSQLiteDB || src.Kind == kiroSourceSQLiteSession)
}

// KiroSQLiteSourcePresent reports whether the SQLite container for a Kiro
// source still exists at parse time.
func KiroSQLiteSourcePresent(source SourceRef) bool {
	src, ok := (kiroSourceSet{}).sourceFromRef(source)
	return ok && IsRegularFile(src.DBPath)
}

func (s kiroSourceSet) sourceRef(
	root, path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if currentPath, sessionID, ok := kiroCurrentPathUnderRoot(root, path); ok {
		if _, err := os.Lstat(currentPath); err == nil {
			if !kiroRegularFileUnderRoot(root, currentPath) {
				return SourceRef{}, false
			}
		} else if !allowMissing || !kiroEventPathUnderRoot(root, currentPath) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, currentPath, "", sessionID, kiroSourceCurrentJSONL), true
	}
	if kiroLegacyPathUnderRoot(root, path) && kiroRegularFileUnderRoot(root, path) {
		return s.newSourceRef(root, path, "", "", kiroSourceLegacyJSONL), true
	}
	if dbPath, sessionID, ok := kiroSQLiteVirtualPathParts(path); ok {
		if !kiroDBUnderRoot(root, dbPath, !allowMissing) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, path, dbPath, sessionID, kiroSourceSQLiteSession), true
	}
	if kiroDBUnderRoot(root, path, !allowMissing) {
		return s.newSourceRef(root, path, path, "", kiroSourceSQLiteDB), true
	}
	if !kiroLegacyPathUnderRoot(root, path) {
		return SourceRef{}, false
	}
	if !allowMissing && !kiroRegularFileUnderRoot(root, path) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, path, "", "", kiroSourceLegacyJSONL), true
}

func (s kiroSourceSet) sourceRefForChangedPath(root, path string) (SourceRef, bool) {
	if source, ok := s.sourceRef(root, path, false); ok {
		return source, true
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if currentPath, sessionID, ok := kiroCurrentPathForEvent(root, path); ok {
		if filepath.Base(path) == "session.json" {
			if _, err := os.Lstat(path); err == nil && !kiroRegularFileUnderRoot(root, path) {
				return SourceRef{}, false
			}
		}
		if !kiroEventPathUnderRoot(root, currentPath) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, currentPath, "", sessionID, kiroSourceCurrentJSONL), true
	}
	if legacyPath, ok := kiroLegacyMetaPathForEvent(root, path); ok {
		if !kiroEventPathUnderRoot(root, legacyPath) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, legacyPath, "", "", kiroSourceLegacyJSONL), true
	}
	if kiroLegacyPathUnderRoot(root, path) && kiroEventPathUnderRoot(root, path) {
		return s.newSourceRef(root, path, "", "", kiroSourceLegacyJSONL), true
	}
	if dbPath, sessionID, ok := kiroSQLiteVirtualPathParts(path); ok {
		if !kiroDBUnderRoot(root, dbPath, false) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, path, dbPath, sessionID, kiroSourceSQLiteSession), true
	}
	if dbPath, ok := kiroDBPathForEvent(root, path); ok {
		if !kiroDBUnderRoot(root, dbPath, false) {
			return SourceRef{}, false
		}
		return s.newSourceRef(root, dbPath, dbPath, "", kiroSourceSQLiteDB), true
	}
	return SourceRef{}, false
}

func kiroLegacyMetaPathForEvent(root, path string) (string, bool) {
	if filepath.Ext(path) != ".json" {
		return "", false
	}
	legacyPath := strings.TrimSuffix(filepath.Clean(path), ".json") + ".jsonl"
	return legacyPath, kiroLegacyPathUnderRoot(root, legacyPath)
}

func (s kiroSourceSet) newSourceRef(
	root, path, dbPath, sessionID string,
	kind kiroSourceKind,
) SourceRef {
	key := path
	switch kind {
	case kiroSourceSQLiteSession, kiroSourceCurrentJSONL:
		key = sessionID
	case kiroSourceLegacyJSONL:
		key = KiroSessionIDFromPath(path)
	}
	return SourceRef{
		Provider:       AgentKiro,
		ConfiguredRoot: root,
		Key:            key,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: kiroSource{
			Root:      root,
			Path:      path,
			DBPath:    dbPath,
			SessionID: sessionID,
			Kind:      kind,
		},
	}
}

func sourceMatchesKiroSession(source SourceRef, rawID string) bool {
	src, ok := source.Opaque.(kiroSource)
	if !ok {
		if ptr, ptrOK := source.Opaque.(*kiroSource); ptrOK && ptr != nil {
			src, ok = *ptr, true
		}
	}
	if !ok {
		return false
	}
	if src.SessionID != "" {
		return src.SessionID == rawID
	}
	return KiroSessionIDFromPath(src.Path) == rawID
}

func (s kiroSourceSet) bestSource(sources []SourceRef) (SourceRef, bool) {
	if len(sources) == 0 {
		return SourceRef{}, false
	}
	best := sources[0]
	bestRank := s.sourceRank(best)
	for _, source := range sources[1:] {
		rank := s.sourceRank(source)
		bestRoot, sourceRoot := s.rootIndex(best.ConfiguredRoot), s.rootIndex(source.ConfiguredRoot)
		if rank.Class > bestRank.Class ||
			(rank.Class == bestRank.Class && (sourceRoot < bestRoot ||
				(sourceRoot == bestRoot && (rank.Recency > bestRank.Recency ||
					(rank.Recency == bestRank.Recency && s.sourcePath(source) < s.sourcePath(best)))))) {
			best, bestRank = source, rank
		}
	}
	return best, true
}

func (s kiroSourceSet) rootIndex(root string) int {
	for i, configured := range s.roots {
		if samePath(configured, root) {
			return i
		}
	}
	return len(s.roots)
}

func (s kiroSourceSet) sourceRank(source SourceRef) ReconciliationSourceRank {
	src, ok := s.sourceFromRef(source)
	if !ok {
		return ReconciliationSourceRank{}
	}
	class := kiroSourceClass(src.Kind)
	recency := source.DiscoveryMTimeNS
	if recency == 0 {
		if info, err := os.Stat(src.Path); err == nil {
			recency = info.ModTime().UnixNano()
		}
	}
	return ReconciliationSourceRank{Class: class, Recency: recency}
}

func kiroSourceClass(kind kiroSourceKind) int64 {
	switch kind {
	case kiroSourceLegacyJSONL:
		return 1
	case kiroSourceCurrentJSONL:
		return 2
	case kiroSourceSQLiteSession:
		return 3
	default:
		return 0
	}
}

func (s kiroSourceSet) sourcePath(source SourceRef) string {
	if src, ok := s.sourceFromRef(source); ok {
		path := src.Path
		if path == "" {
			path = src.DBPath
		}
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			path = canonical
		}
		return filepath.Clean(path)
	}
	return filepath.Clean(providerSourcePathForRank(source))
}

func providerSourcePathForRank(source SourceRef) string {
	for _, path := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if path != "" {
			return path
		}
	}
	return ""
}

func kiroDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != kiroSQLiteDBName {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedDB, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		if requireRegular {
			return false
		}
		_, rootErr := os.Stat(root)
		return rootErr == nil
	}
	if _, ok := relUnder(resolvedRoot, resolvedDB); !ok {
		return false
	}
	return !requireRegular || IsRegularFile(dbPath)
}

func kiroDBPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	if filepath.ToSlash(rel) == kiroSQLiteDBName ||
		(filepath.Dir(rel) == "." &&
			strings.HasPrefix(filepath.Base(rel), kiroSQLiteDBName+"-")) {
		return filepath.Join(root, kiroSQLiteDBName), true
	}
	return "", false
}

func kiroLegacyPathUnderRoot(root, path string) bool {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 2 && parts[0] != "cli" {
		return false
	}
	if len(parts) > 2 {
		return false
	}
	return strings.HasSuffix(rel, ".jsonl")
}

func (s kiroSourceSet) discoverCurrentJSONL(root string) ([]DiscoveredFile, error) {
	var files []DiscoveredFile
	add := func(workspace, session string) error {
		path := filepath.Join(root, workspace, session, "messages.jsonl")
		if _, _, ok := kiroCurrentPathUnderRoot(root, path); !ok {
			return nil
		}
		regular, err := kiroRegularFileUnderRootChecked(root, path)
		if err != nil {
			return fmt.Errorf("probe Kiro current transcript %s: %w", path, err)
		}
		if regular {
			files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
		}
		return nil
	}
	entries, err := s.readDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if isKiroCurrentSessionDir(entry.Name()) {
			if err := add("", entry.Name()); err != nil {
				return nil, err
			}
		}
		if !isKiroCurrentWorkspaceDir(entry.Name()) {
			continue
		}
		workspace := filepath.Join(root, entry.Name())
		children, err := s.readDir(workspace)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, child := range children {
			if child.IsDir() && isKiroCurrentSessionDir(child.Name()) {
				if err := add(entry.Name(), child.Name()); err != nil {
					return nil, err
				}
			}
		}
	}
	return files, nil
}

func (s kiroSourceSet) discoverCurrentJSONLForSession(
	root, sessionID string,
) ([]DiscoveredFile, error) {
	if !isKiroCurrentSessionDir(sessionID) {
		return nil, nil
	}
	var files []DiscoveredFile
	add := func(workspace string) error {
		path := filepath.Join(root, workspace, sessionID, "messages.jsonl")
		if _, _, ok := kiroCurrentPathUnderRoot(root, path); !ok {
			return nil
		}
		regular, err := kiroRegularFileUnderRootChecked(root, path)
		if err != nil {
			return fmt.Errorf("probe Kiro current transcript %s: %w", path, err)
		}
		if regular {
			files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
		}
		return nil
	}
	if err := add(""); err != nil {
		return nil, err
	}
	entries, err := s.readDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return files, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isKiroCurrentWorkspaceDir(entry.Name()) {
			continue
		}
		if err := add(entry.Name()); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (s kiroSourceSet) discoverCurrentJSONLEach(ctx context.Context, root string, yield func(SourceRef) error) error {
	return streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		if !entry.IsDir() {
			return nil
		}
		if isKiroCurrentSessionDir(entry.Name()) {
			path := filepath.Join(root, entry.Name(), "messages.jsonl")
			regular, err := kiroRegularFileUnderRootChecked(root, path)
			if err != nil {
				return fmt.Errorf("probe Kiro current transcript %s: %w", path, err)
			}
			if !regular {
				return nil
			}
			if source, ok := s.sourceRef(root, path, false); ok {
				return yield(source)
			}
		}
		if !isKiroCurrentWorkspaceDir(entry.Name()) {
			return nil
		}
		workspace := filepath.Join(root, entry.Name())
		return streamDirectoryEntries(ctx, workspace, func(child os.DirEntry) error {
			if !child.IsDir() || !isKiroCurrentSessionDir(child.Name()) {
				return nil
			}
			path := filepath.Join(workspace, child.Name(), "messages.jsonl")
			regular, err := kiroRegularFileUnderRootChecked(root, path)
			if err != nil {
				return fmt.Errorf("probe Kiro current transcript %s: %w", path, err)
			}
			if !regular {
				return nil
			}
			if source, ok := s.sourceRef(root, path, false); ok {
				return yield(source)
			}
			return nil
		})
	})
}

func (s kiroSourceSet) discoverLegacyJSONLEach(ctx context.Context, root string, yield func(SourceRef) error) error {
	for _, dir := range []string{root, filepath.Join(root, "cli")} {
		if err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			path := filepath.Join(dir, entry.Name())
			regular, err := kiroRegularFileUnderRootChecked(root, path)
			if err != nil {
				return fmt.Errorf("probe Kiro legacy transcript %s: %w", path, err)
			}
			if !regular {
				return nil
			}
			if _, err := loadKiroMetaStrict(path); err != nil {
				return err
			}
			if source, ok := s.sourceRef(root, path, false); ok {
				return yield(source)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func isKiroCurrentSessionDir(name string) bool {
	id := strings.TrimPrefix(name, "sess_")
	return strings.HasPrefix(name, "sess_") && IsValidSessionID(id)
}

func isKiroCurrentWorkspaceDir(name string) bool {
	return name != "" && name != ".history" && name != "snapshots" &&
		!isKiroCurrentSessionDir(name)
}

func kiroCurrentPathUnderRoot(root, path string) (string, string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if (len(parts) != 2 && len(parts) != 3) || parts[len(parts)-1] != "messages.jsonl" {
		return "", "", false
	}
	if len(parts) == 3 && !isKiroCurrentWorkspaceDir(parts[0]) {
		return "", "", false
	}
	session := parts[len(parts)-2]
	if !isKiroCurrentSessionDir(session) {
		return "", "", false
	}
	return filepath.Clean(path), session, true
}

func kiroCurrentPathForEvent(root, path string) (string, string, bool) {
	if current, id, ok := kiroCurrentPathUnderRoot(root, path); ok {
		return current, id, true
	}
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if (len(parts) != 2 && len(parts) != 3) || parts[len(parts)-1] != "session.json" {
		return "", "", false
	}
	current, session, ok := kiroCurrentPathUnderRoot(root, filepath.Join(filepath.Dir(path), "messages.jsonl"))
	if !ok {
		return "", "", false
	}
	return current, session, true
}

func kiroCurrentSidecarPath(roots []string, transcript string) (string, bool) {
	for _, root := range roots {
		current, _, ok := kiroCurrentPathUnderRoot(root, transcript)
		if !ok || !samePath(current, transcript) {
			continue
		}
		sidecar := filepath.Join(filepath.Dir(transcript), "session.json")
		if kiroRegularFileUnderRoot(root, sidecar) {
			return sidecar, true
		}
	}
	return "", false
}

func kiroRegularFileUnderRoot(root, path string) bool {
	regular, _ := kiroRegularFileUnderRootChecked(root, path)
	return regular
}

func kiroRegularFileUnderRootChecked(root, path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	_, ok := relUnder(resolvedRoot, resolvedPath)
	return ok, nil
}

func kiroLegacySidecarPath(
	roots []string, transcript string,
) (string, bool, error) {
	for _, root := range roots {
		if !kiroLegacyPathUnderRoot(root, transcript) {
			continue
		}
		sidecar := strings.TrimSuffix(transcript, ".jsonl") + ".json"
		if _, err := os.Lstat(sidecar); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("stat %s: %w", sidecar, err)
		}
		if !kiroEventPathUnderRoot(root, sidecar) {
			continue
		}
		return sidecar, true, nil
	}
	return "", false, nil
}

// kiroEventPathUnderRoot validates an event path even when the changed file
// has already been removed; existing ancestors must resolve inside the root.
func kiroEventPathUnderRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if _, ok := relUnder(root, path); !ok {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for candidate := path; ; candidate = filepath.Dir(candidate) {
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			_, ok := relUnder(resolvedRoot, resolved)
			return ok
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return false
		}
	}
}

func kiroProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
	source.StoredSourceHints = CapabilitySupported
	source.MultiSessionSource = CapabilitySupported
	source.PerSessionErrors = CapabilitySupported
	source.ForceReplaceOnParse = CapabilitySupported
	source.PersistentArchive = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			Cwd:          CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			UnchangedResults:                    UnchangedResultMTimeAndHash,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}
