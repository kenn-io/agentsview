package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var _ Provider = (*antigravityCLIProvider)(nil)

type antigravityCLIProviderFactory struct {
	def AgentDef
}

func newAntigravityCLIProviderFactory(def AgentDef) ProviderFactory {
	return antigravityCLIProviderFactory{def: cloneAgentDef(def)}
}

func (f antigravityCLIProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f antigravityCLIProviderFactory) Capabilities() Capabilities {
	return antigravityCLIProviderCapabilities()
}

func (f antigravityCLIProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &antigravityCLIProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   antigravityCLIProviderCapabilities(),
			Config: cfg,
		},
		sources: newAntigravityCLISourceSet(cfg.Roots),
	}
}

type antigravityCLIProvider struct {
	ProviderBase
	sources antigravityCLISourceSet
}

func (p *antigravityCLIProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *antigravityCLIProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *antigravityCLIProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *antigravityCLIProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *antigravityCLIProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *antigravityCLIProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *antigravityCLIProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	src, ok := p.sources.sourceFromRef(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("antigravity cli source path unavailable")
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
	sess, msgs, usageEvents, status, err := p.parseSessionWithStatus(
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
	result := ParseResultOutcome{
		Result: ParseResult{
			Session:     *sess,
			Messages:    msgs,
			UsageEvents: usageEvents,
		},
		DataVersion: DataVersionCurrent,
	}
	if status.NeedsRetry {
		result.DataVersion = DataVersionNeedsRetry
		result.RetryReason = "antigravity cli source needs high-fidelity retry"
	}
	return ParseOutcome{
		Results:           []ParseResultOutcome{result},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

type antigravityCLISource struct {
	Root    string
	Path    string
	ID      string
	Project string
}

type antigravityCLISourceSet struct {
	roots []string
}

func newAntigravityCLISourceSet(roots []string) antigravityCLISourceSet {
	roots = cleanJSONLRoots(roots)
	return antigravityCLISourceSet{
		roots: roots,
	}
}

func (s antigravityCLISourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files, projects := s.discoverSessionsAndProjects(root)
		for _, file := range files {
			// Thread the prebuilt project map so per-source resolution does not
			// re-read and re-parse history.jsonl for every project-less source.
			source, ok := s.sourceRefWithProjects(
				root, file.Path, file.Project, projects, false,
			)
			if ok {
				addJSONLSource(source, &sources, seen)
			}
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s antigravityCLISourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		projects, err := newDiscoveryDiskMapForContext(ctx)
		if err != nil {
			return err
		}
		if err := projects.loadJSONL(
			ctx, filepath.Join(root, "history.jsonl"), "conversationId", "workspace",
		); err != nil {
			return errors.Join(err, projects.close())
		}
		for _, subdir := range []string{"conversations", "implicit"} {
			dir := filepath.Join(root, subdir)
			err := streamDirectoryEntries(ctx, dir, func(entry os.DirEntry) error {
				if entry.IsDir() {
					return nil
				}
				_, ext, ok := antigravityCLIPathID(entry.Name())
				if !ok || subdir == "implicit" && ext != ".pb" {
					return nil
				}
				path := filepath.Join(dir, entry.Name())
				rawID, ok := antigravityCLISessionIDForPath(root, path)
				if !ok {
					return nil
				}
				project, _, err := projects.get(ctx, strings.TrimPrefix(rawID, antigravityImplicitTag))
				if err != nil {
					return err
				}
				return yield(s.newSourceRef(root, path, rawID, project))
			})
			if err != nil {
				return errors.Join(err, projects.close())
			}
		}
		if err := projects.close(); err != nil {
			return err
		}
	}
	return nil
}

// discoverSessions enumerates conversations/*.db, conversations/*.pb, and
// implicit/*.pb under the CLI root and tags each with its workspace (resolved
// via history.jsonl). It owns the on-disk discovery the package-level
// DiscoverAntigravityCLISessions free function used to provide. The result
// keeps the legacy DiscoveredFile shape so the project hint travels with each
// path.
func (s antigravityCLISourceSet) discoverSessions(root string) []DiscoveredFile {
	files, _ := s.discoverSessionsAndProjects(root)
	return files
}

// discoverSessionsAndProjects enumerates sessions and also returns the project
// map it built from history.jsonl, so callers can thread the same map into
// per-source resolution instead of rebuilding it (the map is a full read and
// per-line parse of history.jsonl).
func (s antigravityCLISourceSet) discoverSessionsAndProjects(
	root string,
) ([]DiscoveredFile, map[string]string) {
	if root == "" {
		return nil, nil
	}
	projects := buildAntigravityProjectMap(
		filepath.Join(root, "history.jsonl"),
	)
	var files []DiscoveredFile
	for _, sub := range []string{"conversations", "implicit"} {
		dir := filepath.Join(root, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		byID := make(map[string]string)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			id, ext, ok := antigravityCLIPathID(name)
			if !ok || (sub == "implicit" && ext != ".pb") {
				continue
			}
			// Prefer the new SQLite source when both old and new files
			// exist for a conversation. They share a storage ID.
			if prev := byID[id]; prev == "" ||
				(strings.HasSuffix(prev, ".pb") && ext == ".db") {
				byID[id] = filepath.Join(dir, name)
			}
		}
		for id, path := range byID {
			files = append(files, DiscoveredFile{
				Path:    path,
				Project: projects[id],
				Agent:   AgentAntigravityCLI,
			})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, projects
}

// findSourceFile locates the source file for a session id (without the agent
// prefix). An "implicit-" prefix routes to the implicit/ subdir; bare ids
// resolve under conversations/. It owns the lookup the package-level
// FindAntigravityCLISourceFile free function used to provide.
func (s antigravityCLISourceSet) findSourceFile(root, id string) string {
	if root == "" {
		return ""
	}
	if uuid, ok := strings.CutPrefix(id, antigravityImplicitTag); ok {
		if !IsValidSessionID(uuid) {
			return ""
		}
		for _, ext := range []string{".pb"} {
			p := filepath.Join(root, "implicit", uuid+ext)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	if !IsValidSessionID(id) {
		return ""
	}
	for _, ext := range []string{".db", ".pb"} {
		p := filepath.Join(root, "conversations", id+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func (s antigravityCLISourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*4)
	for _, root := range s.roots {
		roots = append(roots,
			WatchRoot{
				Path:         filepath.Join(root, "brain"),
				Recursive:    true,
				IncludeGlobs: []string{"*.md", "*.md.metadata.json"},
				DebounceKey:  string(AgentAntigravityCLI) + ":brain:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(root, "conversations"),
				Recursive:    false,
				IncludeGlobs: []string{"*.db", "*.db-*", "*.pb", "*.trajectory.json"},
				DebounceKey:  string(AgentAntigravityCLI) + ":conversations:" + root,
			},
			WatchRoot{
				Path:         root,
				Recursive:    false,
				IncludeGlobs: []string{"history.jsonl"},
				DebounceKey:  string(AgentAntigravityCLI) + ":history:" + root,
			},
			WatchRoot{
				Path:         filepath.Join(root, "implicit"),
				Recursive:    false,
				IncludeGlobs: []string{"*.pb", "*.trajectory.json"},
				DebounceKey:  string(AgentAntigravityCLI) + ":implicit:" + root,
			},
		)
	}
	return WatchPlan{Roots: roots}, nil
}

func (s antigravityCLISourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if req.WatchRoot != "" && !antigravityCLIWatchRootMatches(root, req.WatchRoot) {
			continue
		}
		if sources := s.sourcesForChangedPath(root, req); len(sources) > 0 {
			return sources, nil
		}
	}
	return nil, nil
}

func (s antigravityCLISourceSet) FindSource(
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
			if source, ok := s.storedSourceRef(
				root, path, req.RawSessionID, req.RequireFreshSource,
			); ok {
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
	projects := make(map[string]map[string]string)
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		project := ""
		id := strings.TrimPrefix(req.RawSessionID, antigravityImplicitTag)
		if projects[root] == nil {
			projects[root] = buildAntigravityProjectMap(
				filepath.Join(root, "history.jsonl"),
			)
		}
		project = projects[root][id]
		if source, ok := s.sourceRef(root, path, project, false); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

func (s antigravityCLISourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := s.sourceFromRef(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("antigravity cli source path unavailable")
	}
	key := firstNonEmptyJSONLString(source.FingerprintKey, source.Key, src.Path)
	info, err := AntigravityCLIFileInfo(src.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return SourceFingerprint{Key: key}, nil
		}
		return SourceFingerprint{}, err
	}
	hash, err := antigravityCLICompositeHash(src.Path, src.ID)
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

func (s antigravityCLISourceSet) sourceFromRef(
	source SourceRef,
) (antigravityCLISource, bool) {
	switch src := source.Opaque.(type) {
	case antigravityCLISource:
		return src, src.Path != ""
	case *antigravityCLISource:
		if src != nil && src.Path != "" {
			return *src, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate, source.ProjectHint, true); ok {
				src := ref.Opaque.(antigravityCLISource)
				return src, true
			}
		}
	}
	return antigravityCLISource{}, false
}

func (s antigravityCLISourceSet) sourcesForChangedPath(
	root string,
	req ChangedPathRequest,
) []SourceRef {
	root = filepath.Clean(root)
	path := filepath.Clean(req.Path)
	if samePath(path, filepath.Join(root, "history.jsonl")) {
		return s.sourcesForHistoryChange(root, req)
	}
	if sourcePath, id, ok := antigravityCLISourcePathForEvent(root, path); ok {
		if source, ok := s.sourceRef(root, sourcePath, s.projectForID(root, id), true); ok {
			return []SourceRef{source}
		}
	}
	if id, ok := antigravityBrainID(root, path); ok {
		var sources []SourceRef
		for _, sourcePath := range []string{
			antigravityCLIConversationSource(root, id),
			filepath.Join(root, "implicit", id+".pb"),
		} {
			if sourcePath == "" || !IsRegularFile(sourcePath) {
				continue
			}
			source, ok := s.sourceRef(root, sourcePath, s.projectForID(root, id), false)
			if ok {
				sources = append(sources, source)
			}
		}
		sortJSONLSources(sources)
		return sources
	}
	return nil
}

func (s antigravityCLISourceSet) sourcesForHistoryChange(
	root string,
	_ ChangedPathRequest,
) []SourceRef {
	return s.sourcesForUntaggedHistoryChange(root)
}

func (s antigravityCLISourceSet) sourcesForUntaggedHistoryChange(root string) []SourceRef {
	var sources []SourceRef
	seen := make(map[string]struct{})
	files, projects := s.discoverSessionsAndProjects(root)
	for _, file := range files {
		source, ok := s.sourceRefWithProjects(
			root, file.Path, file.Project, projects, false,
		)
		if ok {
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources
}

func (s antigravityCLISourceSet) storedSourceRef(
	root, path, rawSessionID string,
	requireFresh bool,
) (SourceRef, bool) {
	id, ok := antigravityCLISessionIDForPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	if rawSessionID != "" && id != rawSessionID {
		return SourceRef{}, false
	}
	projectID := strings.TrimPrefix(id, antigravityImplicitTag)
	if currentPath := s.findSourceFile(root, id); currentPath != "" {
		return s.sourceRef(root, currentPath, s.projectForID(root, projectID), false)
	}
	if requireFresh {
		return SourceRef{}, false
	}
	return s.sourceRef(root, path, s.projectForID(root, projectID), true)
}

func (s antigravityCLISourceSet) sourceRef(
	root, path, project string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	id, ok := antigravityCLISessionIDForPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	if !allowMissing && !IsRegularFile(path) {
		return SourceRef{}, false
	}
	if project == "" {
		project = s.projectForID(root, strings.TrimPrefix(id, antigravityImplicitTag))
	}
	return s.newSourceRef(root, path, id, project), true
}

// sourceRefWithProjects is sourceRef but resolves a missing project from a
// prebuilt project map instead of rebuilding it from history.jsonl per call.
// Discovery builds the map once per root and threads it for every source.
func (s antigravityCLISourceSet) sourceRefWithProjects(
	root, path, project string,
	projects map[string]string,
	allowMissing bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	id, ok := antigravityCLISessionIDForPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	if !allowMissing && !IsRegularFile(path) {
		return SourceRef{}, false
	}
	if project == "" {
		project = projects[strings.TrimPrefix(id, antigravityImplicitTag)]
	}
	return s.newSourceRef(root, path, id, project), true
}

func (s antigravityCLISourceSet) newSourceRef(
	root, path, id, project string,
) SourceRef {
	return SourceRef{
		Provider:       AgentAntigravityCLI,
		Key:            id,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: antigravityCLISource{
			Root:    root,
			Path:    path,
			ID:      id,
			Project: project,
		},
	}
}

func (s antigravityCLISourceSet) projectForID(root, id string) string {
	return buildAntigravityProjectMap(filepath.Join(root, "history.jsonl"))[id]
}

func antigravityCLISourcePathForEvent(root, path string) (string, string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || (parts[0] != "conversations" && parts[0] != "implicit") {
		return "", "", false
	}
	name := parts[1]
	switch {
	case strings.HasSuffix(name, ".db") ||
		strings.HasSuffix(name, ".db-wal") ||
		strings.HasSuffix(name, ".db-shm"):
		if parts[0] != "conversations" {
			return "", "", false
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, "-wal"), "-shm")
		id := strings.TrimSuffix(base, ".db")
		if !IsValidSessionID(id) {
			return "", "", false
		}
		return filepath.Join(root, "conversations", id+".db"), id, true
	case strings.HasSuffix(name, ".pb"):
		id := strings.TrimSuffix(name, ".pb")
		if !IsValidSessionID(id) {
			return "", "", false
		}
		if parts[0] == "conversations" {
			if dbPath := antigravityCLIConversationSource(root, id); dbPath != "" {
				return dbPath, id, true
			}
		}
		return filepath.Join(root, parts[0], id+".pb"), id, true
	case strings.HasSuffix(name, ".trajectory.json"):
		id := strings.TrimSuffix(name, ".trajectory.json")
		if !IsValidSessionID(id) {
			return "", "", false
		}
		if parts[0] == "conversations" {
			sourcePath := antigravityCLIConversationSource(root, id)
			if sourcePath == "" {
				return "", "", false
			}
			return sourcePath, id, true
		}
		sourcePath := filepath.Join(root, parts[0], id+".pb")
		if !IsRegularFile(sourcePath) {
			return "", "", false
		}
		return sourcePath, id, true
	default:
		return "", "", false
	}
}

func antigravityCLIConversationSource(root, id string) string {
	for _, path := range []string{
		filepath.Join(root, "conversations", id+".db"),
		filepath.Join(root, "conversations", id+".pb"),
	} {
		if IsRegularFile(path) {
			return path
		}
	}
	return ""
}

func antigravityCLISessionIDForPath(root, path string) (string, bool) {
	rel, ok := relUnder(filepath.Clean(root), filepath.Clean(path))
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || (parts[0] != "conversations" && parts[0] != "implicit") {
		return "", false
	}
	id, ext, ok := antigravityCLIPathID(parts[1])
	if !ok {
		return "", false
	}
	if parts[0] == "implicit" {
		if ext != ".pb" {
			return "", false
		}
		return antigravityImplicitTag + id, true
	}
	return id, true
}

func antigravityCLIWatchRootMatches(root, watchRoot string) bool {
	watchRoot = filepath.Clean(watchRoot)
	for _, subdir := range []string{"brain", "conversations", "implicit"} {
		if samePath(watchRoot, filepath.Join(root, subdir)) {
			return true
		}
	}
	return samePath(watchRoot, filepath.Clean(root))
}

func antigravityCLIProviderCapabilities() Capabilities {
	source := jsonlFileProviderSourceCapabilities()
	source.StreamingDiscovery = CapabilitySupported
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
