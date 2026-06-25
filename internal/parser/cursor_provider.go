package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var _ Provider = (*cursorProvider)(nil)

type cursorProviderFactory struct {
	def AgentDef
}

func newCursorProviderFactory(def AgentDef) ProviderFactory {
	return cursorProviderFactory{def: cloneAgentDef(def)}
}

func (f cursorProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f cursorProviderFactory) Capabilities() Capabilities {
	return cursorProviderCapabilities()
}

func (f cursorProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	return &cursorProvider{
		ProviderBase: ProviderBase{
			Def:    cloneAgentDef(f.def),
			Caps:   cursorProviderCapabilities(),
			Config: cfg,
		},
		sources: newCursorSourceSet(cfg.Roots),
	}
}

type cursorProvider struct {
	ProviderBase
	sources cursorSourceSet
}

func (p *cursorProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	return p.sources.Discover(ctx)
}

func (p *cursorProvider) WatchPlan(ctx context.Context) (WatchPlan, error) {
	return p.sources.WatchPlan(ctx)
}

func (p *cursorProvider) SourcesForChangedPath(
	ctx context.Context,
	req ChangedPathRequest,
) ([]SourceRef, error) {
	return p.sources.SourcesForChangedPath(ctx, req)
}

func (p *cursorProvider) FindSource(
	ctx context.Context,
	req FindSourceRequest,
) (SourceRef, bool, error) {
	req = ProviderFindRequestWithRawSessionID(p.Def, req)
	return p.sources.FindSource(ctx, req)
}

func (p *cursorProvider) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	return p.sources.Fingerprint(ctx, source)
}

func (p *cursorProvider) Parse(
	ctx context.Context,
	req ParseRequest,
) (ParseOutcome, error) {
	if err := ctx.Err(); err != nil {
		return ParseOutcome{}, err
	}
	path, ok := p.sources.pathFromSource(req.Source)
	if !ok {
		return ParseOutcome{}, fmt.Errorf("cursor source path unavailable")
	}
	machine := firstNonEmptyJSONLString(req.Machine, p.Config.Machine)
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

type cursorSource struct {
	Root string
	Path string
}

type cursorSourceSet struct {
	roots []string
}

func newCursorSourceSet(roots []string) cursorSourceSet {
	return cursorSourceSet{roots: cleanJSONLRoots(roots)}
}

func (s cursorSourceSet) Discover(ctx context.Context) ([]SourceRef, error) {
	var sources []SourceRef
	seen := make(map[string]struct{})
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, path := range s.discoverTranscriptPaths(root) {
			source, ok := s.sourceRef(root, path)
			if !ok {
				continue
			}
			addJSONLSource(source, &sources, seen)
		}
	}
	sortJSONLSources(sources)
	return sources, nil
}

// discoverTranscriptPaths walks a Cursor projects root and returns the primary
// transcript file paths. All paths resolve within the canonical root,
// preventing symlink escapes. Symlinked project directory entries are rejected.
// Cursor uses two layouts: flat (agent-transcripts/<uuid>.{txt,jsonl}) and
// nested (agent-transcripts/<uuid>/<uuid>.{txt,jsonl}); when both .jsonl and
// .txt exist for the same stem, .jsonl is preferred.
func (s cursorSourceSet) discoverTranscriptPaths(projectsDir string) []string {
	if projectsDir == "" {
		return nil
	}

	// Canonicalize root once for containment checks.
	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Reject symlinked project directory entries.
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}

		transcriptsDir := filepath.Join(
			projectsDir, entry.Name(), "agent-transcripts",
		)

		// Verify the transcripts directory resolves within
		// the canonical root.
		resolvedDir, err := filepath.EvalSymlinks(transcriptsDir)
		if err != nil {
			continue
		}
		if !isContainedIn(resolvedDir, resolvedRoot) {
			continue
		}

		transcripts, err := os.ReadDir(transcriptsDir)
		if err != nil {
			continue
		}

		// Collect valid transcripts, deduping by basename
		// stem. When both .jsonl and .txt exist for the
		// same session, prefer .jsonl.
		//
		// Cursor uses two layouts:
		//   flat:   agent-transcripts/<uuid>.{txt,jsonl}
		//   nested: agent-transcripts/<uuid>/<uuid>.{txt,jsonl}
		seen := make(map[string]string) // stem -> path
		for _, sf := range transcripts {
			if !sf.IsDir() {
				// Flat layout: file directly in
				// agent-transcripts/.
				name := sf.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				fullPath := filepath.Join(transcriptsDir, name)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
				continue
			}

			// Nested layout: agent-transcripts/<uuid>/
			// containing <uuid>.{txt,jsonl}.
			subDir := filepath.Join(transcriptsDir, sf.Name())
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			dirName := sf.Name()
			for _, sub := range subEntries {
				if sub.IsDir() {
					continue
				}
				name := sub.Name()
				if !IsCursorTranscriptExt(name) {
					continue
				}
				// Only accept files whose stem matches
				// the parent directory name, e.g.
				// <uuid>/<uuid>.jsonl.
				stem := strings.TrimSuffix(name, filepath.Ext(name))
				if stem != dirName {
					continue
				}
				fullPath := filepath.Join(subDir, name)
				if !IsRegularFile(fullPath) {
					continue
				}
				cursorAddSeen(seen, name, fullPath)
			}
		}
		for _, path := range seen {
			paths = append(paths, path)
		}
	}
	return paths
}

// cursorAddSeen inserts a transcript path into the seen map, preferring .jsonl
// over .txt when both exist for the same stem.
func cursorAddSeen(seen map[string]string, name, fullPath string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	if prev, ok := seen[stem]; ok {
		if strings.HasSuffix(prev, ".txt") &&
			strings.HasSuffix(name, ".jsonl") {
			seen[stem] = fullPath
		}
		return
	}
	seen[stem] = fullPath
}

func (s cursorSourceSet) WatchPlan(context.Context) (WatchPlan, error) {
	roots := make([]WatchRoot, 0, len(s.roots))
	for _, root := range s.roots {
		roots = append(roots, WatchRoot{
			Path:         root,
			Recursive:    true,
			IncludeGlobs: []string{"*.jsonl", "*.txt"},
			DebounceKey:  string(AgentCursor) + ":transcripts:" + root,
		})
	}
	return WatchPlan{Roots: roots}, nil
}

func (s cursorSourceSet) SourcesForChangedPath(
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

func (s cursorSourceSet) FindSource(
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
		path := cursorFindSourceFile(root, req.RawSessionID)
		if path == "" {
			continue
		}
		if source, ok := s.sourceRef(root, path); ok {
			return source, true, nil
		}
	}
	return SourceRef{}, false, nil
}

// cursorFindSourceFile finds a Cursor transcript file by session UUID across a
// projects root, preferring .jsonl over .txt. Returns "" if no matching file
// resolves within the canonical root.
func cursorFindSourceFile(projectsDir, sessionID string) string {
	if projectsDir == "" || !IsValidSessionID(sessionID) {
		return ""
	}

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}

	resolvedRoot, err := filepath.EvalSymlinks(projectsDir)
	if err != nil {
		return ""
	}

	for _, ext := range []string{".jsonl", ".txt"} {
		target := sessionID + ext
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// Nested layout first (matches discovery
			// precedence), then flat layout.
			candidates := []string{
				filepath.Join(
					projectsDir, entry.Name(),
					"agent-transcripts", sessionID, target,
				),
				filepath.Join(
					projectsDir, entry.Name(),
					"agent-transcripts", target,
				),
			}
			for _, candidate := range candidates {
				if !IsRegularFile(candidate) {
					continue
				}
				resolved, err := filepath.EvalSymlinks(candidate)
				if err != nil {
					continue
				}
				rel, err := filepath.Rel(resolvedRoot, resolved)
				sep := string(filepath.Separator)
				if err != nil || rel == ".." ||
					strings.HasPrefix(rel, ".."+sep) {
					continue
				}
				return candidate
			}
		}
	}
	return ""
}

func (s cursorSourceSet) Fingerprint(
	ctx context.Context,
	source SourceRef,
) (SourceFingerprint, error) {
	if err := ctx.Err(); err != nil {
		return SourceFingerprint{}, err
	}
	path, ok := s.pathFromSource(source)
	if !ok {
		return SourceFingerprint{}, fmt.Errorf("cursor source path unavailable")
	}
	info, err := os.Stat(path)
	if err != nil {
		return SourceFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return SourceFingerprint{}, fmt.Errorf("stat %s: source is a directory", path)
	}
	hash := ""
	if info.Size() <= maxCursorTranscriptSize {
		hash, err = hashJSONLSourceFile(path)
		if err != nil {
			return SourceFingerprint{}, err
		}
	}
	return SourceFingerprint{
		Key:     firstNonEmptyJSONLString(source.FingerprintKey, source.Key, path),
		Size:    info.Size(),
		MTimeNS: info.ModTime().UnixNano(),
		Hash:    hash,
	}, nil
}

func (s cursorSourceSet) pathFromSource(source SourceRef) (string, bool) {
	switch src := source.Opaque.(type) {
	case cursorSource:
		return src.Path, src.Path != ""
	case *cursorSource:
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
			src := ref.Opaque.(cursorSource)
			return src.Path, true
		}
	}
	return "", false
}

func (s cursorSourceSet) sourceForPath(path string) (SourceRef, bool) {
	for _, root := range s.roots {
		if source, ok := s.sourceForPathInRoot(root, path); ok {
			return source, true
		}
	}
	return SourceRef{}, false
}

func (s cursorSourceSet) sourceForPathInRoot(
	root string,
	path string,
) (SourceRef, bool) {
	rawID, ok := cursorRawSessionIDFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	projectDir, ok := cursorProjectDirFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	selected := cursorFindSourceFileInProject(root, projectDir, rawID)
	if selected == "" {
		return SourceRef{}, false
	}
	return s.sourceRef(root, selected)
}

func (s cursorSourceSet) sourceRef(root, path string) (SourceRef, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !IsRegularFile(path) {
		return SourceRef{}, false
	}
	rawID, ok := cursorRawSessionIDFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	projectDir, ok := cursorProjectDirFromPath(root, path)
	if !ok {
		return SourceRef{}, false
	}
	selected := cursorFindSourceFileInProject(root, projectDir, rawID)
	if selected == "" || !samePath(selected, path) {
		return SourceRef{}, false
	}
	project := DecodeCursorProjectDir(projectDir)
	if project == "" {
		project = "unknown"
	}
	return SourceRef{
		Provider:       AgentCursor,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
		ProjectHint:    project,
		Opaque: cursorSource{
			Root: root,
			Path: path,
		},
	}, true
}

func (s cursorSourceSet) hasRoot(root string) bool {
	for _, configured := range s.roots {
		if samePath(root, configured) {
			return true
		}
	}
	return false
}

func cursorFindSourceFileInProject(root, projectDir, rawID string) string {
	if root == "" || projectDir == "" || !IsValidSessionID(rawID) {
		return ""
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	transcriptsDir := filepath.Join(root, projectDir, "agent-transcripts")
	for _, ext := range []string{".jsonl", ".txt"} {
		target := rawID + ext
		candidates := []string{
			filepath.Join(transcriptsDir, rawID, target),
			filepath.Join(transcriptsDir, target),
		}
		for _, candidate := range candidates {
			if !IsRegularFile(candidate) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil || !isContainedIn(resolved, resolvedRoot) {
				continue
			}
			return candidate
		}
	}
	return ""
}

func cursorRawSessionIDFromPath(root, path string) (string, bool) {
	rel, ok := cursorRelPath(root, path)
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	switch len(parts) {
	case 3:
		return strings.TrimSuffix(parts[2], filepath.Ext(parts[2])), true
	case 4:
		return parts[2], true
	default:
		return "", false
	}
}

func cursorProjectDirFromPath(root, path string) (string, bool) {
	rel, ok := cursorRelPath(root, path)
	if !ok {
		return "", false
	}
	return ParseCursorTranscriptRelPath(rel)
}

func cursorRelPath(root, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return "", false
	}
	if _, ok := ParseCursorTranscriptRelPath(rel); !ok {
		return "", false
	}
	return rel, true
}

func cursorProviderCapabilities() Capabilities {
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
			Thinking:     CapabilitySupported,
			ToolCalls:    CapabilitySupported,
			ToolResults:  CapabilitySupported,
		},
	}
}
