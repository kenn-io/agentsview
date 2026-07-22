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
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   kiroProviderCapabilities(),
			Config: cfg,
		},
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
	default:
		return p.parseLegacyJSONL(src, machine, req.Fingerprint)
	}
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
		if err := streamDirectoryEntries(ctx, root, func(entry os.DirEntry) error {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			path := filepath.Join(root, entry.Name())
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

func (s kiroSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{"*.jsonl", kiroSQLiteDBName, kiroSQLiteDBName + "-*"},
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
	if req.RawSessionID != "" {
		for _, root := range s.roots {
			dbPath := kiroSQLiteDBPath(root)
			if dbPath != "" && KiroSQLiteSessionExists(dbPath, req.RawSessionID) {
				return s.newSourceRef(
					root,
					KiroSQLiteVirtualPath(dbPath, req.RawSessionID),
					dbPath,
					req.RawSessionID,
					kiroSourceSQLiteSession,
				), true, nil
			}
		}
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path, true); ok {
				if req.RequireFreshSource && !kiroSourceExists(source) {
					continue
				}
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.legacySourceFile(root, req.RawSessionID)
		if path != "" {
			if source, ok := s.sourceRef(root, path, false); ok {
				return source, true, nil
			}
		}
	}
	sources, err := s.Discover(ctx)
	if err != nil {
		return SourceRef{}, false, err
	}
	for _, source := range sources {
		src := source.Opaque.(kiroSource)
		if src.Kind == kiroSourceLegacyJSONL &&
			KiroSessionIDFromPath(src.Path) == req.RawSessionID {
			return source, true, nil
		}
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
		if compositeMtime, err := sqliteDBCompositeMtime(src.DBPath); err == nil {
			fingerprint.MTimeNS = compositeMtime
		}
		return fingerprint, nil
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
	if kiroLegacyPathUnderRoot(root, path) && IsRegularFile(path) {
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
	if !allowMissing && !IsRegularFile(path) {
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
	if kiroLegacyPathUnderRoot(root, path) {
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
	}
	return false
}

func (s kiroSourceSet) newSourceRef(
	root, path, dbPath, sessionID string,
	kind kiroSourceKind,
) SourceRef {
	return SourceRef{
		Provider:       AgentKiro,
		Key:            path,
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
	if strings.Contains(rel, string(filepath.Separator)) {
		return false
	}
	return strings.HasSuffix(rel, ".jsonl")
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
	}
}
