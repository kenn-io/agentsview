package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*openHandsProvider)(nil)

type openHandsProviderFactory struct {
	def AgentDef
}

func newOpenHandsProviderFactory(def AgentDef) ProviderFactory {
	return openHandsProviderFactory{def: cloneAgentDef(def)}
}

func (f openHandsProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f openHandsProviderFactory) Capabilities() Capabilities {
	return openHandsProviderCapabilities()
}

func (f openHandsProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &openHandsProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   openHandsProviderCapabilities(),
			Config: cfg,
		},
		sources: newOpenHandsSourceSet(cfg.Roots),
	}
}

type openHandsProvider struct {
	ProviderBase
	sources openHandsSourceSet
}

func (p *openHandsProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *openHandsProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *openHandsProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *openHandsProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *openHandsProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *openHandsProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("openhands source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
	sess, msgs, err := p.parseSession(path, machine)
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

type openHandsSource struct {
	Root string
	Path string
}

type openHandsSourceSet struct {
	roots []string
}

func newOpenHandsSourceSet(roots []string) openHandsSourceSet {
	return openHandsSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s openHandsSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !IsValidSessionID(entry.Name()) {
				continue
			}
			sessionDir := filepath.Join(root, entry.Name())
			source, ok := s.sourceRef(root, sessionDir)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

func (s openHandsSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:        root,
			Recursive:   false,
			DebounceKey: string(AgentOpenHands) + ":dir:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s openHandsSourceSet) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.WatchRoot != "" {
		root := filepath.Clean(req.WatchRoot)
		if !s.hasRoot(root) {
			return nil, nil
		}
		source, ok := s.sourceForPathInRoot(root, req.Path)
		if !ok {
			return nil, nil
		}
		return []SourceRef{source}, nil
	}
	for _, root := range s.roots {
		source, ok := s.sourceForPathInRoot(root, req.Path)
		if ok {
			return []SourceRef{source}, nil
		}
	}
	return nil, nil
}

func (s openHandsSourceSet) FindSource(
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
		if source, ok := s.sourceForPath(path); ok {
			return source, true, nil
		}
	}
	if req.RawSessionID == "" {
		return SourceRef{}, false, nil
	}
	for _, root := range s.roots {
		path := s.sessionDirForID(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// sessionDirForID locates an OpenHands conversation directory under
// root by its raw session ID. It first tries the raw ID and its
// dash-stripped form as literal directory names, then falls back to
// matching any session directory whose normalized ID equals the
// normalized raw ID.
func (s openHandsSourceSet) sessionDirForID(root, rawID string) string {
	if root == "" || !IsValidSessionID(rawID) {
		return ""
	}

	candidates := []string{rawID}
	stripped := strings.ReplaceAll(rawID, "-", "")
	if stripped != rawID {
		candidates = append(candidates, stripped)
	}
	for _, cand := range candidates {
		sessionDir := filepath.Join(root, cand)
		if isOpenHandsSessionDir(sessionDir) {
			return sessionDir
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionDir := filepath.Join(root, entry.Name())
		if !isOpenHandsSessionDir(sessionDir) {
			continue
		}
		if normalizeOpenHandsSessionID(entry.Name()) ==
			normalizeOpenHandsSessionID(rawID) {
			return sessionDir
		}
	}
	return ""
}

func (s openHandsSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("openhands source path unavailable")
	}
	snapshot, err := OpenHandsSnapshot(path)
	if err != nil {
		return SourceFingerprint{}, err
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    snapshot.Size,
		MTimeNS: snapshot.Mtime,
		Hash:    snapshot.Hash,
	}, nil
}

func (s openHandsSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case openHandsSource:
		return src.Path, src.Path != ""
	case *openHandsSource:
		if src != nil && src.Path != "" {
			return src.Path, true
		}
	}
	for _, candidate := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if ref, ok := s.sourceForPath(candidate); ok {
			src := ref.Opaque.(openHandsSource)
			return src.Path, true
		}
	}
	return "", false
}

func (s openHandsSourceSet) sourceForPath(path string) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForPathInRoot(root, path); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s openHandsSourceSet) sourceForPathInRoot(
	root string,
	path string,
) (SourceRef, bool) {
	sessionDir, ok := openHandsSessionDirForPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	return s.sourceRef(root, sessionDir)
}

func (s openHandsSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !isOpenHandsSessionDir(path) {
		return SourceRef{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.Contains(rel, string(filepath.Separator)) {
		return SourceRef{}, false
	}
	if !IsValidSessionID(rel) {
		return SourceRef{}, false
	}
	return SourceRef{
		Provider:       AgentOpenHands,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		Opaque: openHandsSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s openHandsSourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

func openHandsSessionDirForPath(root, path string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || !IsValidSessionID(parts[0]) {
		return "", false
	}
	switch len(parts) {
	case 1:
	case 2:
		if parts[1] != "base_state.json" && parts[1] != "TASKS.json" {
			return "", false
		}
	case 3:
		if parts[1] != "events" || filepath.Ext(parts[2]) != ".json" {
			return "", false
		}
	default:
		return "", false
	}
	return filepath.Join(root, parts[0]), true
}

func openHandsProviderCapabilities() Capabilities {
	return Capabilities{
		Source: SourceCapabilities{
			DiscoverSources:      CapabilitySupported,
			WatchSources:         CapabilitySupported,
			ClassifyChangedPath:  CapabilitySupported,
			FindSource:           CapabilitySupported,
			CompositeFingerprint: CapabilitySupported,
			MultiSessionSource:   CapabilityNotApplicable,
			PerSessionErrors:     CapabilityNotApplicable,
			ExcludedSessions:     CapabilityNotApplicable,
			ForceReplaceOnParse:  CapabilityNotApplicable,
		},
		Content: ContentCapabilities{
			FirstMessage: CapabilitySupported,
			Cwd:          CapabilitySupported,
			Thinking:     CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
			Model:        CapabilitySupported,
		},
	}
}
