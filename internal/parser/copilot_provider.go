package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*copilotProvider)(nil)

type copilotProviderFactory struct {
	def   AgentDef
	cache *copilotSourceCache
}

func newCopilotProviderFactory(def AgentDef) ProviderFactory {
	return copilotProviderFactory{def: cloneAgentDef(def), cache: newCopilotSourceCache()}
}

func (f copilotProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f copilotProviderFactory) Capabilities() Capabilities {
	return copilotProviderCapabilities()
}

func (f copilotProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &copilotProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    copilotProviderCapabilities(),
		Config:  cfg,
		sources: newCopilotSourceSet(cfg.Roots, f.cache),
	}
}

type copilotProvider struct {
	ProviderBase
	sources copilotSourceSet
}

func (p *copilotProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *copilotProvider) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	return p.sources.DiscoverEach(ctx, yield)
}

func (p *copilotProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *copilotProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *copilotProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *copilotProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *copilotProvider) FingerprintWithStored(ctx context.Context, source SourceRef, load StoredFingerprintLookup) (SourceFingerprint, error) {
	return p.sources.fingerprint(ctx, source, load)
}

func (p *copilotProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("copilot source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	storePath := ""
	if source, ok := req.Source.Opaque.(copilotSource); ok {
		storePath = filepath.Join(source.Root, "session-store.db")
	}
	sess, msgs, usage, err := p.parseSessionWithStore(path, machine, storePath)
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
	if req.Fingerprint.Size > 0 {
		sess.File.Size = req.Fingerprint.Size
	}
	if req.Fingerprint.MTimeNS > 0 {
		sess.File.Mtime = req.Fingerprint.MTimeNS
	}
	sess.UsageEvents = usage
	return ParseOutcome{
		Results: []ParseResultOutcome{{
			Result: ParseResult{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: usage,
			},
			DataVersion: DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}, nil
}

type copilotSource struct {
	Root string
	Path string
}

type copilotSourceSet struct {
	roots []string
	cache *copilotSourceCache
}

func newCopilotSourceSet(roots []string, cache *copilotSourceCache) copilotSourceSet {
	return copilotSourceSet{roots: cleanJSONLRoots(roots), cache: cache}
}

func (s copilotSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	err := s.DiscoverEach(ctx, func(source SourceRef) error {
		addJSONLSource(source, &sources, seen)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s copilotSourceSet) DiscoverEach(ctx context.Context, yield func(SourceRef) error) error {
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		seen := make(map[string]bool)
		stateDir := filepath.Join(root, copilotStateDir)
		err := streamDirectoryEntries(ctx, stateDir, func(entry os.DirEntry) error {
			name := entry.Name()
			path := ""
			if entry.IsDir() {
				candidate := filepath.Join(stateDir, name, "events.jsonl")
				if IsRegularFile(candidate) {
					path = candidate
				}
			} else if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
				if !IsRegularFile(filepath.Join(stateDir, stem, "events.jsonl")) {
					path = filepath.Join(stateDir, name)
				}
			}
			if path != "" {
				if source, ok := s.sourceRef(root, path); ok {
					seen[path] = true
					return yield(source)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		s.cache.pruneTranscripts(root, seen)
	}
	return nil
}

// discoverSessionPaths finds all Copilot session file paths under
// <root>/session-state/. It supports both the bare layout (<uuid>.jsonl) and
// the directory layout (<uuid>/events.jsonl); when both exist for the same
// session, the directory layout wins and the bare file is dropped so a session
// is not discovered twice.
func (s copilotSourceSet) discoverSessionPaths(root string) []string {
	if root == "" {
		return nil
	}

	stateDir := filepath.Join(root, copilotStateDir)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}

	dirs := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		eventsPath := filepath.Join(stateDir, entry.Name(), "events.jsonl")
		if _, err := os.Stat(eventsPath); err == nil {
			dirs[entry.Name()] = struct{}{}
		}
	}

	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			candidate := filepath.Join(stateDir, name, "events.jsonl")
			if _, err := os.Stat(candidate); err == nil {
				paths = append(paths, candidate)
			}
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
			if _, dup := dirs[stem]; dup {
				continue
			}
			paths = append(paths, filepath.Join(stateDir, name))
		}
	}
	return paths
}

func (s copilotSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots)*2)
	for _, root := range s.roots {
		stateDir := filepath.Join(root, copilotStateDir)
		roots = append(roots, WatchRoot{
			Path:         stateDir,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "workspace.yaml"},
			DebounceKey:  string(AgentCopilot) + ":state:" + stateDir,
		})
		roots = append(roots, WatchRoot{
			Path:         root,
			IncludeGlobs: []string{"session-store.db", "session-store.db-wal"},
			DebounceKey:  string(AgentCopilot) + ":store:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s copilotSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range s.roots {
		if copilotStoreChangedPath(root, req.Path) {
			var sources []SourceRef
			for _, path := range s.discoverSessionPaths(root) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if source, ok := s.sourceRef(root, path); ok {
					sources = append(sources, source)
				}
			}
			sortJSONLSources(sources)
			return sources, nil
		}
		source, ok := s.sourceForChangedPath(root, req)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s copilotSourceSet) FindSource(
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
			if source, ok := s.sourceRef(root, path); ok {
				return source, true, nil
			}
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.findSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// findSourceFile locates a Copilot session file by UUID under root. It checks
// the directory layout (<uuid>/events.jsonl) first, then the bare layout
// (<uuid>.jsonl), so the richer directory form takes precedence. Returns "" for
// invalid IDs or when no file resolves.
func (s copilotSourceSet) findSourceFile(root, rawID string) string {
	if root == "" || !IsValidSessionID(rawID) {
		return ""
	}

	stateDir := filepath.Join(root, copilotStateDir)

	dirFmt := filepath.Join(stateDir, rawID, "events.jsonl")
	if _, err := os.Stat(dirFmt); err == nil {
		return dirFmt
	}

	bare := filepath.Join(stateDir, rawID+".jsonl")
	if _, err := os.Stat(bare); err == nil {
		return bare
	}

	return ""
}

func (s copilotSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return s.fingerprint(ctx, source, nil)
}

func (s copilotSourceSet) fingerprint(ctx context.Context, source SourceRef, load StoredFingerprintLookup) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("copilot source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	transcript, err := s.cache.transcript(ctx, path, info, load)
	if err != nil {
		return SourceFingerprint{}, err
	}
	storeHash := ""
	if transcript.UsesStore {
		storePath := filepath.Join(copilotRootForEventsPath(path), "session-store.db")
		storeHash, err = s.cache.usageHash(ctx, storePath, transcript.SessionID)
		if err != nil {
			if ctx.Err() != nil {
				return SourceFingerprint{}, ctx.Err()
			}
			// Preserve the parser's transcript-only fallback when the optional
			// store cannot be read. Failed reads are not cached, so recovery is
			// retried on the next fingerprint even without a file change.
			state, _ := StatSQLiteContainerState(storePath)
			storeHash = fmt.Sprintf("unavailable:%v", state)
		}
	}
	encoded, err := transcript.encode()
	if err != nil {
		return SourceFingerprint{}, err
	}
	fingerprint := SourceFingerprint{
		Key:  firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size: transcript.Size, MTimeNS: transcript.Mtime,
		Hash: fmt.Sprintf("copilot-session:v3:%s:%s", encoded, storeHash),
	}
	return fingerprint, nil
}

func (s copilotSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case copilotSource:
		return src.Path, src.Path != ""
	case *copilotSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		for _, root := range s.roots {
			if ref, ok := s.sourceRef(root, candidate); ok {
				src := ref.Opaque.(copilotSource)
				return src.Path, true
			}
		}
	}
	return "", false
}

func (s copilotSourceSet) sourceForChangedPath(
	root string,
	req ChangedPathRequest,
) (SourceRef, bool) {
	path := req.Path
	if filepath.Base(path) == "workspace.yaml" {
		return s.sourceRef(root, filepath.Join(filepath.Dir(path), "events.jsonl"))
	}
	if source, ok := s.sourceRef(root, path); ok {
		return source, true
	}
	if !jsonlMissingPathFallbackAllowed(req) {
		return SourceRef{}, false
	}
	if filepath.Base(path) == "events.jsonl" {
		barePath := filepath.Join(
			root,
			copilotStateDir,
			filepath.Base(filepath.Dir(path))+".jsonl",
		)
		if source, ok := s.sourceRef(root, barePath); ok {
			return source, true
		}
	}
	return s.sourceRefForPath(root, path, false)
}

func (s copilotSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	return s.sourceRefForPath(root, path, true)
}

func (s copilotSourceSet) sourceRefForPath(
	root, path string,
	requireRegular bool,
) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, ok := relUnder(root, path)
	if !ok || (requireRegular && !IsRegularFile(path)) {
		return SourceRef{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 3 &&
		parts[0] == copilotStateDir &&
		parts[2] == "events.jsonl" {
		return s.newSourceRef(root, path), true
	}
	if len(parts) == 2 &&
		parts[0] == copilotStateDir &&
		strings.HasSuffix(parts[1], ".jsonl") {
		stem := strings.TrimSuffix(parts[1], ".jsonl")
		if dirPath := s.findSourceFile(root, stem); dirPath != "" &&
			dirPath != path {
			return s.sourceRef(root, dirPath)
		}
		return s.newSourceRef(root, path), true
	}
	return SourceRef{}, false
}

func (s copilotSourceSet) newSourceRef(root, path string) SourceRef {
	return SourceRef{
		Provider:       AgentCopilot,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: copilotSource{
			Root: root,
			Path: path,
		},
	}
}

func copilotWorkspacePath(eventsPath string) string {
	if filepath.Base(eventsPath) != "events.jsonl" {
		return ""
	}
	return filepath.Join(filepath.Dir(eventsPath), "workspace.yaml")
}

func copilotRootForEventsPath(eventsPath string) string {
	stateDir := filepath.Dir(eventsPath)
	if filepath.Base(eventsPath) == "events.jsonl" {
		stateDir = filepath.Dir(stateDir)
	}
	if filepath.Base(stateDir) != copilotStateDir {
		return ""
	}
	return filepath.Dir(stateDir)
}

// CopilotCompositeFileStat describes only this transcript and its workspace.
// Shared store writes are compared through per-session usage fingerprints.
func CopilotCompositeFileStat(eventsPath string, info os.FileInfo) (size, mtime int64) {
	size, mtime = info.Size(), info.ModTime().UnixNano()
	if workspace := copilotWorkspacePath(eventsPath); workspace != "" {
		if ws, err := os.Stat(workspace); err == nil && !ws.IsDir() {
			size += ws.Size()
			mtime = max(mtime, ws.ModTime().UnixNano())
		}
	}
	return size, mtime
}

func copilotStoreChangedPath(root, path string) bool {
	if !samePath(filepath.Dir(path), root) {
		return false
	}
	switch filepath.Base(path) {
	case "session-store.db", "session-store.db-wal":
		return true
	default:
		return false
	}
}

func copilotProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			StreamingDiscovery:   CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			IncrementalAppend:    CapabilityNotApplicable,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			ToolResultEvents:     CapabilitySupported,
			PerMessageTokenUsage: CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashRequiredForFreshness: true,
		},
	}
}
