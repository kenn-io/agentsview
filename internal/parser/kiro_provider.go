package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	src, ok := p.sources.sourceFromRef(source)
	if !ok {
		return ReconciliationSourceRank{}
	}
	class := int64(0)
	switch src.Kind {
	case kiroSourceLegacyJSONL:
		class = 1
	case kiroSourceCurrentJSONL:
		class = 2
	case kiroSourceSQLiteSession:
		class = 3
	}
	recency := source.DiscoveryMTimeNS
	if recency == 0 && src.Path != "" {
		if info, err := os.Stat(src.Path); err == nil {
			recency = info.ModTime().UnixNano()
		}
	}
	return ReconciliationSourceRank{Class: class, Recency: recency}
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
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	return ParseOutcome{
		Results:           results,
		SourceErrors:      sourceErrs,
		ResultSetComplete: true,
		ForceReplace:      true,
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
	if p.sources.legacyPathShadowed(src.Path) {
		return ParseOutcome{
			ResultSetComplete: true,
			SkipReason:        SkipNoSession,
		}, nil
	}
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
	Root      string
	Path      string
	DBPath    string
	SessionID string
	Kind      kiroSourceKind
}

type kiroSourceSet struct {
	roots []string
}

func newKiroSourceSet(roots []string) kiroSourceSet {
	return kiroSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s kiroSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	currentIDs := s.currentSessionIDs()
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if dbPath := kiroSQLiteDBPath(root); dbPath != "" {
			addJSONLSource(s.newSourceRef(root, dbPath, dbPath, "", kiroSourceSQLiteDB), &sources, seen)
		}
		for _, file := range s.discoverLegacyJSONL(root) {
			if _, shadowed := currentIDs[KiroSessionIDFromPath(file.Path)]; shadowed {
				continue
			}
			source, ok := s.sourceRef(root, file.Path, false)
			if ok {
				addJSONLSource(source, &sources, seen)
			}
		}
		for _, file := range s.discoverCurrentJSONL(root) {
			if source, ok := s.sourceRef(root, file.Path, false); ok {
				addJSONLSource(source, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s kiroSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dbPath := kiroSQLiteDBPath(root); dbPath != "" {
			store, err := OpenKiroSQLiteStore(dbPath)
			if err != nil {
				return err
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
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if err := s.discoverLegacyJSONLEach(ctx, root, yield); err != nil {
			return err
		}
		if err := s.discoverCurrentJSONLEach(ctx, root, yield); err != nil {
			return err
		}
	}
	return nil
}

func (s kiroSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "messages.jsonl", "session.json", kiroSQLiteDBName, kiroSQLiteDBName + "-*"},
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
		sources := []SourceRef{source}
		if current, ok := s.sourceFromRef(source); ok &&
			current.Kind == kiroSourceCurrentJSONL {
			if dbPath := kiroSQLiteDBPath(root); dbPath != "" &&
				KiroSQLiteSessionExists(dbPath, current.SessionID) {
				sources = append(sources, s.newSourceRef(
					root, KiroSQLiteVirtualPath(dbPath, current.SessionID), dbPath,
					current.SessionID, kiroSourceSQLiteSession,
				))
			}
		}
		sources = append(
			sources,
			s.changedPathTombstones(root, source, req.StoredSourcePaths)...,
		)
		return sources, nil
	}
	return nil, nil
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
) []SourceRef {
	src, ok := s.sourceFromRef(changed)
	if !ok || src.Kind != kiroSourceSQLiteDB || !IsRegularFile(src.DBPath) {
		return nil
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
		if KiroSQLiteSessionExists(member.DBPath, member.SessionID) {
			continue
		}
		if _, dup := seen[member.Path]; dup {
			continue
		}
		seen[member.Path] = struct{}{}
		tombstones = append(tombstones, ref)
	}
	return tombstones
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
		if dbPath := kiroSQLiteDBPath(root); dbPath != "" &&
			KiroSQLiteSessionExists(dbPath, req.RawSessionID) {
			candidates = append(candidates, s.newSourceRef(
				root, KiroSQLiteVirtualPath(dbPath, req.RawSessionID), dbPath,
				req.RawSessionID, kiroSourceSQLiteSession,
			))
		}
		for _, file := range s.discoverCurrentJSONL(root) {
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
		for _, file := range s.discoverLegacyJSONL(root) {
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
	if src.Kind == kiroSourceCurrentJSONL {
		if sidecar, ok := kiroCurrentSidecarPath(s.roots, src.Path); ok {
			if sideInfo, err := os.Stat(sidecar); err == nil {
				fingerprint.Size += sideInfo.Size()
				if sideInfo.ModTime().UnixNano() > fingerprint.MTimeNS {
					fingerprint.MTimeNS = sideInfo.ModTime().UnixNano()
				}
			}
		}
	}
	hash, err := hashJSONLSourceFile(src.Path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint.Hash = hash
	return fingerprint, nil
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
		return s.newSourceRef(root, dbPath, dbPath, "", kiroSourceSQLiteDB), true
	}
	return SourceRef{}, false
}

func (s kiroSourceSet) currentSessionIDs() map[string]struct{} {
	ids := make(map[string]struct{})
	for _, root := range s.roots {
		for id := range KiroSQLiteSessionIDs(root) {
			ids[id] = struct{}{}
		}
		for _, file := range s.discoverCurrentJSONL(root) {
			if _, id, ok := kiroCurrentPathUnderRoot(root, file.Path); ok {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func (s kiroSourceSet) legacyPathShadowed(path string) bool {
	legacyID := KiroSessionIDFromPath(path)
	if legacyID == "" {
		return false
	}
	for _, root := range s.roots {
		dbPath := kiroSQLiteDBPath(root)
		if dbPath != "" && KiroSQLiteSessionExists(dbPath, legacyID) {
			return true
		}
		for _, file := range s.discoverCurrentJSONL(root) {
			if _, id, ok := kiroCurrentPathUnderRoot(root, file.Path); ok && id == legacyID {
				return true
			}
		}
	}
	return false
}

func (s kiroSourceSet) newSourceRef(
	root, path, dbPath, sessionID string,
	kind kiroSourceKind,
) SourceRef {
	key := path
	if kind == kiroSourceSQLiteSession || kind == kiroSourceCurrentJSONL {
		key = sessionID
	} else if kind == kiroSourceLegacyJSONL {
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
		if sourceRoot < bestRoot ||
			(sourceRoot == bestRoot && (rank.Class > bestRank.Class ||
				(rank.Class == bestRank.Class && rank.Recency > bestRank.Recency))) {
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
	class := int64(0)
	switch src.Kind {
	case kiroSourceLegacyJSONL:
		class = 1
	case kiroSourceCurrentJSONL:
		class = 2
	case kiroSourceSQLiteSession:
		class = 3
	}
	recency := source.DiscoveryMTimeNS
	if recency == 0 {
		if info, err := os.Stat(src.Path); err == nil {
			recency = info.ModTime().UnixNano()
		}
	}
	return ReconciliationSourceRank{Class: class, Recency: recency}
}

func kiroDBUnderRoot(root, dbPath string, requireRegular bool) bool {
	root = filepath.Clean(root)
	dbPath = filepath.Clean(dbPath)
	rel, ok := relUnder(root, dbPath)
	if !ok || filepath.ToSlash(rel) != kiroSQLiteDBName {
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

func (s kiroSourceSet) discoverCurrentJSONL(root string) []DiscoveredFile {
	var files []DiscoveredFile
	add := func(session string) {
		path := filepath.Join(root, session, "messages.jsonl")
		if _, _, ok := kiroCurrentPathUnderRoot(root, path); ok && kiroRegularFileUnderRoot(root, path) {
			files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
		}
	}
	addWorkspace := func(session string) {
		path := filepath.Join(root, kiroCurrentWorkspaceDir, session, "messages.jsonl")
		if _, _, ok := kiroCurrentPathUnderRoot(root, path); ok && kiroRegularFileUnderRoot(root, path) {
			files = append(files, DiscoveredFile{Path: path, Agent: AgentKiro})
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if isKiroCurrentSessionDir(entry.Name()) {
			add(entry.Name())
		}
	}
	workspace := filepath.Join(root, kiroCurrentWorkspaceDir)
	children, err := os.ReadDir(workspace)
	if err == nil {
		for _, child := range children {
			if child.IsDir() && isKiroCurrentSessionDir(child.Name()) {
				addWorkspace(child.Name())
			}
		}
	}
	return files
}

func (s kiroSourceSet) findCurrentJSONL(root, sessionID string) (SourceRef, bool) {
	if !isKiroCurrentSessionDir(sessionID) {
		return SourceRef{}, false
	}
	for _, path := range []string{
		filepath.Join(root, sessionID, "messages.jsonl"),
		filepath.Join(root, kiroCurrentWorkspaceDir, sessionID, "messages.jsonl"),
	} {
		if _, _, ok := kiroCurrentPathUnderRoot(root, path); ok && kiroRegularFileUnderRoot(root, path) {
			return s.newSourceRef(root, path, "", sessionID, kiroSourceCurrentJSONL), true
		}
	}
	return SourceRef{}, false
}

func (s kiroSourceSet) discoverCurrentJSONLEach(ctx context.Context, root string, yield func(SourceRef) error) error {
	return streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
		if !entry.IsDir() {
			return nil
		}
		if isKiroCurrentSessionDir(entry.Name()) {
			path := filepath.Join(root, entry.Name(), "messages.jsonl")
			if source, ok := s.sourceRef(root, path, false); ok {
				return yield(source)
			}
			return nil
		}
		if entry.Name() != kiroCurrentWorkspaceDir {
			return nil
		}
		workspace := filepath.Join(root, kiroCurrentWorkspaceDir)
		return streamDirectoryEntries(ctx, workspace, func(child os.DirEntry) error {
			if !child.IsDir() || !isKiroCurrentSessionDir(child.Name()) {
				return nil
			}
			path := filepath.Join(workspace, child.Name(), "messages.jsonl")
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
			if s.legacyPathShadowed(path) {
				return nil
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

const kiroCurrentWorkspaceDir = "workspace"

func kiroCurrentPathUnderRoot(root, path string) (string, string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if (len(parts) != 2 && len(parts) != 3) || parts[len(parts)-1] != "messages.jsonl" {
		return "", "", false
	}
	if len(parts) == 3 && parts[0] != kiroCurrentWorkspaceDir {
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
	session := parts[len(parts)-2]
	if !isKiroCurrentSessionDir(session) {
		return "", "", false
	}
	return filepath.Join(filepath.Dir(path), "messages.jsonl"), session, true
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
	if !IsRegularFile(path) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	_, ok := relUnder(resolvedRoot, resolvedPath)
	return ok
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
			UnchangedResults: UnchangedResultMTime,
		},
	}
}
