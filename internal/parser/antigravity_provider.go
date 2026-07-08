package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var _ Provider = (*antigravityProvider)(nil)

type antigravityProviderFactory struct {
	def AgentDef
}

func newAntigravityProviderFactory(def AgentDef) ProviderFactory {
	return antigravityProviderFactory{def: cloneAgentDef(def)}
}

func (f antigravityProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f antigravityProviderFactory) Capabilities() Capabilities {
	return antigravityProviderCapabilities()
}

func (f antigravityProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &antigravityProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   antigravityProviderCapabilities(),
			Config: cfg,
		},
		sources: newAntigravitySourceSet(cfg.Roots),
	}
}

type antigravityProvider struct {
	ProviderBase
	sources antigravitySourceSet
}

func (p *antigravityProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *antigravityProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *antigravityProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *antigravityProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *antigravityProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *antigravityProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("antigravity source path unavailable")
	}
	if _, err := os.Stat(src.Path); err != nil {
		if os.IsNotExist(err) {
			return ParseOutcome{
				ResultSetComplete: true,
				ForceReplace:      true,
				SkipReason:        SkipNoSession,
			}, nil
		}
		return ParseOutcome{}, fmt.Errorf("stat %s: %w", src.Path, err)
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, usageEvents, err := p.parseSession(
		src.Path,
		req.Source.ProjectHint,
		machine,
	)
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
	if req.Fingerprint.Hash != "" {
		sess.File.Hash = req.Fingerprint.Hash
	}
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: usageEvents,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type antigravitySource struct {
	Root string
	Path string
	ID   string
}

type antigravitySourceSet struct {
	roots []string
}

func newAntigravitySourceSet(roots []string) antigravitySourceSet {
	return antigravitySourceSet{roots: cleanJSONLRoots(roots)}
}

func (s antigravitySourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, path := range s.discoverSessionPaths(root) {
			source, ok := s.sourceRef(root, path, false)
			if ok {
				addJSONLSource(source, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverSessionPaths returns one conversations/<uuid>.db path per IDE session
// under root, sorted by path. It owns the on-disk discovery the package-level
// DiscoverAntigravitySessions free function used to provide.
func (s antigravitySourceSet) discoverSessionPaths(root string) []string {
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, "conversations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if !IsValidSessionID(id) {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	slices.Sort(paths)
	return paths
}

// findSourceFile locates an IDE session DB by id under root. It owns the lookup
// the package-level FindAntigravitySourceFile free function used to provide.
func (s antigravitySourceSet) findSourceFile(root, id string) string {
	if root == "" || !IsValidSessionID(id) {
		return ""
	}
	p := filepath.Join(root, "conversations", id+".db")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func (s antigravitySourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*3)
	for _, root := range s.roots {
		roots = append(roots,
			WatchRoot{
				Path:         filepath.Join(root, "annotations"),
				Recursive:    false,
				IncludeGlobs: []string{"*.pbtxt"},
				DebounceKey:  string(AgentAntigravity) + ":annotations:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(root, "brain"),
				Recursive:    true,
				IncludeGlobs: []string{"*.md", "*.md.metadata.json"},
				DebounceKey:  string(AgentAntigravity) + ":brain:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(root, "conversations"),
				Recursive:    false,
				IncludeGlobs: []string{"*.db", "*.db-*", "*.trajectory.json"},
				DebounceKey:  string(AgentAntigravity) + ":conversations:" + root,
			},
		)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s antigravitySourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !antigravityWatchRootMatches(root, req.WatchRoot) {
			continue
		}
		source, ok := s.sourceForChangedPath(root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s antigravitySourceSet) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	if err := ctx.Err(); err != nil {
		return SourceRef{}, false, err
	}
	freshStoredSource := req.RequireFreshSource &&
		(req.StoredFilePath != "" || req.FingerprintKey != "")
	for _, path := range []string{req.StoredFilePath, req.FingerprintKey} {
		if path == "" {
			continue
		}
		for _, root := range s.roots {
			if source, ok := s.sourceRef(root, path, true); ok {
				src := source.Opaque.(antigravitySource)
				if req.RawSessionID != "" && src.ID != req.RawSessionID {
					continue
				}
				if req.RequireFreshSource && !IsRegularFile(src.Path) {
					continue
				}
				return source, true, nil
			}
		}
	}
	if freshStoredSource {
		return SourceRef{}, false, nil
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path, false); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s antigravitySourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("antigravity source path unavailable")
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path)
	info, err := AntigravityFileInfo(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, err
	}
	hash, err := antigravityCompositeHash(
		src.Path,
		antigravityIDECompanionPaths(src.Path)...,
	)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     key,
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s antigravitySourceSet) sourceFromRef(source SourceRef) (antigravitySource, bool) {
	switch src := source.Opaque.(type) {
	case antigravitySource:
		return src, src.Path != ""
	case *antigravitySource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, true); ok {
				src := ref.Opaque.(antigravitySource)
				return src, true
			}
		}
	}
	return antigravitySource{}, false
}

func (s antigravitySourceSet) sourceForChangedPath(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if dbPath, id, ok := antigravityConversationDBForPath(root, path); ok {
		return s.newSourceRef(root, dbPath, id), true
	}
	if id, ok := antigravityIDETrajectoryID(root, path); ok {
		dbPath := filepath.Join(root, "conversations", id+".db")
		if IsRegularFile(dbPath) {
			return s.newSourceRef(root, dbPath, id), true
		}
	}
	if id, ok := antigravityAnnotationID(root, path); ok {
		dbPath := filepath.Join(root, "conversations", id+".db")
		if IsRegularFile(dbPath) {
			return s.newSourceRef(root, dbPath, id), true
		}
	}
	if id, ok := antigravityBrainID(root, path); ok {
		dbPath := filepath.Join(root, "conversations", id+".db")
		if IsRegularFile(dbPath) {
			return s.newSourceRef(root, dbPath, id), true
		}
	}
	return SourceRef{}, false
}

func (s antigravitySourceSet) sourceRef(
	root, path string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	dbPath, id, ok := antigravityConversationDBForPath(root, path)
	if !ok || dbPath != path {
		return SourceRef{}, false
	}
	if !allowMissing && !IsRegularFile(path) {
		return SourceRef{}, false
	}
	return s.newSourceRef(root, path, id), true
}

func (s antigravitySourceSet) newSourceRef(root, path, id string) SourceRef {
	return SourceRef{
		Provider:       AgentAntigravity,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: antigravitySource{
			Root: root,
			Path: path,
			ID:   id,
		},
	}
}

func antigravityConversationDBForPath(root, path string) (string, string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "conversations" {
		return "", "", false
	}
	name := strings.TrimSuffix(parts[1], "-wal")
	name = strings.TrimSuffix(name, "-shm")
	if !strings.HasSuffix(name, ".db") {
		return "", "", false
	}
	id := strings.TrimSuffix(name, ".db")
	if !IsValidSessionID(id) {
		return "", "", false
	}
	return filepath.Join(root, "conversations", id+".db"), id, true
}

// antigravityIDETrajectoryID reports whether path is a
// conversations/<id>.trajectory.json agy-reader sidecar under root and,
// if so, returns the session id so a sidecar write routes back to its
// .db source for re-sync.
func antigravityIDETrajectoryID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "conversations" ||
		!strings.HasSuffix(parts[1], ".trajectory.json") {
		return "", false
	}
	id := strings.TrimSuffix(parts[1], ".trajectory.json")
	return id, IsValidSessionID(id)
}

func antigravityAnnotationID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != "annotations" ||
		!strings.HasSuffix(parts[1], ".pbtxt") {
		return "", false
	}
	id := strings.TrimSuffix(parts[1], ".pbtxt")
	return id, IsValidSessionID(id)
}

func antigravityBrainID(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] != "brain" {
		return "", false
	}
	return parts[1], IsValidSessionID(parts[1])
}

func antigravityWatchRootMatches(root, watchRoot string) bool {
	watchRoot = filepath.Clean(watchRoot)
	for _, subdir := range []string{"annotations", "brain", "conversations"} {
		if samePath(watchRoot, filepath.Join(root, subdir)) {
			return true
		}
	}
	return samePath(watchRoot, filepath.Clean(root))
}

func antigravityProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.ForceReplaceOnParse = CapabilitySupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
	}
}
