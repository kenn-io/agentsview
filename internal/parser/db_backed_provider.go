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

type dbBackedSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
}

type dbBackedProviderSpec struct {
	agent        AgentType
	dbName       string
	findDB       func(string) string
	listMeta     func(string) ([]dbBackedSessionMeta, error)
	parse        func(string, string, string) ([]ParseResult, error)
	normalizeRaw func(string) string
	caps         Capabilities
}

type dbBackedProviderFactory struct {
	def  AgentDef
	spec dbBackedProviderSpec
}

func (f dbBackedProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f dbBackedProviderFactory) Capabilities() Capabilities {
	return f.spec.caps
}

func (f dbBackedProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &dbBackedProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   f.spec.caps,
			Config: cfg,
		},
		spec:    f.spec,
		sources: newDBBackedSourceSet(f.spec, cfg.Roots),
	}
}

type dbBackedProvider struct {
	ProviderBase
	spec    dbBackedProviderSpec
	sources dbBackedSourceSet
}

func (p *dbBackedProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *dbBackedProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *dbBackedProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *dbBackedProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	if p.spec.normalizeRaw != nil {
		req.RawSessionID = p.spec.normalizeRaw(req.RawSessionID)
	}
	return p.sources.FindSource(ctx, req)
}

func (p *dbBackedProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *dbBackedProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("%s source path unavailable", p.spec.agent)
	}
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			// The entire backing DB file is gone. The SQLite store is a
			// persistent archive: sessions must be preserved even when their
			// source file no longer exists on disk. Skip without ForceReplace
			// so the engine keeps the stored sessions instead of deleting them.
			// The sql.ErrNoRows / empty-results cases below keep ForceReplace
			// because the DB is still present and the row was genuinely removed.
			return ParseOutcome{
				ResultSetComplete: true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	results, err := p.spec.parse(src.DBPath, src.SessionID, machine)
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
	if len(results) == 0 {
		return ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
			SkipReason:        SkipNoSession,
		}, nil
	}
	out := make([]ParseResultOutcome, 0, len(results))
	for _, result := range results {
		if req.Fingerprint.Hash != "" {
			result.Session.File.Hash = req.Fingerprint.Hash
		}
		out = append(out, ParseResultOutcome{
			Result:      result,
			DataVersion: DataVersionCurrent,
		})
	}
	return ParseOutcome{
		Results:           out,
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type dbBackedSource struct {
	Root      string
	DBPath    string
	SessionID string
}

type dbBackedSourceSet struct {
	spec  dbBackedProviderSpec
	roots []string
}

func newDBBackedSourceSet(
	spec dbBackedProviderSpec,
	roots []string,
) dbBackedSourceSet {
	return dbBackedSourceSet{
		spec:  spec,
		roots: cleanJSONLRoots(roots),
	}
}

func (s dbBackedSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dbPath := s.spec.findDB(root)
		if dbPath == "" {
			continue
		}
		metas, err := s.spec.listMeta(dbPath)
		if err != nil {
			return nil, err
		}
		for _, meta := range metas {
			ref := s.newSourceRef(root, dbPath, meta.SessionID, meta.VirtualPath)
			// Carry the per-session mtime captured here so parse-diff's --limit
			// sampler can order these virtual "<db>#<sessionID>" sources by each
			// session's real mtime rather than stat'ing a path that has no
			// on-disk existence. Ordering metadata only: skip-cache and
			// data-version freshness still resolve through Fingerprint.
			ref.DiscoveryMTimeNS = meta.FileMtime
			addJSONLSource(ref, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s dbBackedSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{s.spec.dbName, s.spec.dbName + "-*"},
			DebounceKey:  string(s.spec.agent) + ":db:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s dbBackedSourceSet) SourcesForChangedPath(
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
		if ref, ok := s.sourceRef(root, req.Path, true); ok {
			return []SourceRef{ref}, nil
		}
		dbPath, ok := s.dbPathForEvent(root, req.Path)
		if !ok {
			continue
		}
		metas, err := s.spec.listMeta(dbPath)
		if err != nil {
			return nil, err
		}
		sources := make([]SourceRef, 0, len(metas))
		seen := make(map[string]struct{}, len(metas))
		for _, meta := range metas {
			addJSONLSource(
				s.newSourceRef(root, dbPath, meta.SessionID, meta.VirtualPath),
				&sources,
				seen,
			)
		}
		for _, path := range req.StoredSourcePaths {
			ref, ok := s.sourceRef(root, path, true)
			if !ok {
				continue
			}
			src := ref.Opaque.(dbBackedSource)
			if !samePath(src.DBPath, dbPath) {
				continue
			}
			addJSONLSource(ref, &sources, seen)
		}
		sortJSONLSources(sources)
		return sources, nil
	}
	return nil, nil
}

func (s dbBackedSourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path, true); ok {
				src := source.Opaque.(dbBackedSource)
				if req.RawSessionID != "" && src.SessionID != req.RawSessionID {
					continue
				}
				if req.RequireFreshSource {
					fresh, err := s.sourceExists(src)
					if err != nil {
						return SourceRef{}, false, err
					}
					if !fresh {
						continue
					}
				}
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		dbPath := s.spec.findDB(root)
		if dbPath == "" {
			continue
		}
		metas, err := s.spec.listMeta(dbPath)
		if err != nil {
			return SourceRef{}, false, err
		}
		for _, meta := range metas {
			if meta.SessionID == req.RawSessionID {
				return s.newSourceRef(root, dbPath, meta.SessionID, meta.VirtualPath), true, nil
			}
		}
	}
	return SourceRef{}, false, nil
}

func (s dbBackedSourceSet) sourceExists(src dbBackedSource) (bool, error) {
	if !IsRegularFile(src.DBPath) {
		return false, nil
	}
	metas, err := s.spec.listMeta(src.DBPath)
	if err != nil {
		return false, err
	}
	for _, meta := range metas {
		if meta.SessionID == src.SessionID {
			return true, nil
		}
	}
	return false, nil
}

func (s dbBackedSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("%s source path unavailable", s.spec.agent)
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.virtualPath())
	if _, err := os.Stat(src.DBPath); err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", src.DBPath, err)
	}
	metas, err := s.spec.listMeta(src.DBPath)
	if err != nil {
		return SourceFingerprint{}, err
	}
	for _, meta := range metas {
		if meta.SessionID == src.SessionID {
			return SourceFingerprint{
				Key:     key,
				MTimeNS: meta.FileMtime,
			}, nil
		}
	}
	return SourceFingerprint{Key: key}, nil
}

func (s dbBackedSourceSet) sourceFromRef(source SourceRef) (dbBackedSource, bool) {
	switch src := source.Opaque.(type) {
	case dbBackedSource:
		return src, src.DBPath != "" && src.SessionID != ""
	case *dbBackedSource:
		if src != nil && src.DBPath != "" && src.SessionID != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, true); ok {
				src := ref.Opaque.(dbBackedSource)
				return src, true
			}
		}
	}
	return dbBackedSource{}, false
}

func (s dbBackedSourceSet) sourceRef(
	root, path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	dbPath, sessionID, ok := parseDBBackedVirtualPath(path)
	if !ok {
		return SourceRef{}, false
	}
	if filepath.Base(dbPath) != s.spec.dbName {
		return SourceRef{}, false
	}
	if !samePath(dbPath, filepath.Join(root, s.spec.dbName)) {
		return SourceRef{}, false
	}
	if !allowMissing && !IsRegularFile(dbPath) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, dbPath, sessionID, path), true
}

func (s dbBackedSourceSet) dbPathForEvent(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok {
		return "", false
	}
	if strings.Contains(rel, string(filepath.Separator)) {
		return "", false
	}
	if rel == s.spec.dbName ||
		rel == s.spec.dbName+"-wal" ||
		rel == s.spec.dbName+"-shm" {
		dbPath := filepath.Join(root, s.spec.dbName)
		return dbPath, true
	}
	return "", false
}

func (s dbBackedSourceSet) newSourceRef(
	root, dbPath, sessionID, virtualPath string,
) SourceRef {
	return SourceRef{
		Provider:       s.spec.agent,
		Key:            virtualPath,
		DisplayPath:    virtualPath,
		FingerprintKey: virtualPath,
		Opaque: dbBackedSource{
			Root:      root,
			DBPath:    dbPath,
			SessionID: sessionID,
		},
	}
}

func (s dbBackedSource) virtualPath() string {
	return s.DBPath + "#" + s.SessionID
}

func parseDBBackedVirtualPath(path string) (string, string, bool) {
	idx := strings.LastIndex(path, "#")
	if idx < 0 {
		return "", "", false
	}
	dbPath, sessionID := path[:idx], path[idx+1:]
	if dbPath == "" || sessionID == "" {
		return "", "", false
	}
	return dbPath, sessionID, true
}

func newForgeProviderFactory(def AgentDef) ProviderFactory {
	return dbBackedProviderFactory{
		def:  cloneAgentDef(def),
		spec: forgeProviderSpec(),
	}
}

func forgeProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentForge,
		dbName: ForgeDBFilename,
		findDB: forgeDBPath,
		listMeta: func(dbPath string) ([]dbBackedSessionMeta, error) {
			metas, err := ListForgeSessionMeta(dbPath)
			out := make([]dbBackedSessionMeta, 0, len(metas))
			for _, meta := range metas {
				out = append(out, dbBackedSessionMeta(meta))
			}
			return out, err
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			sess, msgs, err := parseForgeSession(dbPath, sessionID, machine)
			if err != nil || sess == nil {
				return nil, err
			}
			return []ParseResult{{Session: *sess, Messages: msgs}}, nil
		},
		caps: forgeProviderCapabilities(),
	}
}

func newPiebaldProviderFactory(def AgentDef) ProviderFactory {
	return dbBackedProviderFactory{
		def:  cloneAgentDef(def),
		spec: piebaldProviderSpec(),
	}
}

func piebaldProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentPiebald,
		dbName: PiebaldDBFilename,
		findDB: piebaldDBPath,
		listMeta: func(dbPath string) ([]dbBackedSessionMeta, error) {
			metas, err := ListPiebaldSessionMeta(dbPath)
			out := make([]dbBackedSessionMeta, 0, len(metas))
			for _, meta := range metas {
				out = append(out, dbBackedSessionMeta(meta))
			}
			return out, err
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			return parsePiebaldSessionResults(dbPath, sessionID, machine)
		},
		normalizeRaw: func(raw string) string {
			chatID, _, _ := strings.Cut(raw, "-")
			return chatID
		},
		caps: piebaldProviderCapabilities(),
	}
}

func newWarpProviderFactory(def AgentDef) ProviderFactory {
	return dbBackedProviderFactory{
		def:  cloneAgentDef(def),
		spec: warpProviderSpec(),
	}
}

func warpProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentWarp,
		dbName: WarpDBFilename,
		findDB: warpDBPath,
		listMeta: func(dbPath string) ([]dbBackedSessionMeta, error) {
			metas, err := ListWarpSessionMeta(dbPath)
			out := make([]dbBackedSessionMeta, 0, len(metas))
			for _, meta := range metas {
				out = append(out, dbBackedSessionMeta(meta))
			}
			return out, err
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			sess, msgs, err := parseWarpSession(dbPath, sessionID, machine)
			if err != nil || sess == nil {
				return nil, err
			}
			return []ParseResult{{Session: *sess, Messages: msgs}}, nil
		},
		caps: warpProviderCapabilities(),
	}
}

func dbBackedSourceCapabilities(multiSession CapabilitySupport) SourceCapabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.MultiSessionSource = multiSession
	source.ForceReplaceOnParse = CapabilitySupported
	return source
}

func forgeProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilityNotApplicable),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}

func piebaldProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilitySupported),
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			GitBranch:            CapabilitySupported,
			Relationships:        CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
			StopReason:           CapabilitySupported,
		},
	}
}

func warpProviderCapabilities() Capabilities {
	return Capabilities{
		Source: dbBackedSourceCapabilities(CapabilityNotApplicable),
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			Cwd:          CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			Model:        CapabilitySupported,
		},
	}
}
